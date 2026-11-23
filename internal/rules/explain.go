// Package rules provides the RuleExplainer interface for explaining why
// an event did or did not match a rule.
package rules

import (
	"context"
	"encoding/json"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// RuleExplanation describes the outcome of evaluating a rule against an event.
type RuleExplanation struct {
	// Matched is true if the rule matched the event.
	Matched bool `json:"matched"`
	// Conditions is a list of individual conditions that were checked.
	Conditions []ConditionExplanation `json:"conditions"`
	// Error is any error that occurred during evaluation.
	Error string `json:"error,omitempty"`
}

// ConditionExplanation describes one condition that was checked.
type ConditionExplanation struct {
	// Name is the human-readable name of the condition.
	Name string `json:"name"`
	// Expected is the expected value for the condition.
	Expected any `json:"expected,omitempty"`
	// Actual is the actual value from the event.
	Actual any `json:"actual,omitempty"`
	// Passed is true if the condition passed.
	Passed bool `json:"passed"`
	// Reason explains why the condition passed or failed.
	Reason string `json:"reason,omitempty"`
}

// RuleExplainer is an optional interface that rule evaluators can implement
// to provide explanations for why an event did or did not match.
// When not implemented, the registry will return a basic explanation based
// on the match result.
type RuleExplainer interface {
	// Explain evaluates the rule against the event and returns a detailed
	// explanation of which conditions passed or failed. It should not have
	// side effects and should be safe to call at any time (not just during
	// normal evaluation).
	Explain(ctx context.Context, ev *stellar.DecodedEvent, params json.RawMessage) (*RuleExplanation, error)
}