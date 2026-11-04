package poller

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/sorotrail/sorobeacon/internal/alerts"
	"github.com/sorotrail/sorobeacon/internal/broadcast"
	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/usage"
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
	live                *broadcast.Broadcaster
	enricher            *alerts.Enricher
	usageMeter          *usage.Meter
	slowRuleThreshold   time.Duration
	workspaceEvalBudget time.Duration
}

// NewIngestor wires an Ingestor. d receives every alert unless the caller
// disables delivery (HandleOptions.Deliver).
func NewIngestor(st IngestStore, reg *rules.Registry, d Dispatcher, log *slog.Logger) *Ingestor {
	in := &Ingestor{
		store:               st,
		registry:            reg,
		dispatch:            d,
		log:                 log,
		workspaceEvalBudget: rules.DefaultWorkspaceEvalBudget,
	}
	in.bindRegistryHooks()
	return in
}

// WithMetrics attaches Prometheus instrumentation to alert evaluation.
func (in *Ingestor) WithMetrics(m *metrics.Metrics) *Ingestor {
	in.metrics = m
	return in
}

// WithUsageMeter attaches per-workspace usage metering to alert evaluation.
func (in *Ingestor) WithUsageMeter(um *usage.Meter) *Ingestor {
	in.usageMeter = um
	return in
}

// WithPublisher attaches the live fan-out that serves the SSE endpoint. The
// poller sets it so an alert is visible on a connected dashboard the moment it
// is created, with no database round-trip; a backfill leaves it nil.
func (in *Ingestor) WithPublisher(b *broadcast.Broadcaster) *Ingestor {
	in.live = b
	return in
}

func (in *Ingestor) WithEnricher(e *alerts.Enricher) *Ingestor {
	in.enricher = e
	return in
}

// WithRuleTimeout configures the per-rule evaluation timeout on the registry.
func (in *Ingestor) WithRuleTimeout(d time.Duration) *Ingestor {
	if in.registry != nil {
		in.registry.WithEvalTimeout(d)
	}
	return in
}

// WithRuleQuarantineThreshold configures the consecutive failure count that
// quarantines a rule.
func (in *Ingestor) WithRuleQuarantineThreshold(threshold int) *Ingestor {
	if in.registry != nil {
		in.registry.WithQuarantineThreshold(threshold)
	}
	return in
}

// WithWorkspaceEvalBudget bounds how much total rule-evaluation time a single
// workspace may consume on one event before its remaining rules are skipped
// for that event.
func (in *Ingestor) WithWorkspaceEvalBudget(d time.Duration) *Ingestor {
	in.workspaceEvalBudget = d
	return in
}

// ReenableRule clears any quarantine and failure strikes for ruleID and resets
// its stateful evaluator state.
func (in *Ingestor) ReenableRule(ctx context.Context, ruleID int64) bool {
	if in == nil || in.registry == nil {
		return false
	}
	return in.registry.ReenableRule(ctx, ruleID)
}

// QuarantinedRules returns a snapshot of all currently quarantined rules.
func (in *Ingestor) QuarantinedRules() []rules.RuleQuarantineState {
	if in == nil || in.registry == nil {
		return nil
	}
	return in.registry.QuarantinedRules()
}

func (in *Ingestor) bindRegistryHooks() {
	if in == nil || in.registry == nil {
		return
	}
	in.registry.WithQuarantineHook(func(ctx context.Context, state rules.RuleQuarantineState) {
		handleQuarantineTransition(ctx, in.log, in.metrics, in.store, in.registry, state)
	})
	in.registry.WithEvalObserver(func(_ context.Context, obs rules.EvalObservation) {
		handleEvalObservation(in.log, in.metrics, in.slowRuleThreshold, obs)
	})
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
	orderedMonitors := orderMonitorsByWorkspaceFairness(monitors)
	workspaceElapsed := make(map[string]time.Duration)
	for _, m := range orderedMonitors {
		ruleList, err := in.store.ListRules(ctx, m.ID, true)
		if err != nil {
			in.log.Error("list rules", "monitor_id", m.ID, "err", err)
			continue
		}
		var evals int64
		for _, rule := range ruleList {
			if in.workspaceEvalBudget > 0 && workspaceElapsed[m.WorkspaceID] >= in.workspaceEvalBudget {
				in.metrics.RecordWorkspaceRuleBudgetExhausted(m.WorkspaceID)
				if in.log != nil {
					in.log.Warn("workspace rule evaluation budget exceeded; skipping rule for event",
						"workspace_id", m.WorkspaceID,
						"monitor_id", m.ID,
						"rule_id", rule.ID,
						"rule_type", rule.Type,
						"event_id", decoded.ID,
						"budget", in.workspaceEvalBudget,
						"elapsed", workspaceElapsed[m.WorkspaceID],
					)
				}
				continue
			}
			// The rule id rides in the context so a stateful evaluator (the
			// frequency rule) can key its per-rule state.
			ruleCtx := rules.WithRuleVersion(rules.WithRuleID(ctx, rule.ID), rule.Version)
			start := time.Now()
			matched, err := in.registry.Evaluate(ruleCtx, rule.Type, decoded, rule.Params)
			duration := time.Since(start)
			workspaceElapsed[m.WorkspaceID] += duration
			evals++
			if in.metrics != nil {
				in.metrics.RecordRuleEval(rule.Type, matched, err, duration)
			}
			if err != nil {
				if rules.IsRuleIsolatedFailure(err) {
					logIsolatedRuleFailure(in.log, m, rule, decoded.ID, err)
					continue
				}
				if in.log != nil {
					in.log.Warn("rule evaluation failed due to storage error",
						"rule_id", rule.ID, "monitor_id", m.ID, "event_id", decoded.ID, "err", err)
				}
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
		if evals > 0 {
			in.recordWorkspaceRuleEvaluations(ctx, m.WorkspaceID, evals)
		}
	}
	return res
}

// orderMonitorsByWorkspaceFairness interleaves monitors round-robin across
// workspaces while preserving each workspace's relative monitor order, so one
// workspace with many monitors or slow rules cannot monopolize per-event rule
// evaluation ahead of other workspaces watching the same contract.
func orderMonitorsByWorkspaceFairness(monitors []store.Monitor) []store.Monitor {
	if len(monitors) <= 1 {
		return monitors
	}
	var workspaces []string
	byWorkspace := make(map[string][]store.Monitor)
	for _, m := range monitors {
		ws := m.WorkspaceID
		if _, seen := byWorkspace[ws]; !seen {
			workspaces = append(workspaces, ws)
		}
		byWorkspace[ws] = append(byWorkspace[ws], m)
	}
	if len(workspaces) <= 1 {
		return monitors
	}
	out := make([]store.Monitor, 0, len(monitors))
	for idx := 0; len(out) < len(monitors); idx++ {
		for _, ws := range workspaces {
			list := byWorkspace[ws]
			if idx < len(list) {
				out = append(out, list[idx])
			}
		}
	}
	return out
}

func handleQuarantineTransition(
	ctx context.Context,
	log *slog.Logger,
	m *metrics.Metrics,
	st any,
	reg *rules.Registry,
	state rules.RuleQuarantineState,
) {
	totalQuarantined := 0
	if reg != nil {
		totalQuarantined = reg.QuarantinedCount()
	}
	m.RecordRuleQuarantined(state.RuleID, state.RuleType, string(state.Reason), state.Quarantined, totalQuarantined)
	if state.Quarantined {
		if log != nil {
			log.Error("rule quarantined after repeated evaluation failures",
				"rule_id", state.RuleID,
				"rule_type", state.RuleType,
				"rule_version", state.RuleVersion,
				"reason", string(state.Reason),
				"consecutive_failures", state.ConsecutiveFailures,
				"total_failures", state.TotalFailures,
				"last_error", state.LastError,
				"last_duration", state.LastDuration,
			)
		}
	} else {
		if log != nil {
			log.Info("quarantined rule re-enabled",
				"rule_id", state.RuleID,
				"rule_type", state.RuleType,
				"rule_version", state.RuleVersion,
			)
		}
	}

	auditor, ok := st.(store.Audits)
	if !ok || state.RuleID == 0 {
		return
	}
	status := "recovered"
	if state.Quarantined {
		status = "quarantined"
	}
	detail, err := json.Marshal(map[string]any{
		"status":               status,
		"rule_id":              state.RuleID,
		"rule_type":            state.RuleType,
		"rule_version":         state.RuleVersion,
		"reason":               string(state.Reason),
		"consecutive_failures": state.ConsecutiveFailures,
		"last_error":           state.LastError,
	})
	if err != nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	_ = auditor.CreateAuditEntry(ctx, &store.AuditEntry{
		Actor:      "system:rule_quarantine",
		Action:     store.AuditActionUpdate,
		TargetType: "rule",
		TargetID:   state.RuleID,
		Diff:       detail,
	})
}

func handleEvalObservation(
	log *slog.Logger,
	m *metrics.Metrics,
	slowThreshold time.Duration,
	obs rules.EvalObservation,
) {
	m.RecordRuleIsolatedEval(obs.RuleID, obs.RuleType, obs.RuleDuration)
	if obs.FailureKind != rules.EvalFailureNone && obs.FailureKind != rules.EvalFailureStorage {
		m.RecordRuleEvalFailure(obs.RuleID, obs.RuleType, string(obs.FailureKind))
	}
	if slowThreshold > 0 && obs.RuleDuration >= slowThreshold && log != nil {
		log.Warn("slow rule evaluation",
			"rule_id", obs.RuleID,
			"rule_type", obs.RuleType,
			"rule_duration", obs.RuleDuration,
			"db_duration", obs.DBDuration,
		)
	}
}

func logIsolatedRuleFailure(log *slog.Logger, m store.Monitor, rule store.Rule, eventID string, err error) {
	if log == nil {
		return
	}
	var evalErr *rules.RuleEvalError
	if errors.As(err, &evalErr) {
		switch evalErr.Kind {
		case rules.EvalFailurePanic:
			log.Error("rule evaluation panicked and was isolated",
				"rule_id", rule.ID,
				"monitor_id", m.ID,
				"workspace_id", m.WorkspaceID,
				"rule_type", rule.Type,
				"event_id", eventID,
				"quarantined", evalErr.Quarantined,
				"err", err,
			)
			return
		case rules.EvalFailureTimeout:
			log.Warn("rule evaluation timed out and was isolated",
				"rule_id", rule.ID,
				"monitor_id", m.ID,
				"workspace_id", m.WorkspaceID,
				"rule_type", rule.Type,
				"event_id", eventID,
				"rule_duration", evalErr.Duration,
				"db_duration", evalErr.DBDuration,
				"quarantined", evalErr.Quarantined,
				"err", err,
			)
			return
		case rules.EvalFailureQuarantined:
			log.Warn("skipping quarantined rule",
				"rule_id", rule.ID,
				"monitor_id", m.ID,
				"workspace_id", m.WorkspaceID,
				"rule_type", rule.Type,
				"event_id", eventID,
				"err", err,
			)
			return
		}
	}
	log.Warn("rule evaluation failed",
		"rule_id", rule.ID,
		"monitor_id", m.ID,
		"workspace_id", m.WorkspaceID,
		"rule_type", rule.Type,
		"event_id", eventID,
		"err", err,
	)
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

	runbookURL, responseGuidance := store.ResolveAlertGuidance(&rule, &m, "", "")
	payload = store.WithGuidance(payload, runbookURL, responseGuidance)

	alert := &store.Alert{
		MonitorID:        m.ID,
		RuleID:           rule.ID,
		EventID:          eventID,
		Payload:          payload,
		Ledger:           ev.Ledger,
		LedgerClosedAt:   ev.LedgerClosedAt,
		Cooldown:         ruleCooldown(rule),
		Backfilled:       opts.Backfilled,
		Severity:         rule.Severity,
		RuleVersion:      rule.Version,
		RunbookURL:       runbookURL,
		ResponseGuidance: responseGuidance,
	}
	if in.enricher != nil {
		alert.Enrichment, err = in.enricher.Fetch(ctx, ev.ContractID, ev.EventName())
		if err != nil {
			in.log.Warn("alert enrichment failed; continuing without enrichment", "event_id", eventID, "err", err)
		}
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
	in.recordWorkspaceAlertRaised(ctx, m.WorkspaceID)
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
			Enrichment:  alert.Enrichment,
			CreatedAt:   alert.CreatedAt,
		})
	}

	if !opts.Deliver {
		return true, false
	}
	in.dispatch.Dispatch(ctx, notify.Alert{
		ID:               alert.ID,
		MonitorID:        m.ID,
		MonitorName:      m.Name,
		RuleID:           rule.ID,
		RuleType:         rule.Type,
		EventID:          eventID,
		ContractID:       ev.ContractID,
		EventName:        ev.EventName(),
		Ledger:           ev.Ledger,
		TxHash:           ev.TxHash,
		Payload:          alert.Payload,
		Enrichment:       alert.Enrichment,
		CreatedAt:        alert.CreatedAt,
		Severity:         string(alert.Severity),
		RunbookURL:       alert.RunbookURL,
		ResponseGuidance: alert.ResponseGuidance,
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

func (in *Ingestor) recordWorkspaceRuleEvaluations(ctx context.Context, workspaceID string, n int64) {
	if n <= 0 {
		return
	}
	if in.usageMeter != nil {
		in.usageMeter.RecordRuleEvaluations(workspaceID, n)
		return
	}
	if in.metrics != nil {
		in.metrics.RecordWorkspaceRuleEvaluations(workspaceID, n)
	}
	if uStore, ok := in.store.(store.Usage); ok {
		_ = store.SafeRecordWorkspaceUsageBatch(ctx, uStore, []store.WorkspaceUsageDelta{
			{WorkspaceID: workspaceID, RuleEvaluations: n},
		})
	}
}

func (in *Ingestor) recordWorkspaceAlertRaised(ctx context.Context, workspaceID string) {
	if in.usageMeter != nil {
		in.usageMeter.RecordAlertRaised(workspaceID, 1)
		return
	}
	if in.metrics != nil {
		in.metrics.RecordWorkspaceAlertRaised(workspaceID, 1)
	}
	if uStore, ok := in.store.(store.Usage); ok {
		_ = store.SafeRecordWorkspaceUsageBatch(ctx, uStore, []store.WorkspaceUsageDelta{
			{WorkspaceID: workspaceID, AlertsRaised: 1},
		})
	}
}
