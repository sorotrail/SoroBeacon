package stellar

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- fixture builders -------------------------------------------------------

func topicParam(name string) xdr.ScSpecEventParamV0 {
	return xdr.ScSpecEventParamV0{
		Name:     name,
		Type:     xdr.ScSpecTypeDef{Type: xdr.ScSpecTypeScSpecTypeAddress},
		Location: xdr.ScSpecEventParamLocationV0ScSpecEventParamLocationTopicList,
	}
}

func dataParam(name string) xdr.ScSpecEventParamV0 {
	return xdr.ScSpecEventParamV0{
		Name:     name,
		Type:     xdr.ScSpecTypeDef{Type: xdr.ScSpecTypeScSpecTypeI128},
		Location: xdr.ScSpecEventParamLocationV0ScSpecEventParamLocationData,
	}
}

func eventEntry(t *testing.T, prefix []string, params []xdr.ScSpecEventParamV0, format xdr.ScSpecEventDataFormat) xdr.ScSpecEntry {
	t.Helper()
	prefixTopics := make([]xdr.ScSymbol, len(prefix))
	for i, p := range prefix {
		prefixTopics[i] = xdr.ScSymbol(p)
	}
	entry, err := xdr.NewScSpecEntry(xdr.ScSpecEntryKindScSpecEntryEventV0, xdr.ScSpecEventV0{
		Name:         xdr.ScSymbol("Transfer"),
		PrefixTopics: prefixTopics,
		Params:       params,
		DataFormat:   format,
	})
	require.NoError(t, err)
	return entry
}

// transferSpec is the SEP-48 example: from/to in the topic list and amount in a
// map-shaped data value.
func transferSpec(t *testing.T) *ContractSpec {
	t.Helper()
	return ParseContractSpec([]xdr.ScSpecEntry{
		eventEntry(t, []string{"transfer"}, []xdr.ScSpecEventParamV0{
			topicParam("from"), topicParam("to"), dataParam("amount"),
		}, xdr.ScSpecEventDataFormatScSpecEventDataFormatMap),
	})
}

func TestParseContractSpecIgnoresNonEvents(t *testing.T) {
	// A function entry before the event must not stop event parsing.
	funcEntry, err := xdr.NewScSpecEntry(xdr.ScSpecEntryKindScSpecEntryFunctionV0, xdr.ScSpecFunctionV0{
		Name: xdr.ScSymbol("transfer"),
	})
	require.NoError(t, err)
	spec := ParseContractSpec([]xdr.ScSpecEntry{
		funcEntry,
		eventEntry(t, []string{"transfer"}, []xdr.ScSpecEventParamV0{dataParam("amount")}, xdr.ScSpecEventDataFormatScSpecEventDataFormatSingleValue),
	})
	require.NotNil(t, spec)
	require.Len(t, spec.Events, 1)
	assert.Equal(t, "Transfer", spec.Events[0].Name)
	assert.Equal(t, []string{"transfer"}, spec.Events[0].PrefixTopics)
}

func TestParseContractSpecNoEvents(t *testing.T) {
	assert.Nil(t, ParseContractSpec(nil))
	entry, err := xdr.NewScSpecEntry(xdr.ScSpecEntryKindScSpecEntryFunctionV0, xdr.ScSpecFunctionV0{Name: xdr.ScSymbol("f")})
	require.NoError(t, err)
	assert.Nil(t, ParseContractSpec([]xdr.ScSpecEntry{entry}))
}

func TestFillFieldsNamedDecoding(t *testing.T) {
	spec := transferSpec(t)
	ev := &DecodedEvent{
		Topics: []any{"transfer", "GFROM", "GTO"},
		Value:  map[string]any{"amount": big.NewInt(42)},
	}
	require.True(t, spec.FillFields(ev))
	assert.Equal(t, map[string]any{
		"from":   "GFROM",
		"to":     "GTO",
		"amount": big.NewInt(42),
	}, ev.Fields)
	// The positional decoding is left untouched so existing rules still work.
	assert.Equal(t, []any{"transfer", "GFROM", "GTO"}, ev.Topics)
	assert.Equal(t, map[string]any{"amount": big.NewInt(42)}, ev.Value)
}

func TestFillFieldsSingleValue(t *testing.T) {
	spec := ParseContractSpec([]xdr.ScSpecEntry{
		eventEntry(t, []string{"mint"}, []xdr.ScSpecEventParamV0{
			topicParam("admin"), dataParam("amount"),
		}, xdr.ScSpecEventDataFormatScSpecEventDataFormatSingleValue),
	})
	ev := &DecodedEvent{Topics: []any{"mint", "GADMIN"}, Value: big.NewInt(7)}
	require.True(t, spec.FillFields(ev))
	assert.Equal(t, map[string]any{"admin": "GADMIN", "amount": big.NewInt(7)}, ev.Fields)
}

func TestFillFieldsVec(t *testing.T) {
	spec := ParseContractSpec([]xdr.ScSpecEntry{
		eventEntry(t, []string{"swap"}, []xdr.ScSpecEventParamV0{
			dataParam("amount_in"), dataParam("amount_out"),
		}, xdr.ScSpecEventDataFormatScSpecEventDataFormatVec),
	})
	ev := &DecodedEvent{Topics: []any{"swap"}, Value: []any{big.NewInt(1), big.NewInt(2)}}
	require.True(t, spec.FillFields(ev))
	assert.Equal(t, map[string]any{"amount_in": big.NewInt(1), "amount_out": big.NewInt(2)}, ev.Fields)
}

func TestFillFieldsMismatchFallsBack(t *testing.T) {
	spec := transferSpec(t)
	tests := []struct {
		name string
		ev   *DecodedEvent
	}{
		{"unknown event name", &DecodedEvent{Topics: []any{"mint", "a", "b"}, Value: map[string]any{"amount": big.NewInt(1)}}},
		{"extra topic", &DecodedEvent{Topics: []any{"transfer", "a", "b", "c"}, Value: map[string]any{"amount": big.NewInt(1)}}},
		{"missing topic", &DecodedEvent{Topics: []any{"transfer", "a"}, Value: map[string]any{"amount": big.NewInt(1)}}},
		{"wrong data shape", &DecodedEvent{Topics: []any{"transfer", "a", "b"}, Value: big.NewInt(1)}},
		{"missing data param", &DecodedEvent{Topics: []any{"transfer", "a", "b"}, Value: map[string]any{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := tt.ev.Value
			assert.False(t, spec.FillFields(tt.ev))
			assert.Nil(t, tt.ev.Fields)
			assert.Equal(t, before, tt.ev.Value)
		})
	}
}

func TestTopicTextToleratesStringAndSymbol(t *testing.T) {
	spec := transferSpec(t)
	// The RPC's json path yields bare strings; the XDR path could too. The
	// wrapper shape appears from other sources (e.g. SoroTrail), so both must
	// match the spec's symbol prefix.
	ev := &DecodedEvent{
		Topics: []any{map[string]any{"symbol": "transfer"}, "a", "b"},
		Value:  map[string]any{"amount": big.NewInt(1)},
	}
	require.True(t, spec.FillFields(ev))
	assert.Equal(t, big.NewInt(1), ev.Fields["amount"])
}

// uleb128 encodes an unsigned LEB128 integer, as Wasm uses for lengths.
func uleb128(n int) []byte {
	var out []byte
	for {
		b := byte(n & 0x7f)
		n >>= 7
		if n != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if n == 0 {
			return out
		}
	}
}

// wasmWithSpec builds a minimal Wasm module whose contractspecv0 custom section
// carries the given XDR entries.
func wasmWithSpec(t *testing.T, entries []xdr.ScSpecEntry) []byte {
	t.Helper()
	var content bytes.Buffer
	for _, e := range entries {
		_, err := xdr.Marshal(&content, e)
		require.NoError(t, err)
	}
	name := []byte("contractspecv0")
	payload := append(uleb128(len(name)), name...)
	payload = append(payload, content.Bytes()...)

	wasm := []byte("\x00asm\x01\x00\x00\x00")
	wasm = append(wasm, 0x00) // custom section id
	wasm = append(wasm, uleb128(len(payload))...)
	wasm = append(wasm, payload...)
	return wasm
}

func TestSpecEntriesFromWasm(t *testing.T) {
	wasm := wasmWithSpec(t, []xdr.ScSpecEntry{
		eventEntry(t, []string{"transfer"}, []xdr.ScSpecEventParamV0{topicParam("from"), dataParam("amount")}, xdr.ScSpecEventDataFormatScSpecEventDataFormatMap),
	})
	entries, err := specEntriesFromWasm(wasm)
	require.NoError(t, err)
	spec := ParseContractSpec(entries)
	require.NotNil(t, spec)
	require.Len(t, spec.Events, 1)
	assert.Equal(t, []string{"transfer"}, spec.Events[0].PrefixTopics)
}

func TestSpecEntriesFromWasmNoSpecSection(t *testing.T) {
	// A valid module with an unrelated custom section yields no entries.
	name := []byte("contractmetav0")
	payload := append(uleb128(len(name)), name...)
	wasm := []byte("\x00asm\x01\x00\x00\x00")
	wasm = append(wasm, 0x00)
	wasm = append(wasm, uleb128(len(payload))...)
	wasm = append(wasm, payload...)

	entries, err := specEntriesFromWasm(wasm)
	require.NoError(t, err)
	assert.Empty(t, entries)
	assert.Nil(t, ParseContractSpec(entries))
}

func TestSpecEntriesFromWasmRejectsNonWasm(t *testing.T) {
	_, err := specEntriesFromWasm([]byte("not wasm"))
	require.Error(t, err)
}
