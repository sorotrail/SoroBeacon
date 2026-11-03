// Package rules matches decoded Soroban events against user-defined rules.
//
// Contributors: add a new rule type by implementing RuleEvaluator and
// registering it in NewRegistry (or via Registry.Register from your own
// wiring). Params arrive as the rule's raw JSON; validate them in Validate
// so the API can reject bad rules at create time.
package rules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/telemetry"
)

// Rule type names understood by the default registry.
const (
	TypeEventEmitted       = "event_emitted"
	TypeValueThreshold     = "value_threshold"
	TypeContractAllowlist  = "contract_allowlist"
	TypeEventNameGlob      = "event_name_glob"
	TypeNumericRange       = "numeric_range"
	TypeTokenSupplyChange  = "token_supply_change"
	TypeFrequencyThreshold = "frequency_threshold"
	TypeTopicRegex         = "topic_regex"
	TypeAddressWatchlist   = "address_watchlist"
	TypeTopicPosition      = "topic_position"
	// TypeAbsenceOfEvent is driven by the poller's sweep rather than by
	// event arrival, so it is registered as an AbsenceEvaluator, not an
	// evaluator.
	TypeAbsenceOfEvent = "absence_of_event"
)

// RuleEvaluator decides whether one decoded event matches one rule.
//
// Most implementations must be stateless and safe for concurrent use. An
// evaluator that genuinely needs state across events (frequency_threshold)
// may keep it, but must key it by the RuleID the poller puts in the context
// (WithRuleID) and guard it against concurrent use.
type RuleEvaluator interface {
	// Evaluate reports whether ev matches the rule described by params.
	Evaluate(ctx context.Context, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error)
	// Validate checks params without an event, so the API can reject
	// malformed rules at create/update time.
	Validate(params json.RawMessage) error
}

// AlertEventIDer is implemented by evaluators that choose the event_id their
// alert is stored under, instead of the triggering event's own id. The
// frequency rule uses it so every crossing inside one episode shares a
// synthetic id and the store's dedup guard fires once.
type AlertEventIDer interface {
	// AlertEventID returns the id the alert should be stored under, or "" to
	// fall back to the source event's id.
	AlertEventID(ctx context.Context, ev *stellar.DecodedEvent, params json.RawMessage) string
}

// ruleIDKey carries the rule under evaluation in the context.
type ruleIDKey struct{}

// WithRuleID returns a context carrying the id of the rule under evaluation,
// so a stateful evaluator can key its per-rule state without the RuleEvaluator
// interface growing a rule argument.
func WithRuleID(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, ruleIDKey{}, id)
}

// RuleID returns the rule id carried by ctx, or 0 when none is set.
func RuleID(ctx context.Context) int64 {
	id, _ := ctx.Value(ruleIDKey{}).(int64)
	return id
}

// EventNamer is implemented by evaluators that can name the events they
// match. The poller uses it to compile server-side getEvents topic filters,
// which is purely a bandwidth optimisation: client-side evaluation stays the
// source of truth, so a rule that cannot name its events (or names none)
// simply leaves its contract unfiltered.
type EventNamer interface {
	// EventNames returns the concrete event names the rule can match. ok is
	// false when the rule may match events the names do not cover, so a
	// caller must not narrow a server-side filter on the strength of it.
	EventNames(params json.RawMessage) (names []string, ok bool)
}

// Registry maps rule type names to evaluators.
//
// There are two kinds of rule, and a type belongs to exactly one of them: one
// evaluated per arriving event (an event that matches fires the rule), and one
// evaluated on a timer (an event that matches merely re-arms it). Keeping them
// in one Registry means callers that only care about "is this type known?" —
// the API validating params, the dashboard listing types — do not have to know
// the difference; only the poller does, via Absence.
type Registry struct {
	evaluators map[string]RuleEvaluator
	absences   map[string]AbsenceEvaluator
	// telemetry is optional tracing; a nil Registry.telemetry (tests construct
	// registries directly) just means no spans.
	telemetry *telemetry.Provider

	mu            sync.RWMutex
	evalTimeout   time.Duration
	isolator      *ruleIsolator
	panicObserver func()
}

// WithTelemetry attaches tracing to every evaluation this registry runs.
// The span is a child of the poll cycle span, so a slow rule is visible on
// the same timeline as the fetch that produced the event and the delivery
// its alert caused.
func (r *Registry) WithTelemetry(t *telemetry.Provider) *Registry {
	r.telemetry = t
	return r
}

// WithEvalTimeout sets the per-rule evaluation timeout budget. A non-positive
// duration restores DefaultRuleEvalTimeout.
func (r *Registry) WithEvalTimeout(d time.Duration) *Registry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d <= 0 {
		r.evalTimeout = DefaultRuleEvalTimeout
	} else {
		r.evalTimeout = d
	}
	return r
}

// EvalTimeout returns the active per-rule evaluation timeout budget.
func (r *Registry) EvalTimeout() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.evalTimeout <= 0 {
		return DefaultRuleEvalTimeout
	}
	return r.evalTimeout
}

// WithQuarantineThreshold configures how many consecutive rule-attributed
// timeouts or panics trigger quarantine for a rule.
func (r *Registry) WithQuarantineThreshold(threshold int) *Registry {
	r.isolator.mu.Lock()
	defer r.isolator.mu.Unlock()
	if threshold <= 0 {
		r.isolator.threshold = DefaultQuarantineThreshold
	} else {
		r.isolator.threshold = threshold
	}
	return r
}

// WithLogger attaches a structured logger so quarantine transitions are logged
// with the offending rule's identifiers and failure reason.
func (r *Registry) WithLogger(log *slog.Logger) *Registry {
	r.isolator.mu.Lock()
	defer r.isolator.mu.Unlock()
	r.isolator.log = log
	return r
}

// WithPanicObserver registers a callback invoked whenever a rule panic is
// recovered during evaluation (used to increment sorobeacon_poll_panics_total).
func (r *Registry) WithPanicObserver(fn func()) *Registry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.panicObserver = fn
	return r
}

// QuarantineState returns the current failure and quarantine status for ruleID.
func (r *Registry) QuarantineState(ruleID int64) (RuleQuarantineState, bool) {
	return r.isolator.state(ruleID)
}

// QuarantinedRules returns the snapshot of all currently quarantined rules,
// ordered by RuleID.
func (r *Registry) QuarantinedRules() []RuleQuarantineState {
	return r.isolator.quarantinedList()
}

// QuarantineAuditLog returns the chronological audit trail of rule quarantine
// and re-enable actions.
func (r *Registry) QuarantineAuditLog() []QuarantineAuditEntry {
	return r.isolator.auditLog()
}

// ReenableRule clears a rule's quarantine and resets its consecutive failure
// count so a fixed rule resumes evaluation. It returns true when the rule was
// quarantined and has been re-enabled.
func (r *Registry) ReenableRule(ruleID int64) bool {
	return r.isolator.reenable(ruleID)
}

// NewRegistry returns a Registry with the built-in rule types registered.
func NewRegistry() *Registry {
	r := &Registry{
		evaluators:  map[string]RuleEvaluator{},
		absences:    map[string]AbsenceEvaluator{},
		evalTimeout: DefaultRuleEvalTimeout,
		isolator:    newRuleIsolator(),
	}
	r.Register(TypeEventEmitted, EventEmitted{})
	r.Register(TypeValueThreshold, ValueThreshold{})
	r.Register(TypeContractAllowlist, &ContractAllowlist{})
	r.Register(TypeEventNameGlob, EventNameGlob{})
	r.Register(TypeNumericRange, NumericRange{})
	r.Register(TypeTokenSupplyChange, TokenSupplyChange{})
	r.Register(TypeTokenEvent, TokenEvent{})
	r.Register(TypeSelfTransfer, SelfTransfer{})
	r.Register(TypeTimeWindow, TimeWindow{})
	r.Register(TypeFrequencyThreshold, NewFrequencyThreshold())
	// The composite is registered last because it resolves child types
	// through this registry; passing r to itself lets it see every leaf above
	// and any type registered on this registry later.
	r.Register(TypeComposite, NewComposite(r))
	r.Register(TypeTopicRegex, &TopicRegex{})
	r.Register(TypeAddressWatchlist, &AddressWatchlist{})
	r.Register(TypeTopicPosition, TopicPosition{})
	r.RegisterAbsence(TypeAbsenceOfEvent, Absence{})
	return r
}

// Register adds (or replaces) an evaluator for a rule type name.
func (r *Registry) Register(name string, e RuleEvaluator) {
	r.evaluators[name] = e
}

// RegisterAbsence adds (or replaces) an absence rule type. Registering the
// same name here and in Register is a wiring mistake: the poller looks a rule
// up as an absence first, so the evaluator would never run.
func (r *Registry) RegisterAbsence(name string, e AbsenceEvaluator) {
	r.absences[name] = e
}

// Absence returns the absence evaluator for ruleType, if it is registered as
// one. The poller asks per rule: anything not here is evaluated per event.
func (r *Registry) Absence(ruleType string) (AbsenceEvaluator, bool) {
	e, ok := r.absences[ruleType]
	return e, ok
}

// Types returns every registered rule type, absence types included, so the
// dashboard's rule-type picker and any "known type" check see one list.
func (r *Registry) Types() []string {
	out := make([]string, 0, len(r.evaluators)+len(r.absences))
	for name := range r.evaluators {
		out = append(out, name)
	}
	for name := range r.absences {
		out = append(out, name)
	}
	return out
}

// Evaluate runs the evaluator registered for ruleType inside the per-rule
// isolation boundary: quarantined rules are rejected immediately, panics are
// recovered and attributed to the rule, and evaluation time is bounded by
// EvalTimeout.
func (r *Registry) Evaluate(ctx context.Context, ruleType string, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error) {
	// Nested child evaluations inside a composite rule already run inside the
	// outer rule's isolated timeout and panic recovery boundary.
	if inIsolatedEval(ctx) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		return r.evaluate(ctx, ruleType, ev, params)
	}

	ctx, span := r.startEvalSpan(ctx, ruleType, ev)
	matched, err := r.evaluateIsolated(ctx, ruleType, ev, params)
	if err != nil {
		telemetry.RecordError(span, err)
	}
	telemetry.SetAttrs(span, "matched", matched)
	span.End()
	return matched, err
}

type isolatedResult struct {
	matched  bool
	err      error
	panicErr *RulePanicError
}

func (r *Registry) notifyPanic() {
	r.mu.RLock()
	fn := r.panicObserver
	r.mu.RUnlock()
	if fn != nil {
		fn()
	}
}

func (r *Registry) evaluateIsolated(ctx context.Context, ruleType string, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error) {
	ruleID := RuleID(ctx)
	monitorID := MonitorID(ctx)

	if st, quarantined := r.isolator.checkQuarantined(ruleID); quarantined {
		return false, &QuarantinedRuleError{
			RuleID:        ruleID,
			MonitorID:     st.MonitorID,
			RuleType:      ruleType,
			Reason:        st.Reason,
			LastError:     st.LastError,
			QuarantinedAt: st.QuarantinedAt,
		}
	}

	timeout := r.EvalTimeout()
	evalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	evalCtx = context.WithValue(evalCtx, isolatedEvalKey{}, true)
	evalCtx, tracker := withDependencyTracker(evalCtx)

	start := time.Now()
	done := make(chan isolatedResult, 1)
	go func() {
		var res isolatedResult
		defer func() {
			if rec := recover(); rec != nil {
				res.panicErr = recoverRulePanic(ruleID, monitorID, ruleType, rec)
			}
			done <- res
		}()
		res.matched, res.err = r.evaluate(evalCtx, ruleType, ev, params)
	}()

	select {
	case res := <-done:
		return r.handleIsolatedResult(ruleID, monitorID, ruleType, timeout, start, tracker, res)
	case <-evalCtx.Done():
		// If the goroutine finished at the same instant the deadline fired,
		// prefer its concrete outcome (such as a recovered panic).
		select {
		case res := <-done:
			return r.handleIsolatedResult(ruleID, monitorID, ruleType, timeout, start, tracker, res)
		default:
		}
		return false, r.recordTimeout(ruleID, monitorID, ruleType, timeout, start, tracker)
	}
}

func (r *Registry) handleIsolatedResult(
	ruleID, monitorID int64,
	ruleType string,
	timeout time.Duration,
	start time.Time,
	tracker *dependencyTracker,
	res isolatedResult,
) (bool, error) {
	if res.panicErr != nil {
		r.isolator.recordStrike(ruleID, monitorID, ruleType, QuarantineReasonPanic, res.panicErr.Error())
		r.notifyPanic()
		return false, res.panicErr
	}
	elapsed := time.Since(start)
	if res.err != nil {
		if errors.Is(res.err, context.DeadlineExceeded) {
			return false, r.recordTimeout(ruleID, monitorID, ruleType, timeout, start, tracker)
		}
		return false, res.err
	}
	if elapsed >= timeout {
		return false, r.recordTimeout(ruleID, monitorID, ruleType, timeout, start, tracker)
	}
	r.isolator.recordSuccess(ruleID)
	return res.matched, nil
}

func (r *Registry) recordTimeout(
	ruleID, monitorID int64,
	ruleType string,
	timeout time.Duration,
	start time.Time,
	tracker *dependencyTracker,
) error {
	now := time.Now()
	elapsed := now.Sub(start)
	depElapsed, depErr := tracker.snapshot(now)
	// When the backing store (e.g. MatchLog) consumed the evaluation budget or
	// failed, the timeout is attributed to the dependency rather than the rule
	// so database slowdowns never quarantine rules.
	depAttributed := depErr != nil || (depElapsed > 0 && (elapsed-depElapsed) < timeout/2)
	te := &RuleTimeoutError{
		RuleID:               ruleID,
		MonitorID:            monitorID,
		RuleType:             ruleType,
		Budget:               timeout,
		Elapsed:              elapsed,
		DependencyElapsed:    depElapsed,
		DependencyAttributed: depAttributed,
	}
	if !depAttributed {
		r.isolator.recordStrike(ruleID, monitorID, ruleType, QuarantineReasonTimeout, te.Error())
	}
	return te
}

// startEvalSpan opens the rules.evaluate span with the identifiers an
// operator needs to connect one evaluation to its monitor, rule and event.
// Only ids and public chain data: params and the decoded value can embed
// operator-chosen strings, so they never go into attributes.
func (r *Registry) startEvalSpan(ctx context.Context, ruleType string, ev *stellar.DecodedEvent) (context.Context, trace.Span) {
	if r.telemetry == nil {
		// Preserve the ctx even without tracing: WithRuleID and friends ride
		// the same context values.
		return ctx, telemetry.NoopSpan()
	}
	ctx, span := r.telemetry.WithRequestID(ctx, "rules.evaluate",
		trace.WithAttributes(
			attribute.String(telemetry.AttrRuleType, ruleType),
			attribute.Int64(telemetry.AttrRuleID, RuleID(ctx)),
			attribute.String(telemetry.AttrEventID, ev.ID),
			attribute.String(telemetry.AttrContractID, ev.ContractID),
		),
	)
	return ctx, span
}

// evaluate is Evaluate without its span, so the error handling above stays
// readable.
func (r *Registry) evaluate(ctx context.Context, ruleType string, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error) {
	e, ok := r.evaluators[ruleType]
	if !ok {
		// An absence rule type answers false rather than erroring: it is a
		// known type that no single event can satisfy, and a caller that
		// evaluates every rule of a monitor (the poller does) must not log
		// those as broken. The poller's sweep is what fires them.
		if _, isAbsence := r.absences[ruleType]; isAbsence {
			return false, nil
		}
		return false, fmt.Errorf("unknown rule type %q", ruleType)
	}
	return e.Evaluate(ctx, ev, params)
}

// AlertEventID returns the event id ruleType wants its alert stored under, or
// "" to use the source event's own id.
func (r *Registry) AlertEventID(ctx context.Context, ruleType string, ev *stellar.DecodedEvent, params json.RawMessage) string {
	e, ok := r.evaluators[ruleType]
	if !ok {
		return ""
	}
	provider, ok := e.(AlertEventIDer)
	if !ok {
		return ""
	}
	return provider.AlertEventID(ctx, ev, params)
}

// EventNames reports the concrete event names a rule of ruleType matches.
// ok is false when the type is unknown, the evaluator cannot name its events,
// or the rule may match events beyond the names, so callers must not narrow a
// server-side filter on the strength of it.
func (r *Registry) EventNames(ruleType string, params json.RawMessage) ([]string, bool) {
	e, registered := r.evaluators[ruleType]
	if !registered {
		return nil, false
	}
	namer, ok := e.(EventNamer)
	if !ok {
		return nil, false
	}
	return namer.EventNames(params)
}

// Validate checks params for ruleType, for use at rule create/update time.
func (r *Registry) Validate(ruleType string, params json.RawMessage) error {
	e, ok := r.evaluators[ruleType]
	if !ok {
		if a, isAbsence := r.absences[ruleType]; isAbsence {
			if err := a.Validate(params); err != nil {
				return err
			}
			_, err := ParseCooldown(params)
			return err
		}
		return fmt.Errorf("unknown rule type %q (registered: %v)", ruleType, r.Types())
	}
	if err := e.Validate(params); err != nil {
		return err
	}
	// The cooldown applies to every rule type, so it is validated once here
	// rather than duplicated in each evaluator's Validate.
	_, err := ParseCooldown(params)
	return err
}

// Explain returns a detailed explanation of why an event matched or didn't
// match a rule. If the rule type doesn't implement RuleExplainer, a basic
// explanation is returned based on the match result.
func (r *Registry) Explain(ctx context.Context, ruleType string, ev *stellar.DecodedEvent, params json.RawMessage) (*RuleExplanation, error) {
	e, ok := r.evaluators[ruleType]
	if !ok {
		if _, isAbsence := r.absences[ruleType]; isAbsence {
			return &RuleExplanation{
				Matched:    false,
				Conditions: []ConditionExplanation{{Name: "absence_rule", Expected: "timer-based evaluation", Actual: "event-driven evaluation", Passed: false, Reason: "absence rules are evaluated on a timer, not per event"}},
			}, nil
		}
		return nil, fmt.Errorf("unknown rule type %q", ruleType)
	}

	explainer, ok := e.(RuleExplainer)
	if !ok {
		// Fall back to basic evaluation
		matched, err := e.Evaluate(ctx, ev, params)
		if err != nil {
			return &RuleExplanation{Error: err.Error()}, err
		}
		return &RuleExplanation{
			Matched: matched,
			Conditions: []ConditionExplanation{{
				Name:     "rule_match",
				Expected: "match",
				Actual:   ternary(matched, "matched", "no match"),
				Passed:   matched,
				Reason:   ternary(matched, "rule matched", "rule did not match"),
			}},
		}, nil
	}

	return explainer.Explain(ctx, ev, params)
}

// ternary is a helper for inline conditional strings.
func ternary[T any](cond bool, yes, no T) T {
	if cond {
		return yes
	}
	return no
}

// DryRunResult is the result of evaluating a rule against historical events.
type DryRunResult struct {
	Matches []*stellar.DecodedEvent
	Count   int
}

// DryRun evaluates candidate rule params against a slice of historical
// events using the production evaluation path. It returns the events
// that matched the rule. Nothing is persisted and nothing is delivered.
//
// Design decision: the event corpus used here is stored alert payloads,
// which are biased because only matched events are captured. The caller
// documents this bias in the response note.
func (r *Registry) DryRun(ctx context.Context, ruleType string, events []*stellar.DecodedEvent, params json.RawMessage) ([]*stellar.DecodedEvent, error) {
	e, ok := r.evaluators[ruleType]
	if !ok {
		return nil, fmt.Errorf("unknown rule type %q", ruleType)
	}
	matches := make([]*stellar.DecodedEvent, 0, len(events))
	for _, ev := range events {
		matched, err := e.Evaluate(ctx, ev, params)
		if err != nil {
			return nil, err
		}
		if matched {
			matches = append(matches, ev)
		}
	}
	return matches, nil
}
