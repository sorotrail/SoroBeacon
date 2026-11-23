package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// eventEmittedParams configure the event_emitted rule.
//
//	{
//	  "event_name": "transfer",            // optional: match first topic
//	  "topic_equals": {"1": "GABC..."}     // optional: topic index -> expected value
//	}
//
// The contract itself is scoped by the monitor, so it is not repeated here.
type eventEmittedParams struct {
	EventName   string         `json:"event_name"`
	TopicEquals map[string]any `json:"topic_equals"`
}

// EventEmitted matches when an event with a given name (first topic) fires,
// optionally requiring specific topic arguments to equal expected values.
type EventEmitted struct{}

func (EventEmitted) Validate(params json.RawMessage) error {
	p, err := parseEventEmitted(params)
	if err != nil {
		return err
	}
	var details FieldErrors
	if p.EventName == "" && len(p.TopicEquals) == 0 {
		details = append(details, FieldError{
			Field:  "event_name",
			Reason: "event_emitted: set event_name and/or topic_equals",
		})
	}
	for k := range p.TopicEquals {
		if _, err := strconv.Atoi(k); err != nil {
			details = append(details, FieldError{
				Field:  "topic_equals." + k,
				Reason: fmt.Sprintf("event_emitted: topic_equals key %q is not a topic index", k),
			})
		}
	}
	if len(details) > 0 {
		return details
	}
	return nil
}

func (EventEmitted) Evaluate(_ context.Context, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error) {
	p, err := parseEventEmitted(params)
	if err != nil {
		return false, err
	}
	if p.EventName != "" && ev.EventName() != p.EventName {
		return false, nil
	}
	for key, expected := range p.TopicEquals {
		idx, err := strconv.Atoi(key)
		if err != nil {
			return false, fmt.Errorf("event_emitted: topic_equals key %q is not a topic index", key)
		}
		if idx < 0 || idx >= len(ev.Topics) {
			return false, nil
		}
		if stellar.Canon(ev.Topics[idx]) != stellar.Canon(expected) {
			return false, nil
		}
	}
	return true, nil
}

// Explain returns a detailed explanation of why the event matched or didn't match.
func (EventEmitted) Explain(ctx context.Context, ev *stellar.DecodedEvent, params json.RawMessage) (*RuleExplanation, error) {
	p, err := parseEventEmitted(params)
	if err != nil {
		return &RuleExplanation{Error: err.Error()}, err
	}

	var conditions []ConditionExplanation
	matched := true

	// Check event name
	if p.EventName != "" {
		actual := ev.EventName()
		passed := actual == p.EventName
		if !passed {
			matched = false
		}
		conditions = append(conditions, ConditionExplanation{
			Name:     "event_name",
			Expected: p.EventName,
			Actual:   actual,
			Passed:   passed,
			Reason:   fmt.Sprintf("event name %q %s expected %q", actual, ternary(passed, "matches", "does not match"), p.EventName),
		})
	}

	// Check topic equals
	for key, expected := range p.TopicEquals {
		idx, err := strconv.Atoi(key)
		if err != nil {
			conditions = append(conditions, ConditionExplanation{
				Name:     "topic_equals." + key,
				Expected: expected,
				Actual:   nil,
				Passed:   false,
				Reason:   fmt.Sprintf("topic_equals key %q is not a valid topic index", key),
			})
			matched = false
			continue
		}
		if idx < 0 || idx >= len(ev.Topics) {
			conditions = append(conditions, ConditionExplanation{
				Name:     "topic_equals." + key,
				Expected: expected,
				Actual:   nil,
				Passed:   false,
				Reason:   fmt.Sprintf("topic index %d is out of range (event has %d topics)", idx, len(ev.Topics)),
			})
			matched = false
			continue
		}
		actual := ev.Topics[idx]
		canonExpected := stellar.Canon(expected)
		canonActual := stellar.Canon(actual)
		passed := canonActual == canonExpected
		if !passed {
			matched = false
		}
		conditions = append(conditions, ConditionExplanation{
			Name:     "topic_equals." + key,
			Expected: expected,
			Actual:   actual,
			Passed:   passed,
			Reason:   fmt.Sprintf("topic[%d] canonical form %s expected %s", idx, ternary(passed, "matches", "does not match"), canonExpected),
		})
	}

	return &RuleExplanation{
		Matched:    matched,
		Conditions: conditions,
	}, nil
}

// EventNames reports the event name this rule is scoped to. A rule with no
// event_name matches any event (perhaps via topic_equals), so it cannot name
// them and callers must not narrow a server-side filter.
func (EventEmitted) EventNames(params json.RawMessage) ([]string, bool) {
	p, err := parseEventEmitted(params)
	if err != nil || p.EventName == "" {
		return nil, false
	}
	return []string{p.EventName}, true
}

func parseEventEmitted(params json.RawMessage) (eventEmittedParams, error) {
	var p eventEmittedParams
	if err := json.Unmarshal(params, &p); err != nil {
		return p, fmt.Errorf("event_emitted: invalid params: %w", err)
	}
	return p, nil
}
