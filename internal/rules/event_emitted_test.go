package rules

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEventEmitted_Validate(t *testing.T) {
	tests := []struct {
		name         string
		params       string
		wantErr      bool
		wantFieldErr string
		wantPlainErr bool
	}{
		{
			name:   "Valid params: event_name only",
			params: `{"event_name": "transfer"}`,
		},
		{
			name:   "Valid params: topic_equals only",
			params: `{"topic_equals": {"1": "GABC..."}}`,
		},
		{
			// Catch a future refactor that changes parseEventEmitted's json.Unmarshal failure handling.
			name:         "Malformed JSON",
			params:       `{"event_name": `,
			wantErr:      true,
			wantPlainErr: true,
		},
		{
			// Event_name and/or topic_equals are required fields.
			name:         "Missing both event_name and topic_equals",
			params:       `{}`,
			wantErr:      true,
			wantFieldErr: "event_name",
		},
		{
			// Topic keys must be parseable as integers.
			name:         "topic_equals key that isn't a numeric index",
			params:       `{"topic_equals": {"foo":"bar"}}`,
			wantErr:      true,
			wantFieldErr: "topic_equals.foo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (EventEmitted{}).Validate(json.RawMessage(tt.params))
			if !tt.wantErr {
				require.NoError(t, err, "Validate should succeed")
				return
			}
			require.Error(t, err, "Validate should fail")

			if tt.wantPlainErr {
				// Assert it is NOT a FieldErrors (or FieldError) to catch refactors
				// that might change the json.Unmarshal failure handling.
				switch err.(type) {
				case FieldError, FieldErrors:
					t.Fatalf("expected plain error, got FieldError/FieldErrors: %v", err)
				}
				return
			}

			if tt.wantFieldErr != "" {
				// We expect a FieldErrors containing a specific FieldError.
				fe, ok := err.(FieldErrors)
				require.True(t, ok, "expected FieldErrors type, got %T: %v", err, err)

				found := false
				for _, e := range fe {
					if e.Field == tt.wantFieldErr {
						found = true
						break
					}
				}
				assert.True(t, found, "expected FieldErrors to contain field %q, got: %v", tt.wantFieldErr, fe)
			}
		})
	}
}

func TestEventEmitted_Evaluate(t *testing.T) {
	tests := []struct {
		name   string
		ev     *stellar.DecodedEvent
		params string
		want   bool
	}{
		{
			// Happy path: the event matches both the event name and the expected topic argument.
			name: "Event matching both event_name and topic_equals",
			ev: &stellar.DecodedEvent{
				Topics: []any{"transfer", "GABC..."},
			},
			params: `{"event_name": "transfer", "topic_equals": {"1": "GABC..."}}`,
			want:   true,
		},
		{
			// The event name does not match the rule's target event name.
			name: "Event with wrong event_name",
			ev: &stellar.DecodedEvent{
				Topics: []any{"mint", "GABC..."},
			},
			params: `{"event_name": "transfer"}`,
			want:   false,
		},
		{
			// The event name matches, but a specific topic filter fails.
			name: "Event with wrong topic_equals value",
			ev: &stellar.DecodedEvent{
				Topics: []any{"transfer", "GXYZ..."},
			},
			params: `{"topic_equals": {"1": "GABC..."}}`,
			want:   false,
		},
		{
			// Ensure EventName() and the topic index checks don't panic when Topics is completely empty.
			name: "Event with Topics == nil (no topics at all)",
			ev: &stellar.DecodedEvent{
				Topics: nil,
			},
			params: `{"event_name": "transfer"}`,
			want:   false,
		},
		{
			// EventName() falls through to "" for any type it doesn't recognize.
			// If event_name is set, an event with an unrecognized first topic fails to match.
			name: "Event whose first topic is not a symbol",
			ev: &stellar.DecodedEvent{
				Topics: []any{int64(42), "GABC..."},
			},
			params: `{"event_name": "transfer"}`,
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := (EventEmitted{}).Evaluate(context.Background(), tt.ev, json.RawMessage(tt.params))
			require.NoError(t, err, "Evaluate should not return an error for these cases")
			assert.Equal(t, tt.want, got)
		})
	}
}
