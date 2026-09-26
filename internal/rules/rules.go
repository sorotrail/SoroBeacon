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
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/telemetry"
)

// Rule type names understood by the default registry.
const (
	TypeEventEmitted       = "event_emitted"
	TypeValueThreshold     = "value_threshold"
	TypeFrequencyThreshold = "frequency_threshold"
	TypeTopicRegex         = "topic_regex"
	TypeAddressWatchlist   = "address_watchlist"
	TypeTopicPosition      = "topic_position"
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
type Registry struct {
	evaluators map[string]RuleEvaluator
	// telemetry is optional tracing; a nil Registry.telemetry (tests construct
	// registries directly) just means no spans.
	telemetry *telemetry.Provider
}

// WithTelemetry attaches tracing to every evaluation this registry runs.
// The span is a child of the poll cycle span, so a slow rule is visible on
// the same timeline as the fetch that produced the event and the delivery
// its alert caused.
func (r *Registry) WithTelemetry(t *telemetry.Provider) *Registry {
	r.telemetry = t
	return r
}

// NewRegistry returns a Registry with the built-in rule types registered.
func NewRegistry() *Registry {
	r := &Registry{evaluators: map[string]RuleEvaluator{}}
	r.Register(TypeEventEmitted, EventEmitted{})
	r.Register(TypeValueThreshold, ValueThreshold{})
	r.Register(TypeTokenEvent, TokenEvent{})
	r.Register(TypeFrequencyThreshold, NewFrequencyThreshold())
	r.Register(TypeTopicRegex, &TopicRegex{})
	r.Register(TypeAddressWatchlist, &AddressWatchlist{})
	r.Register(TypeTopicPosition, TopicPosition{})
	return r
}

// Register adds (or replaces) an evaluator for a rule type name.
func (r *Registry) Register(name string, e RuleEvaluator) {
	r.evaluators[name] = e
}

// Types returns the registered rule type names.
func (r *Registry) Types() []string {
	out := make([]string, 0, len(r.evaluators))
	for name := range r.evaluators {
		out = append(out, name)
	}
	return out
}

// Evaluate runs the evaluator registered for ruleType.
func (r *Registry) Evaluate(ctx context.Context, ruleType string, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error) {
	ctx, span := r.startEvalSpan(ctx, ruleType, ev)
	matched, err := r.evaluate(ctx, ruleType, ev, params)
	if err != nil {
		telemetry.RecordError(span, err)
	}
	telemetry.SetAttrs(span, "matched", matched)
	span.End()
	return matched, err
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
