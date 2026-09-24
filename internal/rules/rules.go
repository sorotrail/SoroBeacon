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

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// Rule type names understood by the default registry.
const (
	TypeEventEmitted   = "event_emitted"
	TypeValueThreshold = "value_threshold"
	// TypeAbsenceOfEvent is driven by the poller's sweep rather than by event
	// arrival, so it is registered as an AbsenceEvaluator, not an evaluator.
	TypeAbsenceOfEvent = "absence_of_event"
)

// RuleEvaluator decides whether one decoded event matches one rule.
// Implementations must be stateless and safe for concurrent use.
type RuleEvaluator interface {
	// Evaluate reports whether ev matches the rule described by params.
	Evaluate(ctx context.Context, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error)
	// Validate checks params without an event, so the API can reject
	// malformed rules at create/update time.
	Validate(params json.RawMessage) error
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
}

// NewRegistry returns a Registry with the built-in rule types registered.
func NewRegistry() *Registry {
	r := &Registry{
		evaluators: map[string]RuleEvaluator{},
		absences:   map[string]AbsenceEvaluator{},
	}
	r.Register(TypeEventEmitted, EventEmitted{})
	r.Register(TypeValueThreshold, ValueThreshold{})
	r.Register(TypeTokenEvent, TokenEvent{})
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

// Evaluate runs the evaluator registered for ruleType.
//
// An absence rule type answers false rather than erroring: it is a known type
// that no event can satisfy, and a caller that evaluates every rule of a
// monitor (the poller does) must not log those as broken. The poller's sweep
// is what fires them; see AbsenceEvaluator.
func (r *Registry) Evaluate(ctx context.Context, ruleType string, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error) {
	e, ok := r.evaluators[ruleType]
	if !ok {
		if _, isAbsence := r.absences[ruleType]; isAbsence {
			return false, nil
		}
		return false, fmt.Errorf("unknown rule type %q", ruleType)
	}
	return e.Evaluate(ctx, ev, params)
}

// Validate checks params for ruleType, for use at rule create/update time.
func (r *Registry) Validate(ruleType string, params json.RawMessage) error {
	if e, ok := r.evaluators[ruleType]; ok {
		return e.Validate(params)
	}
	if e, ok := r.absences[ruleType]; ok {
		return e.Validate(params)
	}
	return fmt.Errorf("unknown rule type %q (registered: %v)", ruleType, r.Types())
}
