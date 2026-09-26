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

// supplyEvent builds a SEP-41-style event with bare-string topics, as the
// local XDR decode path produces.
func supplyEvent(name string, value any) *stellar.DecodedEvent {
	return &stellar.DecodedEvent{
		ID:         "0000000012884905986-0000000000",
		ContractID: "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA",
		Topics: []any{
			name,
			map[string]any{"address": "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABCDW"},
			map[string]any{"address": "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABCDX"},
		},
		Value: value,
	}
}

// supplyEventWrapped builds the same event with single-key symbol/address
// wrappers, as the RPC's xdrFormat:"json" path produces.
func supplyEventWrapped(name string, value any) *stellar.DecodedEvent {
	return &stellar.DecodedEvent{
		ID:         "0000000012884905986-0000000000",
		ContractID: "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA",
		Topics: []any{
			map[string]any{"symbol": name},
			map[string]any{"address": "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABCDW"},
			map[string]any{"address": "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABCDX"},
		},
		Value: value,
	}
}

func TestTokenSupplyChangeEvaluate(t *testing.T) {
	tests := []struct {
		name    string
		params  string
		event   *stellar.DecodedEvent
		want    bool
		wantErr bool
	}{
		{
			name:   "mint matches with default direction",
			params: `{}`,
			event:  supplyEvent("mint", map[string]any{"i128": "1000000"}),
			want:   true,
		},
		{
			name:   "burn matches with default direction",
			params: `{}`,
			event:  supplyEvent("burn", map[string]any{"i128": "1000000"}),
			want:   true,
		},
		{
			name:   "any direction matches mint",
			params: `{"direction": "any"}`,
			event:  supplyEvent("mint", map[string]any{"i128": "1000000"}),
			want:   true,
		},
		{
			name:   "mint direction matches mint",
			params: `{"direction": "mint"}`,
			event:  supplyEvent("mint", map[string]any{"i128": "1000000"}),
			want:   true,
		},
		{
			name:   "mint direction does not match burn",
			params: `{"direction": "mint"}`,
			event:  supplyEvent("burn", map[string]any{"i128": "1000000"}),
			want:   false,
		},
		{
			name:   "burn direction matches burn",
			params: `{"direction": "burn"}`,
			event:  supplyEvent("burn", map[string]any{"i128": "1000000"}),
			want:   true,
		},
		{
			name:   "burn direction does not match mint",
			params: `{"direction": "burn"}`,
			event:  supplyEvent("mint", map[string]any{"i128": "1000000"}),
			want:   false,
		},
		{
			name:   "amount above threshold matches",
			params: `{"min_amount": "1000000"}`,
			event:  supplyEvent("mint", map[string]any{"i128": "2000000"}),
			want:   true,
		},
		{
			name:   "amount at threshold matches inclusively",
			params: `{"min_amount": "1000000"}`,
			event:  supplyEvent("burn", map[string]any{"i128": "1000000"}),
			want:   true,
		},
		{
			name:   "amount below threshold does not match",
			params: `{"min_amount": "1000000"}`,
			event:  supplyEvent("mint", map[string]any{"i128": "999999"}),
			want:   false,
		},
		{
			name:   "bare big.Int value matches",
			params: `{"min_amount": "1000000"}`,
			event:  supplyEvent("mint", big.NewInt(2000000)),
			want:   true,
		},
		{
			name:   "wide i128 amount compares exactly",
			params: `{"min_amount": "170141183460469231731687303715884105726"}`,
			event:  supplyEvent("mint", map[string]any{"i128": "170141183460469231731687303715884105727"}),
			want:   true,
		},
		{
			name:   "clawback is not a supply change",
			params: `{}`,
			event:  supplyEvent("clawback", map[string]any{"i128": "1000000"}),
			want:   false,
		},
		{
			name:   "transfer is not a supply change",
			params: `{}`,
			event:  supplyEvent("transfer", map[string]any{"i128": "1000000"}),
			want:   false,
		},
		{
			name:   "non-SEP-41 event does not match",
			params: `{}`,
			event:  &stellar.DecodedEvent{Topics: []any{"swap_exact_in"}},
			want:   false,
		},
		{
			name:   "event with no topics does not match",
			params: `{}`,
			event:  &stellar.DecodedEvent{},
			want:   false,
		},
		{
			name:   "symbol wrapper mint matches",
			params: `{"direction": "mint"}`,
			event:  supplyEventWrapped("mint", map[string]any{"i128": "1000000"}),
			want:   true,
		},
		{
			name:   "symbol wrapper burn matches",
			params: `{}`,
			event:  supplyEventWrapped("burn", map[string]any{"i128": "1000000"}),
			want:   true,
		},
		{
			name:   "symbol wrapper clawback does not match",
			params: `{}`,
			event:  supplyEventWrapped("clawback", map[string]any{"i128": "1000000"}),
			want:   false,
		},
		{
			name:   "missing amount with threshold is a non-match",
			params: `{"min_amount": "1000000"}`,
			event:  supplyEvent("mint", nil),
			want:   false,
		},
		{
			name:    "unknown direction errors",
			params:  `{"direction": "freeze"}`,
			event:   supplyEvent("mint", map[string]any{"i128": "1000000"}),
			wantErr: true,
		},
		{
			name:    "non-numeric min_amount errors",
			params:  `{"min_amount": "lots"}`,
			event:   supplyEvent("mint", map[string]any{"i128": "1000000"}),
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := TokenSupplyChange{}.Evaluate(context.Background(), tt.event, json.RawMessage(tt.params))
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestTokenSupplyChangeValidate(t *testing.T) {
	v := TokenSupplyChange{}
	assert.NoError(t, v.Validate(json.RawMessage(`{}`)))
	assert.NoError(t, v.Validate(json.RawMessage(`{"direction": "any"}`)))
	assert.NoError(t, v.Validate(json.RawMessage(`{"direction": "mint", "min_amount": "1000000"}`)))
	assert.NoError(t, v.Validate(json.RawMessage(`{"direction": "burn"}`)))

	assert.Error(t, v.Validate(json.RawMessage(`{"direction": "freeze"}`)), "unknown direction rejected")
	assert.Error(t, v.Validate(json.RawMessage(`{"min_amount": "lots"}`)), "non-numeric min_amount rejected")
	assert.Error(t, v.Validate(json.RawMessage(`{"min_amount": "1.5"}`)), "fractional min_amount rejected")
}

func TestTokenSupplyChangeRegistered(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Validate(TypeTokenSupplyChange, json.RawMessage(`{"direction": "any"}`)))
	got, err := r.Evaluate(context.Background(), TypeTokenSupplyChange, supplyEvent("mint", map[string]any{"i128": "1"}), json.RawMessage(`{}`))
	require.NoError(t, err)
	assert.True(t, got)
}
