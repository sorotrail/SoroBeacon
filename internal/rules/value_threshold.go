package rules

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/sorobeacon/sorobeacon/internal/stellar"
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
	if _, ok := comparisons[p.Comparison]; !ok {
		return fmt.Errorf("value_threshold: invalid comparison %q (want gt|gte|lt|lte|eq|neq)", p.Comparison)
	}
	if threshold == nil {
		return fmt.Errorf("value_threshold: threshold must be a number (or a numeric string)")
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
	raw, found := stellar.Lookup(ev.Value, p.ValuePath)
	if !found {
		return false, nil
	}
	value, ok := stellar.ToBigFloat(raw)
	if !ok {
		return false, nil
	}
	return match(value.Cmp(threshold)), nil
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
