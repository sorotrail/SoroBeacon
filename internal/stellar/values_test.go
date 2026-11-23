package stellar

import (
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failingMarshaler makes json.Marshal fail so the fmt.Sprintf fallback in
// Canon is exercised. String() keeps the fallback output deterministic.
type failingMarshaler struct{}

func (failingMarshaler) MarshalJSON() ([]byte, error) { return nil, errors.New("marshal failed") }
func (failingMarshaler) String() string               { return "failing" }

func TestValuesIsValidContractID(t *testing.T) {
	const valid = "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"

	tests := []struct {
		name string
		in   string
		want bool
	}{
		{name: "valid contract ID", in: valid, want: true},
		{name: "empty", in: "", want: false},
		{name: "too short", in: valid[:len(valid)-1], want: false},
		{name: "too long", in: valid + "A", want: false},
		{name: "wrong strkey prefix", in: "GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA", want: false},
		{name: "lowercase", in: strings.ToLower(valid), want: false},
		{name: "surrounding whitespace", in: " " + valid, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsValidContractID(tt.in))
		})
	}
}

func TestValuesCanon(t *testing.T) {
	huge := new(big.Int)
	huge.SetString("123456789012345678901234567890", 10)

	tests := []struct {
		name string
		in   any
		want string
	}{
		{name: "nil", in: nil, want: ""},
		{name: "string", in: "transfer", want: "transfer"},
		{name: "true", in: true, want: "true"},
		{name: "false", in: false, want: "false"},
		{name: "big int", in: huge, want: "123456789012345678901234567890"},
		{name: "bytes", in: []byte{0xde, 0xad}, want: "3q0="},
		{name: "json number", in: json.Number("12.50"), want: "12.50"},
		{name: "integral float", in: float64(42), want: "42"},
		{name: "fractional float", in: float64(1.5), want: "1.5"},
		{name: "map", in: map[string]any{"b": 2, "a": 1}, want: `{"a":1,"b":2}`},
		{
			name: "nested map and vector",
			in:   map[string]any{"a": []any{map[string]any{"b": true}}},
			want: `{"a":[{"b":true}]}`,
		},
		{name: "marshal fallback", in: failingMarshaler{}, want: "failing"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Canon(tt.in))
		})
	}
}

// TestCanonIsStableAcrossKeyOrder pins the property rules rely on: equal maps
// built in a different insertion order render identically, so they can be
// compared as canonical strings.
func TestValuesCanonIsStableAcrossKeyOrder(t *testing.T) {
	first := map[string]any{"a": 1, "b": map[string]any{"c": 2, "d": 3}}
	second := map[string]any{"b": map[string]any{"d": 3, "c": 2}, "a": 1}

	assert.Equal(t, Canon(first), Canon(second))
}

func TestValuesToBigFloat(t *testing.T) {
	huge := new(big.Int)
	huge.SetString("123456789012345678901234567890", 10)

	tests := []struct {
		name string
		in   any
		want string
		ok   bool
	}{
		{name: "big int", in: huge, want: "123456789012345678901234567890", ok: true},
		{name: "float", in: float64(1.5), want: "1.5", ok: true},
		{name: "int", in: int(7), want: "7", ok: true},
		{name: "int64", in: int64(-9), want: "-9", ok: true},
		{name: "json number", in: json.Number("2.25"), want: "2.25", ok: true},
		{name: "decimal string", in: "3.5", want: "3.5", ok: true},
		{name: "padded string", in: "  4.75  ", want: "4.75", ok: true},
		{name: "exponent string", in: "1e3", want: "1000", ok: true},
		{name: "invalid json number", in: json.Number("12x"), want: "", ok: false},
		{name: "invalid string", in: "not-a-number", want: "", ok: false},
		{name: "bool", in: true, want: "", ok: false},
		{name: "nil", in: nil, want: "", ok: false},
		{name: "bytes", in: []byte("1"), want: "", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ToBigFloat(tt.in)
			require.Equal(t, tt.ok, ok)
			if !ok {
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tt.want, got.Text('f', -1))
		})
	}
}

func TestValuesLookup(t *testing.T) {
	nested := map[string]any{
		"a": map[string]any{
			"b": []any{"x", map[string]any{"c": 3}},
		},
		"flag": true,
	}

	tests := []struct {
		name   string
		in     any
		path   string
		want   any
		wantOK bool
	}{
		{name: "empty path returns the value", in: nested, path: "", want: nested, wantOK: true},
		{name: "present nested path", in: nested, path: "a.b.1.c", want: 3, wantOK: true},
		{name: "slice element", in: nested, path: "a.b.0", want: "x", wantOK: true},
		{name: "top-level key", in: nested, path: "flag", want: true, wantOK: true},
		{name: "missing map key", in: nested, path: "a.missing", want: nil, wantOK: false},
		{name: "path through a scalar", in: nested, path: "flag.x", want: nil, wantOK: false},
		{name: "non-numeric slice index", in: nested, path: "a.b.x", want: nil, wantOK: false},
		{name: "negative slice index", in: nested, path: "a.b.-1", want: nil, wantOK: false},
		{name: "slice index out of range", in: nested, path: "a.b.9", want: nil, wantOK: false},
		{name: "root slice", in: []any{"first"}, path: "0", want: "first", wantOK: true},
		{name: "nil with empty path", in: nil, path: "", want: nil, wantOK: true},
		{name: "nil with path", in: nil, path: "a", want: nil, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Lookup(tt.in, tt.path)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}
