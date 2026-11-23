package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"path"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// eventNameGlobParams configure the event_name_glob rule.
//
//	{ "patterns": ["swap_*", "pool_deposit"] }
//
// The glob covers the common case topic_regex is overkill for: "every event
// starting with swap_", in a syntax operators write correctly on the first
// try. Patterns follow path.Match (* matches any run of non-separator
// characters, ? matches one, [...] character classes), and matching is
// against the whole event name with an implicit anchor at both ends, so
// "swap" does not match "swap_exact_in" while "swap_*" does.
type eventNameGlobParams struct {
	Patterns []string `json:"patterns"`
}

// EventNameGlob matches the conventional event name (the first topic as a
// symbol) against one or more glob patterns. Multiple patterns are ORed: the
// event matches when any pattern matches the whole name. An event with no
// name (empty topics) is a non-match, not an error; only invalid params
// (rejected by Validate at create time) error.
type EventNameGlob struct{}

func (EventNameGlob) Validate(params json.RawMessage) error {
	p, err := parseEventNameGlob(params)
	if err != nil {
		return err
	}
	var details FieldErrors
	if len(p.Patterns) == 0 {
		details = append(details, FieldError{
			Field:  "patterns",
			Reason: "event_name_glob: patterns is required (a non-empty list of glob patterns)",
		})
	}
	for i, pattern := range p.Patterns {
		if _, err := path.Match(pattern, ""); err != nil {
			details = append(details, FieldError{
				Field:  fmt.Sprintf("patterns[%d]", i),
				Reason: fmt.Sprintf("event_name_glob: patterns[%d] %q is malformed: %v", i, pattern, err),
			})
		}
	}
	if len(details) > 0 {
		return details
	}
	return nil
}

func (EventNameGlob) Evaluate(_ context.Context, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error) {
	p, err := parseEventNameGlob(params)
	if err != nil {
		return false, err
	}
	if len(p.Patterns) == 0 {
		return false, fmt.Errorf("event_name_glob: invalid params: patterns is required")
	}
	// EventName already handles both decoded topic shapes (a bare string on
	// the XDR decode path and the {"symbol": "..."} wrapper on the RPC
	// xdrFormat:"json" path), so there is no reason to reach into Topics
	// here. An empty name means the event carries no conventional name and
	// simply does not match.
	name := ev.EventName()
	if name == "" {
		return false, nil
	}
	for _, pattern := range p.Patterns {
		// path.Match anchors at both ends: the whole name must fit the
		// pattern, which is the behaviour people otherwise get wrong by
		// assuming a substring or prefix match.
		matched, err := path.Match(pattern, name)
		if err != nil {
			return false, fmt.Errorf("event_name_glob: invalid pattern %q: %w", pattern, err)
		}
		if matched {
			return true, nil
		}
	}
	return false, nil
}

func parseEventNameGlob(params json.RawMessage) (eventNameGlobParams, error) {
	var p eventNameGlobParams
	if err := json.Unmarshal(params, &p); err != nil {
		return p, fmt.Errorf("event_name_glob: invalid params: %w", err)
	}
	return p, nil
}
