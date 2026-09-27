package rules

import (
	"encoding/json"
	"fmt"
	"time"
)

// CooldownParam is the cross-cutting rule param that suppresses repeat alerts
// from a rule for a window after it fires, e.g. {"event_name":"transfer",
// "cooldown":"5m"}. It is deliberately not owned by any single rule type:
// evaluators ignore keys they do not use, so a rule of any type can carry it.
const CooldownParam = "cooldown"

// ParseCooldown reads a rule's optional cooldown. The value is a Go duration
// string ("5m", "90s"); absent or empty means no cooldown. A negative or
// unparseable value is an error so the API rejects it when the rule is created,
// rather than silently leaving a busy rule with no suppression.
func ParseCooldown(params json.RawMessage) (time.Duration, error) {
	if len(params) == 0 {
		return 0, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(params, &raw); err != nil {
		// Malformed params are the evaluator's to report; cooldown is only
		// read when the params are an object.
		return 0, nil
	}
	value, ok := raw[CooldownParam]
	if !ok {
		return 0, nil
	}
	var s string
	if err := json.Unmarshal(value, &s); err != nil {
		return 0, FieldError{
			Field:  CooldownParam,
			Reason: `cooldown: must be a duration string such as "5m" or "90s"`,
		}
	}
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, FieldError{
			Field:  CooldownParam,
			Reason: fmt.Sprintf(`cooldown: invalid duration %q (want a Go duration such as "5m" or "90s")`, s),
		}
	}
	if d < 0 {
		return 0, FieldError{Field: CooldownParam, Reason: "cooldown: must not be negative"}
	}
	return d, nil
}
