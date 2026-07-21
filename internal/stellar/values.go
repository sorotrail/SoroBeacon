package stellar

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/stellar/go-stellar-sdk/strkey"
)

// IsValidContractID reports whether s is a valid Soroban contract address
// (strkey starting with "C"). The API rejects monitors with invalid IDs and
// the poller skips them, since one bad ID makes the RPC reject the whole
// getEvents request.
func IsValidContractID(s string) bool {
	_, err := strkey.Decode(strkey.VersionByteContract, s)
	return err == nil
}

// Canon renders a decoded value as a canonical string, used for equality
// comparisons in rules and as map keys. Two values that decode differently
// (e.g. json.Number vs *big.Int) but mean the same thing render identically.
func Canon(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case *big.Int:
		return t.String()
	case []byte:
		return base64.StdEncoding.EncodeToString(t)
	case json.Number:
		return t.String()
	case float64:
		// JSON numbers that survived generic decoding; render integers
		// without an exponent or trailing zeros.
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}

// ToBigFloat coerces a decoded value (or a rule-param JSON value) into a
// number for threshold comparisons.
func ToBigFloat(v any) (*big.Float, bool) {
	switch t := v.(type) {
	case *big.Int:
		return new(big.Float).SetInt(t), true
	case float64:
		return big.NewFloat(t), true
	case int:
		return new(big.Float).SetInt64(int64(t)), true
	case int64:
		return new(big.Float).SetInt64(t), true
	case json.Number:
		f, _, err := big.ParseFloat(t.String(), 10, 256, big.ToNearestEven)
		return f, err == nil
	case string:
		f, _, err := big.ParseFloat(strings.TrimSpace(t), 10, 256, big.ToNearestEven)
		return f, err == nil
	default:
		return nil, false
	}
}

// Lookup resolves a dot-separated path into a decoded value: map keys by
// canonical name, slice elements by index. An empty path returns v itself.
func Lookup(v any, path string) (any, bool) {
	if path == "" {
		return v, true
	}
	cur := v
	for _, part := range strings.Split(path, ".") {
		switch t := cur.(type) {
		case map[string]any:
			next, ok := t[part]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(t) {
				return nil, false
			}
			cur = t[i]
		default:
			return nil, false
		}
	}
	return cur, true
}
