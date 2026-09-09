// Package poller ingests Soroban contract events from a Stellar RPC node
// and feeds them through the rules engine, creating and dispatching alerts.
package poller

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
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
	source   EventSource
	store    Store
	registry *rules.Registry
	dispatch Dispatcher
	interval time.Duration
	log      *slog.Logger
	// metrics is optional instrumentation; nil-safe, see internal/metrics.
	metrics *metrics.Metrics
	// scanned/matched accumulate per-cycle counts for metrics.
	scanned int
	matched int
}

// New wires a Poller. src is where events come from: NewRPCSource for a
// Stellar RPC node, or the SoroTrail source for upstream mode.
func New(src EventSource, st Store, reg *rules.Registry, d Dispatcher, interval time.Duration, log *slog.Logger) *Poller {
	return &Poller{
		source:   src,
		store:    st,
		registry: reg,
		dispatch: d,
		interval: interval,
		log:      log,
	}
}

// WithMetrics attaches Prometheus instrumentation to the poll loop.
func (p *Poller) WithMetrics(m *metrics.Metrics) *Poller {
	p.metrics = m
	return p
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

		p.scanned, p.matched = 0, 0
		start := time.Now()
		err := p.Poll(ctx)
		p.metrics.RecordPoll(err == nil, time.Since(start))
		p.metrics.RecordEvents(p.scanned, p.matched)
		if err != nil {
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
		// Cold start against an RPC source: the node only retains ~1-7
		// days of events, so begin at the current tip. Upstream sources
		// (SoroTrail) hold durable history and answer the same query.
		latest, err := p.source.LatestLedger(ctx)
		if err != nil {
			return err
		}
		startLedger = latest
		p.log.Info("cold start", "start_ledger", startLedger)
	}

	// Page the source until it reports no more events for the cycle. The
	// cursor is opaque; batching (the RPC caps filters per request) is the
	// source's concern, encoded in its cursors.
	checkpoint := uint32(0) // min latestLedger across pages
	tip := uint32(0)        // max latestLedger across pages
	cursor := ""
	for {
		page, err := p.source.FetchEvents(ctx, startLedger, contracts, cursor, stellar.DefaultEventsLimit)
		if err != nil {
			return err
		}
		if page.LatestLedger > 0 && (checkpoint == 0 || page.LatestLedger < checkpoint) {
			checkpoint = page.LatestLedger
		}
		if page.LatestLedger > tip {
			tip = page.LatestLedger
		}
		p.scanned += len(page.Events)
		for _, ev := range page.Events {
			p.handleEvent(ctx, ev, byContract)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	// Lag: how far the checkpoint we reached trails the node's own tip.
	// Grows when a batch's page-through takes longer than ledger cadence.
	p.metrics.SetPollLag(int64(tip) - int64(checkpoint))
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
// handleEvent runs every enabled rule of every monitor watching the
// event's contract. Events arrive already decoded from the source;
// per-event failures are logged, not fatal: one bad event must not stall
// ingestion.
func (p *Poller) handleEvent(ctx context.Context, decoded *stellar.DecodedEvent, byContract map[string][]store.Monitor) {
	monitors, watched := byContract[decoded.ContractID]
	if !watched {
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
				p.log.Warn("rule evaluation failed", "rule_id", rule.ID, "event_id", decoded.ID, "err", err)
				continue
			}
			if matched {
				p.matched++
				p.metrics.RecordAlert()
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
