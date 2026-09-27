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

func transferEvent(amount int64) *stellar.DecodedEvent {
	return &stellar.DecodedEvent{
		ID:         "0000000012884905986-0000000000",
		ContractID: "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA",
		Topics: []any{
			"transfer",
			"GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF",
			"GBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBC7S",
		},
		Value: map[string]any{"amount": big.NewInt(amount), "memo": "hi"},
	}
}

func TestEventEmitted(t *testing.T) {
	tests := []struct {
		name    string
		params  string
		event   *stellar.DecodedEvent
		want    bool
		wantErr bool
	}{
		{
			name:   "matches event name",
			params: `{"event_name": "transfer"}`,
			event:  transferEvent(5),
			want:   true,
		},
		{
			name:   "wrong event name",
			params: `{"event_name": "mint"}`,
			event:  transferEvent(5),
			want:   false,
		},
		{
			name:   "empty name matches any event",
			params: `{"topic_equals": {"0": "transfer"}}`,
			event:  transferEvent(5),
			want:   true,
		},
		{
			name:   "topic arg equality matches",
			params: `{"event_name": "transfer", "topic_equals": {"1": "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF"}}`,
			event:  transferEvent(5),
			want:   true,
		},
		{
			name:   "topic arg equality mismatch",
			params: `{"event_name": "transfer", "topic_equals": {"1": "GBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBC7S"}}`,
			event:  transferEvent(5),
			want:   false,
		},
		{
			name:   "topic index out of range",
			params: `{"event_name": "transfer", "topic_equals": {"9": "x"}}`,
			event:  transferEvent(5),
			want:   false,
		},
		{
			name:   "numeric topic compares canonically",
			params: `{"topic_equals": {"1": 42}}`,
			event: &stellar.DecodedEvent{
				Topics: []any{"count", big.NewInt(42)},
			},
			want: true,
		},
		{
			name:    "invalid params",
			params:  `{"event_name": 7}`,
			event:   transferEvent(5),
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := EventEmitted{}.Evaluate(context.Background(), tt.event, json.RawMessage(tt.params))
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestEventEmittedValidate(t *testing.T) {
	e := EventEmitted{}
	assert.Error(t, e.Validate(json.RawMessage(`{}`)), "must require something to match on")
	assert.Error(t, e.Validate(json.RawMessage(`{"topic_equals": {"notanumber": "x"}}`)))
	assert.NoError(t, e.Validate(json.RawMessage(`{"event_name": "transfer"}`)))
}

func TestValueThreshold(t *testing.T) {
	tests := []struct {
		name    string
		params  string
		event   *stellar.DecodedEvent
		want    bool
		wantErr bool
	}{
		{
			name:   "gt matches",
			params: `{"comparison": "gt", "threshold": 100, "value_path": "amount"}`,
			event:  transferEvent(101),
			want:   true,
		},
		{
			name:   "gt at boundary does not match",
			params: `{"comparison": "gt", "threshold": 100, "value_path": "amount"}`,
			event:  transferEvent(100),
			want:   false,
		},
		{
			name:   "gte at boundary matches",
			params: `{"comparison": "gte", "threshold": 100, "value_path": "amount"}`,
			event:  transferEvent(100),
			want:   true,
		},
		{
			name:   "lt matches",
			params: `{"comparison": "lt", "threshold": 100, "value_path": "amount"}`,
			event:  transferEvent(99),
			want:   true,
		},
		{
			name:   "eq matches",
			params: `{"comparison": "eq", "threshold": 100, "value_path": "amount"}`,
			event:  transferEvent(100),
			want:   true,
		},
		{
			name:   "neq matches",
			params: `{"comparison": "neq", "threshold": 100, "value_path": "amount"}`,
			event:  transferEvent(7),
			want:   true,
		},
		{
			name:   "string threshold handles >53-bit integers",
			params: `{"comparison": "gt", "threshold": "92233720368547758079999", "value_path": "amount"}`,
			event: &stellar.DecodedEvent{
				Topics: []any{"transfer"},
				Value:  map[string]any{"amount": mustBig("92233720368547758080000")},
			},
			want: true,
		},
		{
			name:   "event_name scoping",
			params: `{"comparison": "gt", "threshold": 1, "value_path": "amount", "event_name": "mint"}`,
			event:  transferEvent(50),
			want:   false,
		},
		{
			name:   "bare numeric value without path",
			params: `{"comparison": "gte", "threshold": 10}`,
			event:  &stellar.DecodedEvent{Value: big.NewInt(10)},
			want:   true,
		},
		{
			name:   "missing path does not match",
			params: `{"comparison": "gt", "threshold": 1, "value_path": "nope"}`,
			event:  transferEvent(50),
			want:   false,
		},
		{
			name:   "non-numeric value does not match",
			params: `{"comparison": "gt", "threshold": 1, "value_path": "memo"}`,
			event:  transferEvent(50),
			want:   false,
		},
		{
			// With a contract spec, value_path addresses the spec's named
			// fields rather than the raw positional value.
			name:   "value_path addresses named spec fields",
			params: `{"comparison": "gt", "threshold": 100, "value_path": "amount"}`,
			event: &stellar.DecodedEvent{
				Topics: []any{"transfer", "GFROM", "GTO"},
				Value:  map[string]any{"amount": big.NewInt(5)},
				Fields: map[string]any{"from": "GFROM", "to": "GTO", "amount": big.NewInt(500)},
			},
			want: true,
		},
		{
			// A named topic-located parameter is addressable too.
			name:   "value_path addresses a named topic parameter",
			params: `{"comparison": "gte", "threshold": 500, "value_path": "amount"}`,
			event: &stellar.DecodedEvent{
				Topics: []any{"transfer", "GFROM", "GTO"},
				Value:  big.NewInt(1),
				Fields: map[string]any{"from": "GFROM", "to": "GTO", "amount": big.NewInt(500)},
			},
			want: true,
		},
		{
			// A rule that predates the spec (no value_path) must keep
			// matching the raw value even when the contract now exports a
			// spec, otherwise making a contract spec-aware would silently
			// stop existing alerts from firing.
			name:   "bare value still matches when a spec is present",
			params: `{"comparison": "gte", "threshold": 10}`,
			event: &stellar.DecodedEvent{
				Value:  big.NewInt(10),
				Fields: map[string]any{"admin": "GADMIN"},
			},
			want: true,
		},
		{
			// Named fields win when the path resolves there, but a path the
			// spec does not name falls back to the raw value so a positional
			// rule keeps working.
			name:   "unmapped path falls back to the raw value",
			params: `{"comparison": "gt", "threshold": 1, "value_path": "amount"}`,
			event: &stellar.DecodedEvent{
				Value:  map[string]any{"amount": big.NewInt(50)},
				Fields: map[string]any{"admin": "GADMIN"},
			},
			want: true,
		},
		{
			name:    "invalid comparison errors",
			params:  `{"comparison": "wat", "threshold": 1}`,
			event:   transferEvent(50),
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValueThreshold{}.Evaluate(context.Background(), tt.event, json.RawMessage(tt.params))
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestValueThresholdValidate(t *testing.T) {
	v := ValueThreshold{}
	assert.Error(t, v.Validate(json.RawMessage(`{}`)))
	assert.Error(t, v.Validate(json.RawMessage(`{"comparison": "sideways", "threshold": 1}`)))
	assert.Error(t, v.Validate(json.RawMessage(`{"comparison": "gt", "threshold": "not a number"}`)))
	assert.NoError(t, v.Validate(json.RawMessage(`{"comparison": "gt", "threshold": "123456789012345678901234567890"}`)))
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	assert.ElementsMatch(t, []string{TypeEventEmitted, TypeValueThreshold, TypeTokenEvent, TypeFrequencyThreshold, TypeTopicRegex, TypeAddressWatchlist, TypeTopicPosition}, r.Types())

	_, err := r.Evaluate(context.Background(), "unknown", transferEvent(1), json.RawMessage(`{}`))
	assert.Error(t, err)
	assert.Error(t, r.Validate("unknown", json.RawMessage(`{}`)))

	got, err := r.Evaluate(context.Background(), TypeEventEmitted, transferEvent(1), json.RawMessage(`{"event_name": "transfer"}`))
	require.NoError(t, err)
	assert.True(t, got)
}

func mustBig(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic("bad big.Int literal " + s)
	}
	return v
}

// Benchmarks use the same decoded transfer events as the table tests so the
// numbers reflect the real hot path (JSON params + a populated DecodedEvent),
// not empty structs. Matching and non-matching cases are both measured: the
// non-matching path is what the poller hits for most (rule, event) pairs.
func BenchmarkEventEmitted(b *testing.B) {
	ev := transferEvent(5)
	e := EventEmitted{}
	ctx := context.Background()
	cases := []struct {
		name  string
		want  bool
		param json.RawMessage
	}{
		{name: "match", want: true, param: json.RawMessage(`{"event_name":"transfer"}`)},
		{name: "nomatch", want: false, param: json.RawMessage(`{"event_name":"mint"}`)},
	}
	for _, tt := range cases {
		b.Run(tt.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				got, err := e.Evaluate(ctx, ev, tt.param)
				if err != nil {
					b.Fatal(err)
				}
				if got != tt.want {
					b.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func BenchmarkValueThreshold(b *testing.B) {
	matchEv := transferEvent(101)
	missEv := transferEvent(100)
	v := ValueThreshold{}
	ctx := context.Background()
	params := json.RawMessage(`{"comparison":"gt","threshold":100,"value_path":"amount"}`)
	cases := []struct {
		name string
		ev   *stellar.DecodedEvent
		want bool
	}{
		{name: "match", ev: matchEv, want: true},
		{name: "nomatch", ev: missEv, want: false},
	}
	for _, tt := range cases {
		b.Run(tt.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				got, err := v.Evaluate(ctx, tt.ev, params)
				if err != nil {
					b.Fatal(err)
				}
				if got != tt.want {
					b.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}
