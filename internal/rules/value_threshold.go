package rules

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// valueThresholdParams configure the value_threshold rule.
//
//	{
//	  "event_name": "transfer",   // optional: only consider these events
//	  "value_path": "amount",     // optional: dot path into the event value
//	  "comparison": "gt",         // gt | gte | lt | lte | eq | neq
//	  "threshold": "1000000000"   // number, or string for >53-bit integers
//	}
type valueThresholdParams struct {
	EventName  string          `json:"event_name"`
	ValuePath  string          `json:"value_path"`
	Comparison string          `json:"comparison"`
	Threshold  json.RawMessage `json:"threshold"`
}

var comparisons = map[string]func(cmp int) bool{
	"gt":  func(c int) bool { return c > 0 },
	"gte": func(c int) bool { return c >= 0 },
	"lt":  func(c int) bool { return c < 0 },
	"lte": func(c int) bool { return c <= 0 },
	"eq":  func(c int) bool { return c == 0 },
	"neq": func(c int) bool { return c != 0 },
}

// ValueThreshold matches when a numeric value in the event's data crosses a
// threshold. Events whose value (or value_path) is missing or non-numeric
// simply don't match; they are not errors.
type ValueThreshold struct{}

func (ValueThreshold) Validate(params json.RawMessage) error {
	p, threshold, err := parseValueThreshold(params)
	if err != nil {
		return err
	}
	var details FieldErrors
	if _, ok := comparisons[p.Comparison]; !ok {
		details = append(details, FieldError{
			Field:  "comparison",
			Reason: fmt.Sprintf("value_threshold: invalid comparison %q (want gt|gte|lt|lte|eq|neq)", p.Comparison),
		})
	}
	if threshold == nil {
		details = append(details, FieldError{
			Field:  "threshold",
			Reason: "value_threshold: threshold must be a number (or a numeric string)",
		})
	}
	if len(details) > 0 {
		return details
	}
	return nil
}

func (ValueThreshold) Evaluate(_ context.Context, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error) {
	p, threshold, err := parseValueThreshold(params)
	if err != nil {
		return false, err
	}
	match, ok := comparisons[p.Comparison]
	if !ok || threshold == nil {
		return false, fmt.Errorf("value_threshold: invalid params: comparison=%q threshold=%s", p.Comparison, p.Threshold)
	}

	if p.EventName != "" && ev.EventName() != p.EventName {
		return false, nil
	}
	// When the contract's spec was available, the event carries named fields
	// and value_path addresses those first; otherwise it keeps addressing the
	// raw positional value, exactly as before. An empty value_path, or a path
	// the spec does not name, falls back to the raw value so a rule that
	// omits value_path (documented as valid when the value itself is the
	// number) keeps matching events from a contract that now has a spec.
	var raw any
	var found bool
	if ev.Fields != nil && p.ValuePath != "" {
		raw, found = stellar.Lookup(ev.Fields, p.ValuePath)
	}
	if !found {
		raw, found = stellar.Lookup(ev.Value, p.ValuePath)
	}
	if !found {
		return false, nil
	}
	value, ok := stellar.ToBigFloat(raw)
	if !ok {
		return false, nil
	}
	return match(value.Cmp(threshold)), nil
}

// EventNames reports the event name this rule is scoped to. value_threshold
// may omit event_name, in which case it judges every event's value and cannot
// be narrowed server-side.
func (ValueThreshold) EventNames(params json.RawMessage) ([]string, bool) {
	p, _, err := parseValueThreshold(params)
	if err != nil || p.EventName == "" {
		return nil, false
	}
	return []string{p.EventName}, true
}

func parseValueThreshold(params json.RawMessage) (valueThresholdParams, *big.Float, error) {
	var p valueThresholdParams
	if err := json.Unmarshal(params, &p); err != nil {
		return p, nil, fmt.Errorf("value_threshold: invalid params: %w", err)
	}
	if len(p.Threshold) == 0 {
		return p, nil, nil
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(p.Threshold))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return p, nil, fmt.Errorf("value_threshold: invalid threshold: %w", err)
	}
	f, ok := stellar.ToBigFloat(v)
	if !ok {
		return p, nil, nil
	}
	return p, f, nil
}
