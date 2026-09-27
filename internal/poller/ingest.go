package poller

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/sorotrail/sorobeacon/internal/broadcast"
	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// IngestStore is the slice of persistence the Ingestor needs. It is a subset
// of Store, so the same store backs both the live poller and the backfill job.
type IngestStore interface {
	ListRules(ctx context.Context, monitorID int64, enabledOnly bool) ([]store.Rule, error)
	CreateAlert(ctx context.Context, a *store.Alert) (store.AlertOutcome, error)
}

// Ingestor evaluates decoded events against monitors' rules and records the
// resulting alerts. The live poller and the backfill job share one
// implementation so a historical replay and live ingestion apply identical
// dedup, cooldown and frequency-window semantics, and the store stays the
// single source of truth for both gates.
type Ingestor struct {
	store    IngestStore
	registry *rules.Registry
	dispatch Dispatcher
	log      *slog.Logger
	// metrics is optional instrumentation; nil-safe, see internal/metrics.
	metrics *metrics.Metrics
	// live is the optional SSE fan-out. When set, every alert this Ingestor
	// creates for a live event is published to it; nil (the NewIngestor
	// default) disables live alerts entirely.
	live *broadcast.Broadcaster
}

// NewIngestor wires an Ingestor. d receives every alert unless the caller
// disables delivery (HandleOptions.Deliver).
func NewIngestor(st IngestStore, reg *rules.Registry, d Dispatcher, log *slog.Logger) *Ingestor {
	return &Ingestor{store: st, registry: reg, dispatch: d, log: log}
}

// WithMetrics attaches Prometheus instrumentation to alert evaluation.
func (in *Ingestor) WithMetrics(m *metrics.Metrics) *Ingestor {
	in.metrics = m
	return in
}

// WithPublisher attaches the live fan-out that serves the SSE endpoint. The
// poller sets it so an alert is visible on a connected dashboard the moment it
// is created, with no database round-trip; a backfill leaves it nil.
func (in *Ingestor) WithPublisher(b *broadcast.Broadcaster) *Ingestor {
	in.live = b
	return in
}

// HandleOptions controls how one event's matches are recorded.
type HandleOptions struct {
	// Backfilled marks alerts created for a historical event.
	Backfilled bool
	// Deliver hands created alerts to the dispatcher. The live poller always
	// delivers; a backfill defaults it off so replaying months of history
	// cannot page anyone.
	Deliver bool
}

// HandleResult counts what one event produced.
type HandleResult struct {
	// Matched is the number of rules that matched the event, whether or not
	// each match produced a new alert (dedup and cooldown can absorb it).
	Matched int
	// Alerts is the number of new alert rows written.
	Alerts int
	// Dispatched is the number of alerts handed to the dispatcher.
	Dispatched int
}

// Handle runs every enabled rule of every given monitor against the event.
// Events arrive already decoded from the source; per-event failures are
// logged, not fatal: one bad event must not stall ingestion.
func (in *Ingestor) Handle(ctx context.Context, decoded *stellar.DecodedEvent, monitors []store.Monitor, opts HandleOptions) HandleResult {
	var res HandleResult
	for _, m := range monitors {
		ruleList, err := in.store.ListRules(ctx, m.ID, true)
		if err != nil {
			in.log.Error("list rules", "monitor_id", m.ID, "err", err)
			continue
		}
		for _, rule := range ruleList {
			// The rule id rides in the context so a stateful evaluator (the
			// frequency rule) can key its per-rule state.
			ruleCtx := rules.WithRuleID(ctx, rule.ID)
			matched, err := in.registry.Evaluate(ruleCtx, rule.Type, decoded, rule.Params)
			if err != nil {
				in.log.Warn("rule evaluation failed", "rule_id", rule.ID, "event_id", decoded.ID, "err", err)
				continue
			}
			if !matched {
				continue
			}
			res.Matched++
			in.metrics.RecordAlert()
			// An evaluator may override the dedup key (the frequency rule
			// stores every crossing in one episode under the window start).
			eventID := decoded.ID
			if id := in.registry.AlertEventID(ruleCtx, rule.Type, decoded, rule.Params); id != "" {
				eventID = id
			}
			created, dispatched := in.fireAlert(ctx, m, rule, decoded, eventID, opts)
			if created {
				res.Alerts++
			}
			if dispatched {
				res.Dispatched++
			}
		}
	}
	return res
}

// fireAlert persists a deduped, cooldown-gated alert and, when delivery is
// enabled, hands it to the dispatcher. The store owns both gates so they hold
// across poller instances and restarts; this method only reports the outcome.
func (in *Ingestor) fireAlert(ctx context.Context, m store.Monitor, rule store.Rule, ev *stellar.DecodedEvent, eventID string, opts HandleOptions) (created, dispatched bool) {
	body := map[string]any{
		"contract_id":      ev.ContractID,
		"event_name":       ev.EventName(),
		"ledger":           ev.Ledger,
		"ledger_closed_at": ev.LedgerClosedAt,
		"tx_hash":          ev.TxHash,
		"topics":           ev.Topics,
		"value":            ev.Value,
	}
	// Named fields are only present when the contract's spec was available;
	// without one the payload is byte-for-byte what it has always been.
	if ev.Fields != nil {
		body["fields"] = ev.Fields
	}
	payload, err := json.Marshal(body)
	if err != nil {
		in.log.Error("marshal alert payload", "event_id", ev.ID, "err", err)
		return false, false
	}

	alert := &store.Alert{
		MonitorID:      m.ID,
		RuleID:         rule.ID,
		EventID:        eventID,
		Payload:        payload,
		Ledger:         ev.Ledger,
		LedgerClosedAt: ev.LedgerClosedAt,
		Cooldown:       ruleCooldown(rule),
		Backfilled:     opts.Backfilled,
		Severity:       rule.Severity,
	}
	outcome, err := in.store.CreateAlert(ctx, alert)
	if err != nil {
		in.log.Error("create alert", "rule_id", rule.ID, "event_id", ev.ID, "err", err)
		return false, false
	}
	switch outcome {
	case store.AlertDuplicate:
		return false, false // dedup: this rule already fired for this event
	case store.AlertSuppressed:
		// Counted by the store; logged so an operator can see the rule is
		// firing far more often than it is alerting.
		in.log.Info("alert suppressed by cooldown",
			"rule_id", rule.ID, "event_id", ev.ID, "cooldown", alert.Cooldown)
		return false, false
	}
	logAttrs := []any{"alert_id", alert.ID, "monitor", m.Name, "rule_id", rule.ID, "event_id", ev.ID}
	if alert.SuppressedSinceLast > 0 {
		logAttrs = append(logAttrs, "suppressed_since_last", alert.SuppressedSinceLast)
	}
	// A backfill can create thousands of alerts; log each at debug so a
	// replay does not flood the log, and report the aggregate when it ends.
	if opts.Backfilled {
		in.log.Debug("backfilled alert created", logAttrs...)
	} else {
		in.log.Info("alert created", logAttrs...)
	}

	// Publish before dispatching: a live dashboard should see the alert the
	// moment it exists, not after the (possibly retried) channel fan-out.
	// Only created alerts reach here — duplicates and cooldown-suppressed
	// matches returned above — so the stream mirrors the alert table exactly.
	// A backfilled alert is historical, so it never reaches the live stream.
	if in.live != nil && !opts.Backfilled {
		in.live.Publish(broadcast.Alert{
			ID:          alert.ID,
			MonitorID:   m.ID,
			MonitorName: m.Name,
			RuleID:      rule.ID,
			EventID:     eventID,
			Payload:     alert.Payload,
			CreatedAt:   alert.CreatedAt,
		})
	}

	if !opts.Deliver {
		return true, false
	}
	in.dispatch.Dispatch(ctx, notify.Alert{
		ID:          alert.ID,
		MonitorID:   m.ID,
		MonitorName: m.Name,
		RuleID:      rule.ID,
		RuleType:    rule.Type,
		EventID:     eventID,
		ContractID:  ev.ContractID,
		EventName:   ev.EventName(),
		Ledger:      ev.Ledger,
		TxHash:      ev.TxHash,
		Payload:     alert.Payload,
		CreatedAt:   alert.CreatedAt,
		Severity:    string(alert.Severity),
	})
	return true, true
}

// ruleCooldown reads a rule's optional cooldown. It is validated when the rule
// is created, so a value that no longer parses is treated as "no cooldown"
// rather than dropping matches.
func ruleCooldown(rule store.Rule) time.Duration {
	d, err := rules.ParseCooldown(rule.Params)
	if err != nil {
		return 0
	}
	return d
}
