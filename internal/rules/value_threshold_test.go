package rules

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

func TestValueThreshold_Validate(t *testing.T) {
	tests := []struct {
		name    string
		params  string
		wantErr bool
	}{
		{
			name:    "valid eq number",
			params:  `{"comparison": "eq", "threshold": 100}`,
			wantErr: false,
		},
		{
			name:    "valid gt string",
			params:  `{"comparison": "gt", "threshold": "340282366920938463463374607431768211455"}`,
			wantErr: false,
		},
		{
			name:    "malformed JSON",
			params:  `{"comparison": "eq"`,
			wantErr: true,
		},
		{
			name:    "unknown comparison",
			params:  `{"comparison": "foo", "threshold": 100}`,
			wantErr: true,
		},
		{
			name:    "non-numeric threshold",
			params:  `{"comparison": "eq", "threshold": "foo"}`,
			wantErr: true,
		},
		{
			name:    "missing threshold",
			params:  `{"comparison": "eq"}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule := ValueThreshold{}
			err := rule.Validate(json.RawMessage(tt.params))
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValueThreshold_Evaluate(t *testing.T) {
	tests := []struct {
		name    string
		params  string
		event   stellar.DecodedEvent
		want    bool
		wantErr bool
	}{
		// Testing "eq"
		{
			name:   "eq match",
			params: `{"comparison": "eq", "threshold": 100}`,
			event:  stellar.DecodedEvent{Value: 100},
			want:   true,
		},
		{
			name:   "eq no match (above)",
			params: `{"comparison": "eq", "threshold": 100}`,
			event:  stellar.DecodedEvent{Value: 101},
			want:   false,
		},
		{
			name:   "eq no match (below)",
			params: `{"comparison": "eq", "threshold": 100}`,
			event:  stellar.DecodedEvent{Value: 99},
			want:   false,
		},

		// Testing "neq"
		{
			name:   "neq match (above)",
			params: `{"comparison": "neq", "threshold": 100}`,
			event:  stellar.DecodedEvent{Value: 101},
			want:   true,
		},
		{
			name:   "neq match (below)",
			params: `{"comparison": "neq", "threshold": 100}`,
			event:  stellar.DecodedEvent{Value: 99},
			want:   true,
		},
		{
			name:   "neq no match",
			params: `{"comparison": "neq", "threshold": 100}`,
			event:  stellar.DecodedEvent{Value: 100},
			want:   false,
		},

		// Testing "gt"
		{
			name:   "gt match",
			params: `{"comparison": "gt", "threshold": 100}`,
			event:  stellar.DecodedEvent{Value: 101},
			want:   true,
		},
		{
			name:   "gt no match (equal)",
			params: `{"comparison": "gt", "threshold": 100}`,
			event:  stellar.DecodedEvent{Value: 100},
			want:   false,
		},
		{
			name:   "gt no match (below)",
			params: `{"comparison": "gt", "threshold": 100}`,
			event:  stellar.DecodedEvent{Value: 99},
			want:   false,
		},

		// Testing "gte"
		{
			name:   "gte match (above)",
			params: `{"comparison": "gte", "threshold": 100}`,
			event:  stellar.DecodedEvent{Value: 101},
			want:   true,
		},
		{
			name:   "gte match (equal)",
			params: `{"comparison": "gte", "threshold": 100}`,
			event:  stellar.DecodedEvent{Value: 100},
			want:   true,
		},
		{
			name:   "gte no match (below)",
			params: `{"comparison": "gte", "threshold": 100}`,
			event:  stellar.DecodedEvent{Value: 99},
			want:   false,
		},

		// Testing "lt"
		{
			name:   "lt match",
			params: `{"comparison": "lt", "threshold": 100}`,
			event:  stellar.DecodedEvent{Value: 99},
			want:   true,
		},
		{
			name:   "lt no match (equal)",
			params: `{"comparison": "lt", "threshold": 100}`,
			event:  stellar.DecodedEvent{Value: 100},
			want:   false,
		},
		{
			name:   "lt no match (above)",
			params: `{"comparison": "lt", "threshold": 100}`,
			event:  stellar.DecodedEvent{Value: 101},
			want:   false,
		},

		// Testing "lte"
		{
			name:   "lte match (below)",
			params: `{"comparison": "lte", "threshold": 100}`,
			event:  stellar.DecodedEvent{Value: 99},
			want:   true,
		},
		{
			name:   "lte match (equal)",
			params: `{"comparison": "lte", "threshold": 100}`,
			event:  stellar.DecodedEvent{Value: 100},
			want:   true,
		},
		{
			name:   "lte no match (above)",
			params: `{"comparison": "lte", "threshold": 100}`,
			event:  stellar.DecodedEvent{Value: 101},
			want:   false,
		},

		// CRITICAL: i128 well beyond int64 range
		{
			name:   "i128 eq match",
			params: `{"comparison": "eq", "threshold": "340282366920938463463374607431768211455"}`,
			event:  stellar.DecodedEvent{Value: "340282366920938463463374607431768211455"},
			want:   true,
		},
		{
			name:   "i128 gt match",
			params: `{"comparison": "gt", "threshold": "340282366920938463463374607431768211455"}`,
			event:  stellar.DecodedEvent{Value: "340282366920938463463374607431768211456"},
			want:   true,
		},
		{
			name:   "i128 lt match",
			params: `{"comparison": "lt", "threshold": "340282366920938463463374607431768211455"}`,
			event:  stellar.DecodedEvent{Value: "340282366920938463463374607431768211454"},
			want:   true,
		},
		{
			name:   "i128 gt no match",
			params: `{"comparison": "gt", "threshold": "340282366920938463463374607431768211455"}`,
			event:  stellar.DecodedEvent{Value: "340282366920938463463374607431768211455"},
			want:   false,
		},

		// Value is missing
		{
			name:   "missing value",
			params: `{"comparison": "eq", "threshold": 100, "value_path": "amount"}`,
			event:  stellar.DecodedEvent{Value: map[string]any{"other": 100}},
			want:   false,
		},

		// Value is not numeric
		{
			name:   "non-numeric value",
			params: `{"comparison": "eq", "threshold": 100}`,
			event:  stellar.DecodedEvent{Value: "notanumber"},
			want:   false,
		},

		// Event name filter
		{
			name:   "event name mismatch",
			params: `{"comparison": "eq", "threshold": 100, "event_name": "transfer"}`,
			event: stellar.DecodedEvent{
				Topics: []any{"mint"},
				Value:  100,
			},
			want: false,
		},
		{
			name:   "event name match",
			params: `{"comparison": "eq", "threshold": 100, "event_name": "transfer"}`,
			event: stellar.DecodedEvent{
				Topics: []any{"transfer"},
				Value:  100,
			},
			want: true,
		},

		// Event name from fields map
		{
			name:   "value path in fields",
			params: `{"comparison": "eq", "threshold": 100, "value_path": "amount"}`,
			event: stellar.DecodedEvent{
				Fields: map[string]any{"amount": 100},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule := ValueThreshold{}
			got, err := rule.Evaluate(context.Background(), &tt.event, json.RawMessage(tt.params))
			if (err != nil) != tt.wantErr {
				t.Errorf("Evaluate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("Evaluate() got = %v, want %v", got, tt.want)
			}
		})
	}
}
