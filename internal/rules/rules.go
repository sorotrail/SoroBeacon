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

	"github.com/sorobeacon/sorobeacon/internal/stellar"
)

// Rule type names understood by the default registry.
const (
	TypeEventEmitted   = "event_emitted"
	TypeValueThreshold = "value_threshold"
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
type Registry struct {
	evaluators map[string]RuleEvaluator
}

// NewRegistry returns a Registry with the built-in rule types registered.
func NewRegistry() *Registry {
	r := &Registry{evaluators: map[string]RuleEvaluator{}}
	r.Register(TypeEventEmitted, EventEmitted{})
	r.Register(TypeValueThreshold, ValueThreshold{})
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
	e, ok := r.evaluators[ruleType]
	if !ok {
		return false, fmt.Errorf("unknown rule type %q", ruleType)
	}
	return e.Evaluate(ctx, ev, params)
}

// Validate checks params for ruleType, for use at rule create/update time.
func (r *Registry) Validate(ruleType string, params json.RawMessage) error {
	e, ok := r.evaluators[ruleType]
	if !ok {
		return fmt.Errorf("unknown rule type %q (registered: %v)", ruleType, r.Types())
	}
	return e.Validate(params)
}
