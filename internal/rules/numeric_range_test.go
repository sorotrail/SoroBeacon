package rules

import (
	"context"
	"encoding/json"
	"math/big"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

func numericRangeEvent(v any) *stellar.DecodedEvent {
	return &stellar.DecodedEvent{
		ID:         "0000000012884905986-0000000000",
		ContractID: "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA",
		Topics:     []any{"transfer"},
		Value:      v,
	}
}

func TestNumericRangeEvaluate(t *testing.T) {
	bigInside := mustBig("170141183460469231731687303715884105726")
	bigMin := `"170141183460469231731687303715884105725"`
	bigMax := `"170141183460469231731687303715884105727"`
	aboveInt64 := mustBig("9223372036854775808")

	tests := []struct {
		name    string
		params  string
		event   *stellar.DecodedEvent
		want    bool
		wantErr bool
	}{
		{
			name:   "inside the range matches",
			params: `{"min": "1000000", "max": "10000000"}`,
			event:  numericRangeEvent(big.NewInt(5000000)),
			want:   true,
		},
		{
			name:   "below the range does not match",
			params: `{"min": "1000000", "max": "10000000"}`,
			event:  numericRangeEvent(big.NewInt(999999)),
			want:   false,
		},
		{
			name:   "above the range does not match",
			params: `{"min": "1000000", "max": "10000000"}`,
			event:  numericRangeEvent(big.NewInt(10000001)),
			want:   false,
		},
		{
			name:   "lower boundary matches when inclusive",
			params: `{"min": "1000000", "max": "10000000", "inclusive": true}`,
			event:  numericRangeEvent(big.NewInt(1000000)),
			want:   true,
		},
		{
			name:   "upper boundary matches when inclusive",
			params: `{"min": "1000000", "max": "10000000", "inclusive": true}`,
			event:  numericRangeEvent(big.NewInt(10000000)),
			want:   true,
		},
		{
			name:   "lower boundary does not match when exclusive",
			params: `{"min": "1000000", "max": "10000000", "inclusive": false}`,
			event:  numericRangeEvent(big.NewInt(1000000)),
			want:   false,
		},
		{
			name:   "upper boundary does not match when exclusive",
			params: `{"min": "1000000", "max": "10000000", "inclusive": false}`,
			event:  numericRangeEvent(big.NewInt(10000000)),
			want:   false,
		},
		{
			name:   "interior still matches when exclusive",
			params: `{"min": "1000000", "max": "10000000", "inclusive": false}`,
			event:  numericRangeEvent(big.NewInt(5000000)),
			want:   true,
		},
		{
			name:   "inclusive defaults to true",
			params: `{"min": "1000000", "max": "10000000"}`,
			event:  numericRangeEvent(big.NewInt(1000000)),
			want:   true,
		},
		{
			name:   "outside inverts an interior non-match into a match",
			params: `{"min": "1000000", "max": "10000000", "outside": true}`,
			event:  numericRangeEvent(big.NewInt(5)),
			want:   true,
		},
		{
			name:   "outside inverts an interior match into a non-match",
			params: `{"min": "1000000", "max": "10000000", "outside": true}`,
			event:  numericRangeEvent(big.NewInt(5000000)),
			want:   false,
		},
		{
			name:   "outside defaults to false",
			params: `{"min": "1000000", "max": "10000000"}`,
			event:  numericRangeEvent(big.NewInt(5)),
			want:   false,
		},
		{
			name:   "open-ended min only matches above",
			params: `{"min": "1000000"}`,
			event:  numericRangeEvent(big.NewInt(2000000)),
			want:   true,
		},
		{
			name:   "open-ended min only rejects below",
			params: `{"min": "1000000"}`,
			event:  numericRangeEvent(big.NewInt(999999)),
			want:   false,
		},
		{
			name:   "open-ended max only matches below",
			params: `{"max": "10000000"}`,
			event:  numericRangeEvent(big.NewInt(5)),
			want:   true,
		},
		{
			name:   "open-ended max only rejects above",
			params: `{"max": "10000000"}`,
			event:  numericRangeEvent(big.NewInt(10000001)),
			want:   false,
		},
		{
			name:   "non-numeric string value is a non-match",
			params: `{"min": "1000000", "max": "10000000"}`,
			event:  numericRangeEvent("not a number"),
			want:   false,
		},
		{
			name:   "missing value is a non-match",
			params: `{"min": "1000000", "max": "10000000"}`,
			event:  numericRangeEvent(nil),
			want:   false,
		},
		{
			name:   "map value without a numeric wrapper is a non-match",
			params: `{"min": "1000000", "max": "10000000"}`,
			event:  numericRangeEvent(map[string]any{"memo": "hi"}),
			want:   false,
		},
		{
			name:   "value larger than int64 compares exactly",
			params: `{"min": "9223372036854775807", "max": "9223372036854775809"}`,
			event:  numericRangeEvent(aboveInt64),
			want:   true,
		},
		{
			name:   "i128 wider than 64 bits compares exactly",
			params: `{"min": ` + bigMin + `, "max": ` + bigMax + `}`,
			event:  numericRangeEvent(bigInside),
			want:   true,
		},
		{
			name:   "i128 wrapper value decodes without float64",
			params: `{"min": "1000000", "max": "10000000"}`,
			event:  numericRangeEvent(map[string]any{"i128": "5000000"}),
			want:   true,
		},
		{
			name:    "invalid params error",
			params:  `{"min": "abc", "max": "10"}`,
			event:   numericRangeEvent(big.NewInt(5)),
			wantErr: true,
		},
		{
			name:    "missing bounds error",
			params:  `{}`,
			event:   numericRangeEvent(big.NewInt(5)),
			wantErr: true,
		},
		{
			name:    "min greater than max errors",
			params:  `{"min": "10", "max": "5"}`,
			event:   numericRangeEvent(big.NewInt(7)),
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NumericRange{}.Evaluate(context.Background(), tt.event, json.RawMessage(tt.params))
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNumericRangeValidate(t *testing.T) {
	v := NumericRange{}
	assert.NoError(t, v.Validate(json.RawMessage(`{"min": "1000000", "max": "10000000"}`)))
	assert.NoError(t, v.Validate(json.RawMessage(`{"min": "1000000"}`)))
	assert.NoError(t, v.Validate(json.RawMessage(`{"max": "10000000"}`)))
	assert.NoError(t, v.Validate(json.RawMessage(`{"min": "170141183460469231731687303715884105727"}`)))
	assert.NoError(t, v.Validate(json.RawMessage(`{"min": "1", "max": "2", "inclusive": false, "outside": true}`)))

	assert.Error(t, v.Validate(json.RawMessage(`{}`)), "at least one bound is required")
	assert.Error(t, v.Validate(json.RawMessage(`{"inclusive": true}`)), "at least one bound is required")
	assert.Error(t, v.Validate(json.RawMessage(`{"min": "abc", "max": "10"}`)), "non-numeric min rejected")
	assert.Error(t, v.Validate(json.RawMessage(`{"min": "10", "max": "xyz"}`)), "non-numeric max rejected")
	assert.Error(t, v.Validate(json.RawMessage(`{"min": "10", "max": "5"}`)), "min > max rejected")
	assert.Error(t, v.Validate(json.RawMessage(`{"min": "1.5", "max": "10"}`)), "fractional bound rejected")
}

func TestNumericRangeRegistered(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Validate(TypeNumericRange, json.RawMessage(`{"min": "1", "max": "2"}`)))
	got, err := r.Evaluate(context.Background(), TypeNumericRange, numericRangeEvent(big.NewInt(1)), json.RawMessage(`{"min": "1", "max": "2"}`))
	require.NoError(t, err)
	assert.True(t, got)
}
