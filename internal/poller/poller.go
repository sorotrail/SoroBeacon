// Package poller ingests Soroban contract events from a Stellar RPC node
// and feeds them through the rules engine, creating and dispatching alerts.
package poller

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// Position is a race-free snapshot of how far the poller has got relative
// to the chain. A zero LastSuccessfulPoll means no successful poll has
// completed yet — callers must not treat the ledger fields as "in sync".
type Position struct {
	LastProcessedLedger uint32
	LatestChainLedger   uint32
	LastSuccessfulPoll  time.Time
}

// Ready reports whether a successful poll has completed.
func (p Position) Ready() bool { return !p.LastSuccessfulPoll.IsZero() }

// Lag is latest known chain ledger minus last processed ledger.
func (p Position) Lag() int64 {
	return int64(p.LatestChainLedger) - int64(p.LastProcessedLedger)
}

// Store is the slice of the store the poller needs.
type Store interface {
	ListMonitors(ctx context.Context, enabledOnly bool) ([]store.Monitor, error)
	ListRules(ctx context.Context, monitorID int64, enabledOnly bool) ([]store.Rule, error)
	CreateAlert(ctx context.Context, a *store.Alert) (bool, error)
	GetIngestState(ctx context.Context) (store.IngestState, error)
	SetIngestState(ctx context.Context, s store.IngestState) error
	// The absence-of-event rules need a clock that outlives the process: see
	// SweepAbsence.
	ListAbsenceState(ctx context.Context) ([]store.AbsenceState, error)
	RecordAbsenceSeen(ctx context.Context, ruleID int64, eventName string, at time.Time) error
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
	// now is the clock the absence rules are measured against, both when an
	// event re-arms one and when the sweep checks for silence. It is a field
	// rather than a direct time.Now() call so a test can step time instead of
	// sleeping through a real window; production leaves it at New's default.
	now func() time.Time
	// scanned/matched accumulate per-cycle counts for metrics.
	scanned int
	matched int
	// pos is the last successful poll snapshot, stored as Position.
	// atomic.Value so HTTP handlers can read it without a mutex.
	pos atomic.Value
}

// Position returns the last successful poll snapshot. Safe to call from
// another goroutine (the HTTP health handler). Before the first successful
// poll the returned Position is the zero value and Ready is false.
func (p *Poller) Position() Position {
	if v := p.pos.Load(); v != nil {
		return v.(Position)
	}
	return Position{}
}

func (p *Poller) recordPosition(processed, latest uint32, at time.Time) {
	p.pos.Store(Position{
		LastProcessedLedger: processed,
		LatestChainLedger:   latest,
		LastSuccessfulPoll:  at.UTC(),
	})
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
		now:      time.Now,
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

		// The absence sweep rides the same tick, but only after a cycle that
		// actually succeeded: a failed cycle means "we could not look", not
		// "nothing happened", and alerting on silence during an outage of the
		// very thing we watch through would be a false alarm about the one
		// situation we cannot currently observe.
		if err := p.SweepAbsence(ctx, p.now()); err != nil {
			p.log.Error("absence sweep failed", "err", err)
		}
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
	processed := checkpoint
	if processed == 0 {
		processed = state.LastLedger
	}
	p.recordPosition(processed, tip, time.Now())
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
			// An absence rule is re-armed by a matching event and fired by
			// the sweep; evaluating it here would be asking the wrong
			// question (and, for a rule with no event to see, always "no").
			if abs, ok := p.registry.Absence(rule.Type); ok {
				p.observeAbsence(ctx, rule, abs, decoded)
				continue
			}
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
		MonitorID:      m.ID,
		RuleID:         rule.ID,
		EventID:        ev.ID,
		Payload:        payload,
		LedgerClosedAt: ev.LedgerClosedAt,
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

// --- absence-of-event rules ---

// stateKey identifies one absence rule's clock: the rule plus the event
// pattern it waits for. Keying on the pattern as well as the rule means
// editing a rule from "heartbeat" to "ping" starts a fresh clock instead of
// measuring ping's silence from heartbeat's last appearance.
type stateKey struct {
	ruleID    int64
	eventName string
}

// SweepAbsence fires the absence rules whose window has elapsed. It is the
// other half of the rules engine: where handleEvent reacts to events that
// arrived, this reacts to the ones that did not, which is why it runs on a
// timer instead of on arrival.
//
// now is passed in (rather than read here) so the caller's clock and the
// rearm clock in observeAbsence are the same one; Run passes the poller's.
// The window id comes from the stored last-seen instant, so every sweep
// inside one silent window produces the same synthetic event id and the
// existing (rule_id, event_id) unique index turns the repeat into a no-op.
func (p *Poller) SweepAbsence(ctx context.Context, now time.Time) error {
	monitors, err := p.store.ListMonitors(ctx, true)
	if err != nil {
		return err
	}
	if len(monitors) == 0 {
		return nil
	}

	// One read for every clock: the table holds at most one row per absence
	// rule, and looking each rule up individually would mean a query per rule
	// on every tick.
	states, err := p.store.ListAbsenceState(ctx)
	if err != nil {
		return err
	}
	stored := make(map[stateKey]time.Time, len(states))
	for _, s := range states {
		stored[stateKey{ruleID: s.RuleID, eventName: s.EventName}] = s.LastSeen
	}

	for _, m := range monitors {
		ruleList, err := p.store.ListRules(ctx, m.ID, true)
		if err != nil {
			p.log.Error("absence sweep: list rules", "monitor_id", m.ID, "err", err)
			continue
		}
		for _, rule := range ruleList {
			abs, ok := p.registry.Absence(rule.Type)
			if !ok {
				continue // an event-driven rule: not this sweep's business
			}
			spec, err := abs.Spec(rule.Params)
			if err != nil {
				// Stored params predate this rule's validation, or were
				// written straight into the database. Skip rather than guess: a
				// window we cannot read is one we cannot measure.
				p.log.Warn("absence sweep: unusable params", "rule_id", rule.ID, "err", err)
				continue
			}

			key := stateKey{ruleID: rule.ID, eventName: spec.EventName}
			lastSeen, armed := stored[key]
			if !armed {
				// Never observed this rule before: arm it now instead of firing.
				// Without this a brand-new rule would either fire immediately
				// (nothing has been seen yet, so any window "has elapsed") or
				// never fire at all; either way the operator would get an alert
				// about silence that started before the rule existed. The
				// baseline is persisted, so the next restart resumes from it.
				if err := p.store.RecordAbsenceSeen(ctx, rule.ID, spec.EventName, now); err != nil {
					p.log.Error("absence sweep: arm rule", "rule_id", rule.ID, "err", err)
				}
				continue
			}

			silent := now.Sub(lastSeen)
			if silent < spec.Window {
				continue
			}
			p.fireAbsence(ctx, m, rule, spec, lastSeen, silent)
		}
	}
	return nil
}

// observeAbsence advances a rule's clock when the event it is waiting for
// arrives. The clock is wall-clock time, not the event's ledger close time:
// the sweep compares it with wall clock, and upstream mode (SOURCE_MODE=
// sorotrail) never populates LedgerClosedAt, so the measurement must not
// depend on which event source is wired.
func (p *Poller) observeAbsence(ctx context.Context, rule store.Rule, abs rules.AbsenceEvaluator, ev *stellar.DecodedEvent) {
	spec, err := abs.Spec(rule.Params)
	if err != nil {
		p.log.Warn("absence rule has unusable params", "rule_id", rule.ID, "err", err)
		return
	}
	if !abs.Matches(ev, spec) {
		return
	}
	if err := p.store.RecordAbsenceSeen(ctx, rule.ID, spec.EventName, p.now()); err != nil {
		p.log.Error("record absence seen", "rule_id", rule.ID, "event_name", spec.EventName, "err", err)
	}
}

// fireAbsence persists one absence alert and hands it to the dispatcher.
//
// LedgerClosedAt is deliberately left zero: that field means "the ledger close
// time of the event that matched", and there is no such event here. Leaving it
// zero keeps monitors.last_matched_at honest (the dashboard reads it as chain
// time), instead of inventing a wall-clock match time for a monitor that, by
// definition, matched nothing.
func (p *Poller) fireAbsence(ctx context.Context, m store.Monitor, rule store.Rule, spec rules.AbsenceSpec, lastSeen time.Time, silent time.Duration) {
	payload, err := json.Marshal(map[string]any{
		// Every contract the monitor watches: the rule is monitor-scoped, so
		// silence is "none of them emitted this", not "one contract went
		// quiet".
		"contract_ids":       m.ContractIDs,
		"event_name":         spec.EventName,
		"window_seconds":     int64(spec.Window.Seconds()),
		"last_seen_at":       lastSeen.UTC().Format(time.RFC3339),
		"silent_for_seconds": int64(silent.Seconds()),
	})
	if err != nil {
		p.log.Error("marshal absence payload", "rule_id", rule.ID, "err", err)
		return
	}

	alert := &store.Alert{
		MonitorID: m.ID,
		RuleID:    rule.ID,
		EventID:   AbsenceEventID(lastSeen),
		Payload:   payload,
	}
	created, err := p.store.CreateAlert(ctx, alert)
	if err != nil {
		p.log.Error("create alert", "rule_id", rule.ID, "event_id", alert.EventID, "err", err)
		return
	}
	if !created {
		return // dedup: this silent window has already alerted
	}
	p.metrics.RecordAlert()
	p.log.Info("absence alert created",
		"alert_id", alert.ID, "monitor", m.Name, "rule_id", rule.ID,
		"event_name", spec.EventName, "silent_for", silent)

	p.dispatch.Dispatch(ctx, notify.Alert{
		ID:          alert.ID,
		MonitorID:   m.ID,
		MonitorName: m.Name,
		RuleID:      rule.ID,
		RuleType:    rule.Type,
		EventID:     alert.EventID,
		EventName:   spec.EventName,
		Silence:     silent,
		Payload:     payload,
		CreatedAt:   alert.CreatedAt,
	})
}

// AbsenceEventID is the synthetic event id for one silent window: derived from
// the instant silence started (the stored last-seen time), never from a TOID,
// because there is no event to take one from.
//
// Deriving it from the window rather than from the sweep means the alert's own
// dedup guard does the de-duplication: every sweep in the same window writes
// the same id and inserts nothing, and once the awaited event returns the next
// window starts from a new clock and so produces a new id — which is what
// re-arms the rule. It is also stable across a restart, since the clock it is
// built from lives in the database.
func AbsenceEventID(lastSeen time.Time) string {
	return "absence-" + strconv.FormatInt(lastSeen.UTC().UnixNano(), 10)
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
