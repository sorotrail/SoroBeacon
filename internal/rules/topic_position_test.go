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

// poolEvent is a custom (non-SEP-41) pool contract's deposit event. The
// topics arrive in the two decoded shapes: the XDR path emits bare Go values
// (strings and *big.Int numerics), while the RPC's xdrFormat:"json" path
// emits single-key wrappers ({"symbol":...} / {"address":...}) that
// decodeJSONVal normally normalises — a rule evaluator must handle both.
func poolEvent(shaped bool) *stellar.DecodedEvent {
	name, pool, sender := any("deposit"), any("POOL_USDC_XLM"), any("GAAA1")
	amount := any(big.NewInt(1000000))
	if shaped {
		name = map[string]any{"symbol": "deposit"}
		pool = map[string]any{"string": "POOL_USDC_XLM"}
		sender = map[string]any{"address": "GAAA1"}
		amount = map[string]any{"i128": "1000000"}
	}
	return &stellar.DecodedEvent{
		ID:         "0000000012884905986-0000000002",
		ContractID: "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA",
		Topics:     []any{name, pool, sender},
		Value:      amount,
	}
}

func TestTopicPositionEvaluate(t *testing.T) {
	tests := []struct {
		name    string
		params  string
		event   *stellar.DecodedEvent
		want    bool
		wantErr bool
	}{
		{
			name:   "matches at the configured position",
			params: `{"position": 1, "equals": "POOL_USDC_XLM"}`,
			event:  poolEvent(false),
			want:   true,
		},
		{
			name:   "wrong value at that position does not match",
			params: `{"position": 1, "equals": "POOL_XLM_USDC"}`,
			event:  poolEvent(false),
			want:   false,
		},
		{
			name:   "same value at another position does not match",
			params: `{"position": 0, "equals": "POOL_USDC_XLM"}`,
			event:  poolEvent(false),
			want:   false,
		},
		{
			name:   "position 0 pins the exact event name",
			params: `{"position": 0, "equals": "deposit"}`,
			event:  poolEvent(false),
			want:   true,
		},
		{
			name:   "position 0 with the wrong name does not match",
			params: `{"position": 0, "equals": "withdraw"}`,
			event:  poolEvent(false),
			want:   false,
		},
		{
			name:   "out-of-range position means no match, not an error",
			params: `{"position": 7, "equals": "POOL_USDC_XLM"}`,
			event:  poolEvent(false),
			want:   false,
		},
		{
			name:   "event with no topics never matches",
			params: `{"position": 0, "equals": "deposit"}`,
			event:  &stellar.DecodedEvent{},
			want:   false,
		},
		{
			name:   "event filter matches when the name agrees",
			params: `{"position": 1, "equals": "POOL_USDC_XLM", "event": "deposit"}`,
			event:  poolEvent(false),
			want:   true,
		},
		{
			name:   "event filter excludes identically-shaped events with another name",
			params: `{"position": 1, "equals": "POOL_USDC_XLM", "event": "withdraw"}`,
			event:  poolEvent(false),
			want:   false,
		},
		{
			name:   "event filter excludes an event with no readable name",
			params: `{"position": 1, "equals": "POOL_USDC_XLM", "event": "deposit"}`,
			event:  &stellar.DecodedEvent{Topics: []any{"POOL_USDC_XLM"}},
			want:   false,
		},
		{
			name:   "numeric topic compares by its decoded decimal form",
			params: `{"position": 0, "equals": "42"}`,
			event:  &stellar.DecodedEvent{Topics: []any{big.NewInt(42)}},
			want:   true,
		},
		{
			name:   "numeric topic does not match a different rendering of another number",
			params: `{"position": 0, "equals": "42000"}`,
			event:  &stellar.DecodedEvent{Topics: []any{big.NewInt(42)}},
			want:   false,
		},
		{
			name:   "wrapper-shaped symbol topic matches like a bare string",
			params: `{"position": 0, "equals": "deposit"}`,
			event:  poolEvent(true),
			want:   true,
		},
		{
			name:   "wrapper-shaped string topic matches",
			params: `{"position": 1, "equals": "POOL_USDC_XLM"}`,
			event:  poolEvent(true),
			want:   true,
		},
		{
			name:   "wrapper-shaped address topic matches",
			params: `{"position": 2, "equals": "GAAA1"}`,
			event:  poolEvent(true),
			want:   true,
		},
		{
			name:   "matching is case-sensitive",
			params: `{"position": 1, "equals": "pool_usdc_xlm"}`,
			event:  poolEvent(false),
			want:   false,
		},
		{
			name:    "malformed params error",
			params:  `{"position":`,
			event:   poolEvent(false),
			wantErr: true,
		},
		{
			name:    "position of the wrong JSON type errors",
			params:  `{"position": "one", "equals": "x"}`,
			event:   poolEvent(false),
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := new(TopicPosition).Evaluate(context.Background(), tt.event, json.RawMessage(tt.params))
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestTopicPositionWrapperNumericTopic covers the one wrapper case the main
// table cannot express: a position that exists on the event and carries a
// raw {"i128": "..."} wrapper, comparing by its decoded decimal form.
func TestTopicPositionWrapperNumericTopic(t *testing.T) {
	ev := &stellar.DecodedEvent{Topics: []any{map[string]any{"i128": "1000000"}}}
	got, err := new(TopicPosition).Evaluate(context.Background(), ev, json.RawMessage(`{"position": 0, "equals": "1000000"}`))
	require.NoError(t, err)
	assert.True(t, got)

	got, err = new(TopicPosition).Evaluate(context.Background(), ev, json.RawMessage(`{"position": 0, "equals": "1000001"}`))
	require.NoError(t, err)
	assert.False(t, got)
}

func TestTopicPositionValidate(t *testing.T) {
	tests := []struct {
		name    string
		params  string
		wantErr bool
		wantSub string
	}{
		{name: "valid position and equals", params: `{"position": 2, "equals": "POOL_USDC_XLM"}`},
		{name: "position 0 is valid", params: `{"position": 0, "equals": "deposit"}`},
		{name: "optional event accepted", params: `{"position": 2, "equals": "POOL_USDC_XLM", "event": "deposit"}`},
		{name: "missing position defaults to 0, the event-name topic", params: `{"equals": "deposit"}`},
		{name: "negative position", params: `{"position": -1, "equals": "x"}`, wantErr: true, wantSub: "negative"},
		{name: "missing equals", params: `{"position": 2}`, wantErr: true, wantSub: "equals is required"},
		{name: "empty equals", params: `{"position": 2, "equals": ""}`, wantErr: true, wantSub: "equals is required"},
		{name: "position must be a number", params: `{"position": "two", "equals": "x"}`, wantErr: true, wantSub: "invalid params"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := new(TopicPosition).Validate(json.RawMessage(tt.params))
			if !tt.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			if tt.wantSub != "" {
				assert.Contains(t, err.Error(), tt.wantSub)
			}
		})
	}
}

// TestTopicPositionRejectedAtCreateTime checks the registry path the API
// uses: bad params must be a Validate failure (an HTTP 400 at rule
// creation), not a per-event evaluation error discovered once an event
// arrives.
func TestTopicPositionRejectedAtCreateTime(t *testing.T) {
	r := NewRegistry()
	err := r.Validate(TypeTopicPosition, json.RawMessage(`{"position": -1, "equals": ""}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "negative")
	assert.Contains(t, err.Error(), "equals is required")
}

// TestTopicPositionRegistered confirms the type is reachable through the
// default registry, both for validation and evaluation.
func TestTopicPositionRegistered(t *testing.T) {
	r := NewRegistry()
	assert.Contains(t, r.Types(), TypeTopicPosition)

	require.NoError(t, r.Validate(TypeTopicPosition, json.RawMessage(`{"position": 1, "equals": "POOL_USDC_XLM"}`)))
	got, err := r.Evaluate(context.Background(), TypeTopicPosition, poolEvent(false), json.RawMessage(`{"position": 1, "equals": "POOL_USDC_XLM"}`))
	require.NoError(t, err)
	assert.True(t, got)
}

// TestTopicPositionEventNames covers the EventNamer contract the poller uses
// to narrow server-side getEvents filters: the optional event filter makes
// the rule nameable, its absence must not.
func TestTopicPositionEventNames(t *testing.T) {
	names, ok := new(TopicPosition).EventNames(json.RawMessage(`{"position": 1, "equals": "x", "event": "deposit"}`))
	require.True(t, ok)
	assert.Equal(t, []string{"deposit"}, names)

	_, ok = new(TopicPosition).EventNames(json.RawMessage(`{"position": 1, "equals": "x"}`))
	assert.False(t, ok, "without an event filter the rule must not narrow the server-side filter")
}
