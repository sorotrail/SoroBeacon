package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// timeWindowParams configure the time_window rule:
//
//	{
//	  "start": "09:00",                              // required, HH:MM UTC
//	  "end": "17:00",                                // required, HH:MM UTC
//	  "days": ["mon", "tue", "wed", "thu", "fri"],   // optional, default every day
//	  "outside": true                                // optional, default false
//	}
//
// With outside:true the rule matches events *not* in the window — the
// common case, "page me only when this happens outside business hours".
// A window whose end is earlier than its start (22:00-06:00) crosses
// midnight. The window is half-open: start is inside it, end is not.
//
// Times are compared against the event's ledger close time, not wall-clock
// now, so replayed or backfilled events evaluate against when they actually
// happened. Everything is UTC; per-monitor timezones are deliberately out
// of scope.
type timeWindowParams struct {
	Start   string   `json:"start"`
	End     string   `json:"end"`
	Days    []string `json:"days"`
	Outside bool     `json:"outside"`
}

// weekdays maps the accepted day names to Go weekdays.
var weekdays = map[string]time.Weekday{
	"sun": time.Sunday,
	"mon": time.Monday,
	"tue": time.Tuesday,
	"wed": time.Wednesday,
	"thu": time.Thursday,
	"fri": time.Friday,
	"sat": time.Saturday,
}

// TypeTimeWindow is the registry name of the time_window rule.
const TypeTimeWindow = "time_window"

// TimeWindow matches when an event's ledger close time falls inside (or,
// with outside:true, outside) a recurring UTC window.
type TimeWindow struct{}

func (TimeWindow) Validate(params json.RawMessage) error {
	p, err := parseTimeWindow(params)
	if err != nil {
		return err
	}
	var details FieldErrors
	if p.Start == "" {
		details = append(details, FieldError{Field: "start", Reason: "time_window: start is required (HH:MM)"})
	} else if _, ok := parseClock(p.Start); !ok {
		details = append(details, FieldError{
			Field:  "start",
			Reason: fmt.Sprintf("time_window: start %q is not a valid HH:MM time", p.Start),
		})
	}
	if p.End == "" {
		details = append(details, FieldError{Field: "end", Reason: "time_window: end is required (HH:MM)"})
	} else if _, ok := parseClock(p.End); !ok {
		details = append(details, FieldError{
			Field:  "end",
			Reason: fmt.Sprintf("time_window: end %q is not a valid HH:MM time", p.End),
		})
	}
	// An empty days array is almost certainly a mistake: it would mean no
	// day matches, so nothing ever fires. Omit the key for "every day".
	if p.Days != nil && len(p.Days) == 0 {
		details = append(details, FieldError{
			Field:  "days",
			Reason: "time_window: days must not be empty; omit it to match every day",
		})
	}
	seen := map[string]bool{}
	for _, d := range p.Days {
		key := strings.ToLower(d)
		if _, ok := weekdays[key]; !ok {
			if !seen[key] {
				seen[key] = true
				details = append(details, FieldError{
					Field:  "days",
					Reason: fmt.Sprintf("time_window: unknown day %q (want mon|tue|wed|thu|fri|sat|sun)", d),
				})
			}
		}
	}
	if len(details) > 0 {
		return details
	}
	return nil
}

func (TimeWindow) Evaluate(_ context.Context, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error) {
	p, err := parseTimeWindow(params)
	if err != nil {
		return false, err
	}
	start, ok := parseClock(p.Start)
	if !ok {
		return false, fmt.Errorf("time_window: invalid params: start=%q", p.Start)
	}
	end, ok := parseClock(p.End)
	if !ok {
		return false, fmt.Errorf("time_window: invalid params: end=%q", p.End)
	}
	// Events without a known close time (synthetic or malformed) cannot be
	// placed in a window; treat them as non-matches rather than guessing.
	if ev.LedgerClosedAt.IsZero() {
		return false, nil
	}
	utc := ev.LedgerClosedAt.UTC()
	minutes := utc.Hour()*60 + utc.Minute()

	var inTime bool
	if start <= end {
		inTime = minutes >= start && minutes < end
	} else {
		// Crosses midnight, e.g. 22:00-06:00.
		inTime = minutes >= start || minutes < end
	}

	inDay := true
	if len(p.Days) > 0 {
		inDay = false
		wd := utc.Weekday()
		for _, d := range p.Days {
			if weekdays[strings.ToLower(d)] == wd {
				inDay = true
				break
			}
		}
	}

	in := inTime && inDay
	if p.Outside {
		return !in, nil
	}
	return in, nil
}

// parseClock parses a strict HH:MM string into minutes since midnight.
// time.Parse's layouts would accept "9:00" and other loose forms; a
// malformed window should be rejected at create time, not silently
// reinterpreted.
func parseClock(s string) (int, bool) {
	if len(s) != 5 || s[2] != ':' {
		return 0, false
	}
	for i, c := range []byte(s) {
		if i == 2 {
			continue
		}
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	h, _ := strconv.Atoi(s[:2])
	m, _ := strconv.Atoi(s[3:])
	if h > 23 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

func parseTimeWindow(params json.RawMessage) (timeWindowParams, error) {
	var p timeWindowParams
	if err := json.Unmarshal(params, &p); err != nil {
		return p, fmt.Errorf("time_window: invalid params: %w", err)
	}
	return p, nil
}
