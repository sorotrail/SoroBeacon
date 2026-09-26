package rules

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strings"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// numericRangeParams configure the numeric_range rule.
//
//	{
//	  "min": "1000000",   // optional: decimal integer string, at least one of min/max required
//	  "max": "10000000",  // optional: decimal integer string
//	  "inclusive": true,  // optional: boundary values match (default true)
//	  "outside": false,   // optional: invert the match (default false)
//	  "event_name": "transfer", // optional: only consider events with this name
//	  "value_path": "amount"    // optional: dot path into the event value
//	}
//
// A range expresses in one rule what value_threshold needs two rules for:
// "transfers between 1k and 10k", the structuring band people actually look
// for. With outside:true the same rule expresses exclusion ("anything but
// the dust range") without a second rule and a client-side negation.
//
// Bounds are decimal integer strings because Soroban i128 amounts exceed
// int64; comparison is math/big.Int end to end so wide values stay exact
// and never pass through float64.
type numericRangeParams struct {
	Min       json.RawMessage `json:"min"`
	Max       json.RawMessage `json:"max"`
	Inclusive *bool           `json:"inclusive"`
	Outside   bool            `json:"outside"`
	EventName string          `json:"event_name"`
	ValuePath string          `json:"value_path"`
}

// NumericRange matches when the event's numeric value falls inside (or, with
// outside:true, outside) a configured range. Events whose value is missing
// or non-numeric simply don't match; they are not errors.
type NumericRange struct{}

func (NumericRange) Validate(params json.RawMessage) error {
	p, min, minPresent, minErr, max, maxPresent, maxErr, err := parseNumericRange(params)
	if err != nil {
		return err
	}
	var details FieldErrors
	if minErr != nil {
		details = append(details, FieldError{
			Field:  "min",
			Reason: fmt.Sprintf("numeric_range: min %s is not a decimal integer", compactJSON(p.Min)),
		})
	}
	if maxErr != nil {
		details = append(details, FieldError{
			Field:  "max",
			Reason: fmt.Sprintf("numeric_range: max %s is not a decimal integer", compactJSON(p.Max)),
		})
	}
	if !minPresent && !maxPresent && minErr == nil && maxErr == nil {
		details = append(details, FieldError{
			Field:  "min",
			Reason: "numeric_range: at least one of min or max is required",
		})
	}
	if minPresent && maxPresent && minErr == nil && maxErr == nil && min.Cmp(max) > 0 {
		details = append(details, FieldError{
			Field:  "min",
			Reason: fmt.Sprintf("numeric_range: min %q is greater than max %q", min.String(), max.String()),
		})
	}
	if len(details) > 0 {
		return details
	}
	return nil
}

func (NumericRange) Evaluate(_ context.Context, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error) {
	p, min, minPresent, minErr, max, maxPresent, maxErr, err := parseNumericRange(params)
	if err != nil {
		return false, err
	}
	if minErr != nil || maxErr != nil || (!minPresent && !maxPresent) {
		return false, fmt.Errorf("numeric_range: invalid params: min=%s max=%s", compactJSON(p.Min), compactJSON(p.Max))
	}
	if minPresent && maxPresent && min.Cmp(max) > 0 {
		return false, fmt.Errorf("numeric_range: invalid params: min %q is greater than max %q", min.String(), max.String())
	}

	if p.EventName != "" && ev.EventName() != p.EventName {
		return false, nil
	}
	// Like value_threshold, value_path addresses the spec's named fields
	// first and falls back to the raw positional value, so a rule written
	// before the contract gained a spec keeps matching. An empty path
	// addresses the value itself.
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
	value, ok := eventToBigInt(raw)
	if !ok {
		return false, nil
	}

	inclusive := true
	if p.Inclusive != nil {
		inclusive = *p.Inclusive
	}
	inside := inRange(value, min, minPresent, max, maxPresent, inclusive)
	if p.Outside {
		return !inside, nil
	}
	return inside, nil
}

// inRange reports whether value lies within the configured bounds. A nil
// bound leaves that side open; inclusive controls whether the boundary
// values themselves count as inside.
func inRange(value, min *big.Int, hasMin bool, max *big.Int, hasMax bool, inclusive bool) bool {
	if hasMin {
		if c := value.Cmp(min); inclusive && c < 0 || !inclusive && c <= 0 {
			return false
		}
	}
	if hasMax {
		if c := value.Cmp(max); inclusive && c > 0 || !inclusive && c >= 0 {
			return false
		}
	}
	return true
}

func parseNumericRange(params json.RawMessage) (numericRangeParams, *big.Int, bool, error, *big.Int, bool, error, error) {
	var p numericRangeParams
	if err := json.Unmarshal(params, &p); err != nil {
		return p, nil, false, nil, nil, false, nil, fmt.Errorf("numeric_range: invalid params: %w", err)
	}
	min, minPresent, minErr := parseBound(p.Min)
	max, maxPresent, maxErr := parseBound(p.Max)
	return p, min, minPresent, minErr, max, maxPresent, maxErr, nil
}

// parseBound parses one range bound. Missing (absent or null) reports
// present=false; a present but non-integer value reports an error so
// Validate can point at the offending field and Evaluate can fail the
// params rather than the event.
func parseBound(raw json.RawMessage) (*big.Int, bool, error) {
	if len(bytes.TrimSpace(raw)) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, false, nil
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, true, err
	}
	n, ok := boundToBigInt(v)
	if !ok {
		return nil, true, fmt.Errorf("not a decimal integer: %s", strings.TrimSpace(string(raw)))
	}
	return n, true, nil
}

// boundToBigInt coerces a bound JSON value (string or number) to a big.Int.
// Strings carry wide i128 values exactly; JSON numbers are accepted when
// they spell an integer. Anything else (booleans, floats, objects) is not
// a bound.
func boundToBigInt(v any) (*big.Int, bool) {
	switch t := v.(type) {
	case string:
		return parseDecimalInt(strings.TrimSpace(t))
	case json.Number:
		return parseDecimalInt(strings.TrimSpace(t.String()))
	case *big.Int:
		return new(big.Int).Set(t), true
	case int:
		return big.NewInt(int64(t)), true
	case int64:
		return big.NewInt(t), true
	case float64:
		if math.Trunc(t) != t {
			return nil, false
		}
		return new(big.Int).SetInt64(int64(t)), true
	default:
		return nil, false
	}
}

func parseDecimalInt(s string) (*big.Int, bool) {
	if s == "" {
		return nil, false
	}
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, false
	}
	return n, true
}

// eventToBigInt coerces a decoded event value to a big.Int without passing
// through float64, so i128 amounts stay exact. The decoder vocabulary is
// *big.Int for every integer type; the RPC's xdrFormat:"json" path can also
// surface single-key wrappers ({"i128":"..."}) and generic JSON numbers,
// all handled here. Anything non-numeric reports false so the caller treats
// the event as a non-match rather than an error.
func eventToBigInt(v any) (*big.Int, bool) {
	switch t := v.(type) {
	case *big.Int:
		return t, true
	case big.Int:
		c := new(big.Int).Set(&t)
		return c, true
	case int:
		return big.NewInt(int64(t)), true
	case int8:
		return big.NewInt(int64(t)), true
	case int16:
		return big.NewInt(int64(t)), true
	case int32:
		return big.NewInt(int64(t)), true
	case int64:
		return big.NewInt(t), true
	case uint:
		return new(big.Int).SetUint64(uint64(t)), true
	case uint8:
		return new(big.Int).SetUint64(uint64(t)), true
	case uint16:
		return new(big.Int).SetUint64(uint64(t)), true
	case uint32:
		return new(big.Int).SetUint64(uint64(t)), true
	case uint64:
		return new(big.Int).SetUint64(t), true
	case string:
		return parseDecimalInt(strings.TrimSpace(t))
	case json.Number:
		return parseDecimalInt(strings.TrimSpace(t.String()))
	case float64:
		// Generic JSON decoding yields float64; only integral values can
		// be compared exactly as integers.
		if math.Trunc(t) != t {
			return nil, false
		}
		return big.NewInt(int64(t)), true
	case map[string]any:
		// Single-key integer wrappers from the RPC JSON path
		// ({"i128":"..."}, {"u64":7}, ...) decode to the integer inside.
		for _, key := range []string{"i128", "u128", "i256", "u256", "i64", "u64", "i32", "u32"} {
			if inner, ok := t[key]; ok {
				return eventToBigInt(inner)
			}
		}
		return nil, false
	default:
		return nil, false
	}
}

func compactJSON(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return ""
	}
	return s
}
