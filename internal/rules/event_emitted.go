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
	if p.EventName == "" && len(p.TopicEquals) == 0 {
		return fmt.Errorf("event_emitted: set event_name and/or topic_equals")
	}
	for k := range p.TopicEquals {
		if _, err := strconv.Atoi(k); err != nil {
			return fmt.Errorf("event_emitted: topic_equals key %q is not a topic index", k)
		}
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

func parseEventEmitted(params json.RawMessage) (eventEmittedParams, error) {
	var p eventEmittedParams
	if err := json.Unmarshal(params, &p); err != nil {
		return p, fmt.Errorf("event_emitted: invalid params: %w", err)
	}
	return p, nil
}
