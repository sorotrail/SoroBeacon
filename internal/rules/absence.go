package rules

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// absenceParams configure the absence_of_event rule.
//
//	{
//	  "event_name": "heartbeat",   // required: the event that should keep arriving
//	  "window": "30m"              // required: how long silence is tolerated
//	}
//
// The contract itself is scoped by the monitor, as it is for every other rule
// type: a monitor watching several contracts is quiet when none of them emits
// the event.
type absenceParams struct {
	EventName string `json:"event_name"`
	Window    string `json:"window"`
}

// AbsenceSpec is the parsed, validated form of an absence rule's params: the
// event the rule waits for, and how long it tolerates not seeing it.
type AbsenceSpec struct {
	EventName string
	Window    time.Duration
}

// AbsenceEvaluator is the shape a rule type takes when what it watches for
// cannot produce an event. "Alert me when my contract goes quiet" is not
// expressible as RuleEvaluator: the event you care about is the one that never
// arrived, so there is nothing to pass to Evaluate. Rule types of this shape
// are registered separately (Registry.RegisterAbsence) and driven by the
// poller's periodic sweep instead of by event arrival:
//
//   - the poller calls Matches on arriving events, and a match only re-arms
//     the rule — it can never fire it;
//   - the sweep compares the stored last-seen clock against Window and fires
//     when the window has elapsed.
//
// Implementations must be stateless and safe for concurrent use, exactly like
// RuleEvaluator.
type AbsenceEvaluator interface {
	// Validate checks params without an event, so the API can reject
	// malformed rules at create/update time.
	Validate(params json.RawMessage) error
	// Spec parses params for the sweep. It repeats Validate's checks and
	// returns the first problem as a plain error, because the sweep's only
	// recourse is to log it and skip the rule.
	Spec(params json.RawMessage) (AbsenceSpec, error)
	// Matches reports whether ev is the event this rule waits for, and so
	// re-arms it. spec is the already-parsed params from Spec.
	Matches(ev *stellar.DecodedEvent, spec AbsenceSpec) bool
}

// Absence fires when an expected event has not been seen for the configured
// window.
//
// It is deliberately not a RuleEvaluator: every method here answers a
// question about time, not about an incoming event. The poller asks the
// registry which shape a rule is (Registry.Absence) before deciding how to
// treat it, so an absence rule is never evaluated against an event.
type Absence struct{}

// Validate rejects params the sweep could not act on: a missing event name or
// a window that is unparseable or not positive.
func (Absence) Validate(params json.RawMessage) error {
	if _, problems := parseAbsenceProblems(params); len(problems) > 0 {
		return problems
	}
	return nil
}

func (Absence) Spec(params json.RawMessage) (AbsenceSpec, error) {
	spec, problems := parseAbsenceProblems(params)
	if len(problems) > 0 {
		return AbsenceSpec{}, problems
	}
	return spec, nil
}

// Matches reports whether ev is the awaited event. Matching is by name only
// for now — topic filters would have to be re-armed by the same event that
// re-arms the name, so they belong in this method when someone needs them.
func (Absence) Matches(ev *stellar.DecodedEvent, spec AbsenceSpec) bool {
	if spec.EventName == "" || ev == nil {
		return false
	}
	return ev.EventName() == spec.EventName
}

// parseAbsenceProblems reads the rule's params and reports every problem it
// finds, so Validate can hand them to the API as field-level details and Spec
// can collapse them into one error for the sweep's log line. There is one
// definition of "usable params" rather than two that can drift.
func parseAbsenceProblems(params json.RawMessage) (AbsenceSpec, FieldErrors) {
	var p absenceParams
	if err := json.Unmarshal(params, &p); err != nil {
		return AbsenceSpec{}, FieldErrors{{
			Field:  "params",
			Reason: "absence_of_event: invalid params: " + err.Error(),
		}}
	}

	spec := AbsenceSpec{EventName: strings.TrimSpace(p.EventName)}
	window := strings.TrimSpace(p.Window)

	var problems FieldErrors
	if spec.EventName == "" {
		problems = append(problems, FieldError{
			Field:  "event_name",
			Reason: "absence_of_event: event_name is required (the event that should keep arriving)",
		})
	}
	switch d, err := time.ParseDuration(window); {
	case window == "":
		problems = append(problems, FieldError{
			Field:  "window",
			Reason: `absence_of_event: window is required (a Go duration such as "30m")`,
		})
	case err != nil:
		problems = append(problems, FieldError{
			Field:  "window",
			Reason: fmt.Sprintf(`absence_of_event: window %q is not a duration (want "30m", "2h", ...)`, p.Window),
		})
	case d <= 0:
		problems = append(problems, FieldError{
			Field:  "window",
			Reason: "absence_of_event: window must be greater than zero",
		})
	default:
		spec.Window = d
	}
	return spec, problems
}
