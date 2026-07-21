package stellar

import (
	"encoding/json"
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustB64(t *testing.T, v xdr.ScVal) string {
	t.Helper()
	s, err := xdr.MarshalBase64(v)
	require.NoError(t, err)
	return s
}

func symVal(s string) xdr.ScVal {
	sym := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
}

func i128Val(hi int64, lo uint64) xdr.ScVal {
	parts := xdr.Int128Parts{Hi: xdr.Int64(hi), Lo: xdr.Uint64(lo)}
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &parts}
}

func TestDecodeEventFromXDR(t *testing.T) {
	u32 := xdr.Uint32(7)
	ev := Event{
		ID:         "0000000012884905986-0000000000",
		ContractID: "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA",
		Ledger:     123,
		Topic: []string{
			mustB64(t, symVal("transfer")),
			mustB64(t, xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &u32}),
		},
		Value: mustB64(t, i128Val(1, 5)), // 2^64 + 5
	}

	decoded, err := DefaultDecoder{}.DecodeEvent(ev)
	require.NoError(t, err)
	assert.Equal(t, "transfer", decoded.EventName())
	require.Len(t, decoded.Topics, 2)
	assert.Equal(t, big.NewInt(7), decoded.Topics[1])

	want, _ := new(big.Int).SetString("18446744073709551621", 10) // 2^64 + 5
	assert.Equal(t, want, decoded.Value)
}

func TestDecodeEventPrefersJSON(t *testing.T) {
	ev := Event{
		ID: "e1",
		TopicJSON: []json.RawMessage{
			json.RawMessage(`{"symbol": "transfer"}`),
			json.RawMessage(`{"address": "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF"}`),
		},
		ValueJSON: json.RawMessage(`{"map": [{"key": {"symbol": "amount"}, "val": {"i128": "170141183460469231731687303715884105727"}}]}`),
		// Base64 fields present too; JSON must win.
		Topic: []string{"ignored"},
	}
	decoded, err := DefaultDecoder{}.DecodeEvent(ev)
	require.NoError(t, err)
	assert.Equal(t, "transfer", decoded.EventName())

	m, ok := decoded.Value.(map[string]any)
	require.True(t, ok)
	amount, ok := m["amount"].(*big.Int)
	require.True(t, ok)
	assert.Equal(t, "170141183460469231731687303715884105727", amount.String())
}

func TestDecodeJSONValShapes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want any
	}{
		{"bool", `{"bool": true}`, true},
		{"void", `{"void": null}`, nil},
		{"u32 number", `{"u32": 42}`, big.NewInt(42)},
		{"i128 string", `{"i128": "-5"}`, big.NewInt(-5)},
		{"string", `{"string": "hello"}`, "hello"},
		{"symbol", `{"symbol": "transfer"}`, "transfer"},
		{"vec", `{"vec": [{"u32": 1}, {"symbol": "two"}]}`, []any{big.NewInt(1), "two"}},
		{"unknown wrapper passes through", `{"contract_instance": {"executable": "x"}}`,
			map[string]any{"contract_instance": map[string]any{"executable": "x"}}},
		{"non-wrapper passes through", `"plain"`, "plain"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeJSONVal(json.RawMessage(tt.in))
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCanon(t *testing.T) {
	assert.Equal(t, "42", Canon(big.NewInt(42)))
	assert.Equal(t, "42", Canon(float64(42)))
	assert.Equal(t, "42", Canon(json.Number("42")))
	assert.Equal(t, "transfer", Canon("transfer"))
	assert.Equal(t, "true", Canon(true))
	assert.Equal(t, "", Canon(nil))
	// Equal values from different decode paths must render identically.
	assert.Equal(t, Canon(big.NewInt(7)), Canon(float64(7)))
}

func TestToBigFloat(t *testing.T) {
	f, ok := ToBigFloat(big.NewInt(10))
	require.True(t, ok)
	assert.Equal(t, 0, f.Cmp(big.NewFloat(10)))

	f, ok = ToBigFloat("123.5")
	require.True(t, ok)
	assert.Equal(t, 0, f.Cmp(big.NewFloat(123.5)))

	_, ok = ToBigFloat("not a number")
	assert.False(t, ok)
	_, ok = ToBigFloat([]any{})
	assert.False(t, ok)
}

func TestLookup(t *testing.T) {
	v := map[string]any{
		"amount": big.NewInt(5),
		"nested": map[string]any{"deep": []any{"zero", "one"}},
	}
	got, ok := Lookup(v, "amount")
	require.True(t, ok)
	assert.Equal(t, big.NewInt(5), got)

	got, ok = Lookup(v, "nested.deep.1")
	require.True(t, ok)
	assert.Equal(t, "one", got)

	_, ok = Lookup(v, "missing")
	assert.False(t, ok)
	_, ok = Lookup(v, "nested.deep.9")
	assert.False(t, ok)

	got, ok = Lookup(v, "")
	require.True(t, ok)
	assert.Equal(t, v, got)
}
