package stellar

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// Decoder turns a raw RPC Event into a DecodedEvent whose topics and value
// use a small vocabulary of Go types:
//
//	nil, bool, string, *big.Int, []byte, []any, map[string]any
//
// Symbols, strings and addresses all decode to string; every integer type
// (u32 through i256) decodes to *big.Int so rules can compare uniformly.
//
// Contributors: to change how events are decoded (e.g. contract-spec-aware
// decoding into named fields), implement this interface and wire it into the
// poller in cmd/sorobeacon.
type Decoder interface {
	DecodeEvent(ev Event) (*DecodedEvent, error)
}

// DefaultDecoder prefers the RPC's xdrFormat:"json" fields when present and
// falls back to decoding base64 XDR ScVals via the Stellar SDK.
type DefaultDecoder struct{}

func (DefaultDecoder) DecodeEvent(ev Event) (*DecodedEvent, error) {
	out := &DecodedEvent{
		ID:             ev.ID,
		ContractID:     ev.ContractID,
		Ledger:         ev.Ledger,
		LedgerClosedAt: ev.LedgerClosedAt,
		TxHash:         ev.TxHash,
	}

	switch {
	case len(ev.TopicJSON) > 0 || len(ev.ValueJSON) > 0:
		for i, raw := range ev.TopicJSON {
			v, err := decodeJSONVal(raw)
			if err != nil {
				return nil, fmt.Errorf("event %s: topic %d: %w", ev.ID, i, err)
			}
			out.Topics = append(out.Topics, v)
		}
		if len(ev.ValueJSON) > 0 {
			v, err := decodeJSONVal(ev.ValueJSON)
			if err != nil {
				return nil, fmt.Errorf("event %s: value: %w", ev.ID, err)
			}
			out.Value = v
		}
	default:
		for i, b64 := range ev.Topic {
			v, err := decodeXDRVal(b64)
			if err != nil {
				return nil, fmt.Errorf("event %s: topic %d: %w", ev.ID, i, err)
			}
			out.Topics = append(out.Topics, v)
		}
		if ev.Value != "" {
			v, err := decodeXDRVal(ev.Value)
			if err != nil {
				return nil, fmt.Errorf("event %s: value: %w", ev.ID, err)
			}
			out.Value = v
		}
	}
	return out, nil
}

// decodeXDRVal decodes one base64 XDR ScVal.
func decodeXDRVal(b64 string) (any, error) {
	var sc xdr.ScVal
	if err := xdr.SafeUnmarshalBase64(b64, &sc); err != nil {
		return nil, fmt.Errorf("unmarshal ScVal: %w", err)
	}
	return scValToAny(sc), nil
}

func scValToAny(sc xdr.ScVal) any {
	switch sc.Type {
	case xdr.ScValTypeScvBool:
		if v, ok := sc.GetB(); ok {
			return v
		}
	case xdr.ScValTypeScvVoid:
		return nil
	case xdr.ScValTypeScvU32:
		if v, ok := sc.GetU32(); ok {
			return new(big.Int).SetUint64(uint64(v))
		}
	case xdr.ScValTypeScvI32:
		if v, ok := sc.GetI32(); ok {
			return big.NewInt(int64(v))
		}
	case xdr.ScValTypeScvU64:
		if v, ok := sc.GetU64(); ok {
			return new(big.Int).SetUint64(uint64(v))
		}
	case xdr.ScValTypeScvI64:
		if v, ok := sc.GetI64(); ok {
			return big.NewInt(int64(v))
		}
	case xdr.ScValTypeScvTimepoint:
		if v, ok := sc.GetTimepoint(); ok {
			return new(big.Int).SetUint64(uint64(v))
		}
	case xdr.ScValTypeScvDuration:
		if v, ok := sc.GetDuration(); ok {
			return new(big.Int).SetUint64(uint64(v))
		}
	case xdr.ScValTypeScvU128:
		if v, ok := sc.GetU128(); ok {
			hi := new(big.Int).SetUint64(uint64(v.Hi))
			lo := new(big.Int).SetUint64(uint64(v.Lo))
			return hi.Lsh(hi, 64).Add(hi, lo)
		}
	case xdr.ScValTypeScvI128:
		if v, ok := sc.GetI128(); ok {
			hi := big.NewInt(int64(v.Hi))
			lo := new(big.Int).SetUint64(uint64(v.Lo))
			return hi.Lsh(hi, 64).Add(hi, lo)
		}
	case xdr.ScValTypeScvU256:
		if v, ok := sc.GetU256(); ok {
			return partsToBig(new(big.Int).SetUint64(uint64(v.HiHi)), uint64(v.HiLo), uint64(v.LoHi), uint64(v.LoLo))
		}
	case xdr.ScValTypeScvI256:
		if v, ok := sc.GetI256(); ok {
			return partsToBig(big.NewInt(int64(v.HiHi)), uint64(v.HiLo), uint64(v.LoHi), uint64(v.LoLo))
		}
	case xdr.ScValTypeScvBytes:
		if v, ok := sc.GetBytes(); ok {
			return []byte(v)
		}
	case xdr.ScValTypeScvString:
		if v, ok := sc.GetStr(); ok {
			return string(v)
		}
	case xdr.ScValTypeScvSymbol:
		if v, ok := sc.GetSym(); ok {
			return string(v)
		}
	case xdr.ScValTypeScvAddress:
		if v, ok := sc.GetAddress(); ok {
			if s, err := v.String(); err == nil {
				return s
			}
		}
	case xdr.ScValTypeScvVec:
		if v, ok := sc.GetVec(); ok && v != nil {
			out := make([]any, 0, len(*v))
			for _, item := range *v {
				out = append(out, scValToAny(item))
			}
			return out
		}
	case xdr.ScValTypeScvMap:
		if v, ok := sc.GetMap(); ok && v != nil {
			out := make(map[string]any, len(*v))
			for _, entry := range *v {
				out[Canon(scValToAny(entry.Key))] = scValToAny(entry.Val)
			}
			return out
		}
	}
	// Errors, contract instances, ledger keys: fall back to the SDK's
	// string rendering rather than dropping the value.
	return sc.String()
}

func partsToBig(hi *big.Int, rest ...uint64) *big.Int {
	out := hi
	for _, part := range rest {
		out.Lsh(out, 64).Add(out, new(big.Int).SetUint64(part))
	}
	return out
}

// decodeJSONVal normalizes one value from the RPC's xdrFormat:"json" output
// (e.g. {"symbol":"transfer"}, {"u32":7}, {"i128":"9000000000000000000000"})
// into the same vocabulary scValToAny produces. Unknown shapes are returned
// as-is rather than rejected, so newer RPC output degrades gracefully.
func decodeJSONVal(raw json.RawMessage) (any, error) {
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wrapper); err != nil || len(wrapper) != 1 {
		// Not a single-key wrapper object; keep the generic decoding.
		return genericJSON(raw)
	}
	for key, inner := range wrapper {
		switch key {
		case "bool":
			var b bool
			if err := json.Unmarshal(inner, &b); err != nil {
				return nil, err
			}
			return b, nil
		case "void":
			return nil, nil
		case "u32", "i32", "u64", "i64", "timepoint", "duration", "u128", "i128", "u256", "i256":
			return jsonToBigInt(inner)
		case "string", "symbol", "address":
			var s string
			if err := json.Unmarshal(inner, &s); err != nil {
				return nil, err
			}
			return s, nil
		case "bytes":
			var s string
			if err := json.Unmarshal(inner, &s); err != nil {
				return nil, err
			}
			if b, err := hex.DecodeString(s); err == nil {
				return b, nil
			}
			return s, nil
		case "vec":
			var items []json.RawMessage
			if err := json.Unmarshal(inner, &items); err != nil {
				return nil, err
			}
			out := make([]any, 0, len(items))
			for _, item := range items {
				v, err := decodeJSONVal(item)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			return out, nil
		case "map":
			var entries []struct {
				Key json.RawMessage `json:"key"`
				Val json.RawMessage `json:"val"`
			}
			if err := json.Unmarshal(inner, &entries); err != nil {
				return nil, err
			}
			out := make(map[string]any, len(entries))
			for _, e := range entries {
				k, err := decodeJSONVal(e.Key)
				if err != nil {
					return nil, err
				}
				v, err := decodeJSONVal(e.Val)
				if err != nil {
					return nil, err
				}
				out[Canon(k)] = v
			}
			return out, nil
		default:
			return genericJSON(raw)
		}
	}
	return genericJSON(raw)
}

// jsonToBigInt accepts a JSON number or a decimal string (the RPC emits
// >64-bit integers as strings).
func jsonToBigInt(raw json.RawMessage) (*big.Int, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		var n json.Number
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("expected integer, got %s", string(raw))
		}
		s = n.String()
	}
	out, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, fmt.Errorf("invalid integer %q", s)
	}
	return out, nil
}

func genericJSON(raw json.RawMessage) (any, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}
