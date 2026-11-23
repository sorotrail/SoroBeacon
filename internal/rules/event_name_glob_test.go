package rules

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

func globEvent(topics []any) *stellar.DecodedEvent {
	return &stellar.DecodedEvent{
		ID:         "0000000012884905986-0000000000",
		ContractID: "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA",
		Topics:     topics,
	}
}

func TestEventNameGlobEvaluate(t *testing.T) {
	tests := []struct {
		name    string
		params  string
		event   *stellar.DecodedEvent
		want    bool
		wantErr bool
	}{
		{
			name:   "exact match",
			params: `{"patterns": ["pool_deposit"]}`,
			event:  globEvent([]any{"pool_deposit"}),
			want:   true,
		},
		{
			name:   "star suffix matches family",
			params: `{"patterns": ["swap_*"]}`,
			event:  globEvent([]any{"swap_exact_in"}),
			want:   true,
		},
		{
			name:   "match is anchored so a prefix alone does not match",
			params: `{"patterns": ["swap"]}`,
			event:  globEvent([]any{"swap_exact_in"}),
			want:   false,
		},
		{
			name:   "question mark matches one character",
			params: `{"patterns": ["swap_?"]}`,
			event:  globEvent([]any{"swap_a"}),
			want:   true,
		},
		{
			name:   "question mark does not match two characters",
			params: `{"patterns": ["swap_?"]}`,
			event:  globEvent([]any{"swap_ab"}),
			want:   false,
		},
		{
			name:   "multiple patterns are ORed",
			params: `{"patterns": ["mint", "swap_*"]}`,
			event:  globEvent([]any{"swap_exact_out"}),
			want:   true,
		},
		{
			name:   "no pattern matches",
			params: `{"patterns": ["swap_*", "pool_deposit"]}`,
			event:  globEvent([]any{"transfer"}),
			want:   false,
		},
		{
			name:   "empty topics are a non-match",
			params: `{"patterns": ["swap_*"]}`,
			event:  globEvent(nil),
			want:   false,
		},
		{
			name:   "symbol wrapper shape matches",
			params: `{"patterns": ["swap_*"]}`,
			event:  globEvent([]any{map[string]any{"symbol": "swap_exact_in"}}),
			want:   true,
		},
		{
			name:    "malformed pattern errors",
			params:  `{"patterns": ["["]}`,
			event:   globEvent([]any{"transfer"}),
			wantErr: true,
		},
		{
			name:    "empty patterns error",
			params:  `{"patterns": []}`,
			event:   globEvent([]any{"transfer"}),
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := EventNameGlob{}.Evaluate(context.Background(), tt.event, json.RawMessage(tt.params))
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestEventNameGlobValidate(t *testing.T) {
	v := EventNameGlob{}
	assert.NoError(t, v.Validate(json.RawMessage(`{"patterns": ["swap_*", "pool_deposit"]}`)))
	assert.NoError(t, v.Validate(json.RawMessage(`{"patterns": ["transfer"]}`)))

	assert.Error(t, v.Validate(json.RawMessage(`{"patterns": []}`)), "empty patterns rejected")
	assert.Error(t, v.Validate(json.RawMessage(`{}`)), "missing patterns rejected")
	assert.Error(t, v.Validate(json.RawMessage(`{"patterns": ["["]}`)), "malformed pattern rejected")
}

func TestEventNameGlobRegistered(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Validate(TypeEventNameGlob, json.RawMessage(`{"patterns": ["swap_*"]}`)))
	got, err := r.Evaluate(context.Background(), TypeEventNameGlob, globEvent([]any{"swap_exact_in"}), json.RawMessage(`{"patterns": ["swap_*"]}`))
	require.NoError(t, err)
	assert.True(t, got)
}
