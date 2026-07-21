// Package poller ingests Soroban contract events from a Stellar RPC node
// and feeds them through the rules engine, creating and dispatching alerts.
package poller

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/sorobeacon/sorobeacon/internal/notify"
	"github.com/sorobeacon/sorobeacon/internal/rules"
	"github.com/sorobeacon/sorobeacon/internal/stellar"
	"github.com/sorobeacon/sorobeacon/internal/store"
)

// Store is the slice of the store the poller needs.
type Store interface {
	ListMonitors(ctx context.Context, enabledOnly bool) ([]store.Monitor, error)
	ListRules(ctx context.Context, monitorID int64, enabledOnly bool) ([]store.Rule, error)
	CreateAlert(ctx context.Context, a *store.Alert) (bool, error)
	GetIngestState(ctx context.Context) (store.IngestState, error)
	SetIngestState(ctx context.Context, s store.IngestState) error
}

// Dispatcher receives every newly created alert. Implemented by
// notify.Dispatcher; mocked in tests.
type Dispatcher interface {
	Dispatch(ctx context.Context, a notify.Alert)
}

// Poller runs the ingest loop: getEvents from the last checkpoint, decode,
// match rules, create + dispatch alerts, advance the checkpoint.
type Poller struct {
	rpc      stellar.Client
	decoder  stellar.Decoder
	store    Store
	registry *rules.Registry
	dispatch Dispatcher
	interval time.Duration
	log      *slog.Logger
}

// New wires a Poller.
func New(rpc stellar.Client, dec stellar.Decoder, st Store, reg *rules.Registry, d Dispatcher, interval time.Duration, log *slog.Logger) *Poller {
	return &Poller{
		rpc:      rpc,
		decoder:  dec,
		store:    st,
		registry: reg,
		dispatch: d,
		interval: interval,
		log:      log,
	}
}

// Run polls until ctx is cancelled. RPC errors back off exponentially
// (capped at 10x the poll interval) instead of hammering the node.
func (p *Poller) Run(ctx context.Context) {
	p.log.Info("poller started", "interval", p.interval)
	delay := p.interval
	for {
		select {
		case <-ctx.Done():
			p.log.Info("poller stopped")
			return
		case <-time.After(delay):
		}

		if err := p.Poll(ctx); err != nil {
			if ctx.Err() != nil {
				continue
			}
			delay = min(delay*2, 10*p.interval)
			p.log.Error("poll failed", "err", err, "retry_in", delay)
			continue
		}
		delay = p.interval
	}
}

// Poll runs one ingest cycle. Exported so tests (and one-shot tools) can
// drive the poller without the timing loop.
func (p *Poller) Poll(ctx context.Context) error {
	monitors, err := p.store.ListMonitors(ctx, true)
	if err != nil {
		return err
	}

	// Map each contract to the monitors watching it; dedupe contracts.
	byContract := map[string][]store.Monitor{}
	var contracts []string
	for _, m := range monitors {
		for _, c := range m.ContractIDs {
			// A single malformed ID makes the RPC reject the whole request,
			// stalling ingestion for every monitor — skip, don't send.
			if !stellar.IsValidContractID(c) {
				p.log.Warn("skipping invalid contract id", "monitor_id", m.ID, "contract_id", c)
				continue
			}
			if _, seen := byContract[c]; !seen {
				contracts = append(contracts, c)
			}
			byContract[c] = append(byContract[c], m)
		}
	}
	if len(contracts) == 0 {
		return nil
	}

	state, err := p.store.GetIngestState(ctx)
	if err != nil {
		return err
	}
	startLedger := state.LastLedger + 1
	if state.LastLedger == 0 {
		// Cold start: the RPC only retains ~1-7 days of events, so begin at
		// the current tip rather than trying to backfill history.
		latest, err := p.rpc.GetLatestLedger(ctx)
		if err != nil {
			return err
		}
		startLedger = latest.Sequence
		p.log.Info("cold start", "start_ledger", startLedger)
	}

	// getEvents caps filters per request and contractIds per filter, so
	// batch: 5 contracts per filter, 5 filters per request.
	filters := buildFilters(contracts)
	checkpoint := uint32(0) // min latestLedger across request batches
	for i := 0; i < len(filters); i += stellar.MaxFiltersPerRequest {
		batch := filters[i:min(i+stellar.MaxFiltersPerRequest, len(filters))]
		latest, err := p.pollBatch(ctx, startLedger, batch, byContract)
		if err != nil {
			return err
		}
		if checkpoint == 0 || latest < checkpoint {
			checkpoint = latest
		}
	}

	if checkpoint > state.LastLedger {
		state.LastLedger = checkpoint
		state.LastCursor = ""
		if err := p.store.SetIngestState(ctx, state); err != nil {
			return err
		}
	}
	return nil
}

// pollBatch pages through getEvents for one set of filters, following the
// cursor until the stream is drained. Returns the node's latestLedger.
func (p *Poller) pollBatch(ctx context.Context, startLedger uint32, filters []stellar.EventFilter, byContract map[string][]store.Monitor) (uint32, error) {
	req := stellar.GetEventsRequest{
		StartLedger: startLedger,
		Filters:     filters,
		Pagination:  &stellar.Pagination{Limit: stellar.DefaultEventsLimit},
	}
	var latest uint32
	for {
		res, err := p.rpc.GetEvents(ctx, req)
		if err != nil {
			return 0, err
		}
		latest = res.LatestLedger

		for _, ev := range res.Events {
			p.handleEvent(ctx, ev, byContract)
		}

		if len(res.Events) < stellar.DefaultEventsLimit {
			return latest, nil
		}
		cursor := res.Cursor
		if cursor == "" && len(res.Events) > 0 {
			// Older RPC versions: page with the last event's token/id.
			last := res.Events[len(res.Events)-1]
			cursor = last.PagingToken
			if cursor == "" {
				cursor = last.ID
			}
		}
		if cursor == "" {
			return latest, nil
		}
		req.Pagination = &stellar.Pagination{Cursor: cursor, Limit: stellar.DefaultEventsLimit}
		req.StartLedger = 0
	}
}

// handleEvent decodes one event and runs every enabled rule of every
// monitor watching its contract. Per-event failures are logged, not fatal:
// one undecodable event must not stall ingestion.
func (p *Poller) handleEvent(ctx context.Context, ev stellar.Event, byContract map[string][]store.Monitor) {
	monitors, watched := byContract[ev.ContractID]
	if !watched {
		return
	}
	decoded, err := p.decoder.DecodeEvent(ev)
	if err != nil {
		p.log.Warn("decode event failed", "event_id", ev.ID, "contract_id", ev.ContractID, "err", err)
		return
	}

	for _, m := range monitors {
		ruleList, err := p.store.ListRules(ctx, m.ID, true)
		if err != nil {
			p.log.Error("list rules", "monitor_id", m.ID, "err", err)
			continue
		}
		for _, rule := range ruleList {
			matched, err := p.registry.Evaluate(ctx, rule.Type, decoded, rule.Params)
			if err != nil {
				p.log.Warn("rule evaluation failed", "rule_id", rule.ID, "event_id", ev.ID, "err", err)
				continue
			}
			if matched {
				p.fireAlert(ctx, m, rule, decoded)
			}
		}
	}
}

// fireAlert persists a deduped alert and hands it to the dispatcher.
func (p *Poller) fireAlert(ctx context.Context, m store.Monitor, rule store.Rule, ev *stellar.DecodedEvent) {
	payload, err := json.Marshal(map[string]any{
		"contract_id":      ev.ContractID,
		"event_name":       ev.EventName(),
		"ledger":           ev.Ledger,
		"ledger_closed_at": ev.LedgerClosedAt,
		"tx_hash":          ev.TxHash,
		"topics":           ev.Topics,
		"value":            ev.Value,
	})
	if err != nil {
		p.log.Error("marshal alert payload", "event_id", ev.ID, "err", err)
		return
	}

	alert := &store.Alert{
		MonitorID: m.ID,
		RuleID:    rule.ID,
		EventID:   ev.ID,
		Payload:   payload,
	}
	created, err := p.store.CreateAlert(ctx, alert)
	if err != nil {
		p.log.Error("create alert", "rule_id", rule.ID, "event_id", ev.ID, "err", err)
		return
	}
	if !created {
		return // dedup: this rule already fired for this event
	}
	p.log.Info("alert created",
		"alert_id", alert.ID, "monitor", m.Name, "rule_id", rule.ID, "event_id", ev.ID)

	p.dispatch.Dispatch(ctx, notify.Alert{
		ID:          alert.ID,
		MonitorID:   m.ID,
		MonitorName: m.Name,
		RuleID:      rule.ID,
		RuleType:    rule.Type,
		EventID:     ev.ID,
		ContractID:  ev.ContractID,
		EventName:   ev.EventName(),
		Ledger:      ev.Ledger,
		TxHash:      ev.TxHash,
		Payload:     payload,
		CreatedAt:   alert.CreatedAt,
	})
}

// buildFilters packs contract IDs into getEvents filters, respecting the
// per-filter contractIds cap.
func buildFilters(contracts []string) []stellar.EventFilter {
	var out []stellar.EventFilter
	for i := 0; i < len(contracts); i += stellar.MaxContractIDsPerFilter {
		end := min(i+stellar.MaxContractIDsPerFilter, len(contracts))
		out = append(out, stellar.EventFilter{
			Type:        "contract",
			ContractIDs: contracts[i:end],
		})
	}
	return out
}
