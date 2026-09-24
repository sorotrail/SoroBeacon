package rules

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

func TestAbsenceValidate(t *testing.T) {
	tests := []struct {
		name    string
		params  string
		wantErr string
	}{
		{
			name:   "valid",
			params: `{"event_name": "heartbeat", "window": "30m"}`,
		},
		{
			name:    "missing event name",
			params:  `{"window": "30m"}`,
			wantErr: "event_name is required",
		},
		{
			name:    "blank event name",
			params:  `{"event_name": "  ", "window": "30m"}`,
			wantErr: "event_name is required",
		},
		{
			name:    "missing window",
			params:  `{"event_name": "heartbeat"}`,
			wantErr: "window is required",
		},
		{
			name:    "unparseable window",
			params:  `{"event_name": "heartbeat", "window": "half an hour"}`,
			wantErr: "is not a duration",
		},
		{
			name:    "zero window",
			params:  `{"event_name": "heartbeat", "window": "0s"}`,
			wantErr: "must be greater than zero",
		},
		{
			name:    "negative window",
			params:  `{"event_name": "heartbeat", "window": "-5m"}`,
			wantErr: "must be greater than zero",
		},
		{
			name:    "invalid json",
			params:  `{"event_name": 7}`,
			wantErr: "invalid params",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (Absence{}).Validate(json.RawMessage(tt.params))
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestAbsenceSpec(t *testing.T) {
	spec, err := (Absence{}).Spec(json.RawMessage(`{"event_name": " heartbeat ", "window": "90s"}`))
	require.NoError(t, err)
	assert.Equal(t, "heartbeat", spec.EventName, "the name is trimmed")
	assert.Equal(t, 90*1e9, float64(spec.Window), "the window is parsed as a duration")

	// Spec repeats Validate's checks rather than trusting a validated rule:
	// params can reach the database without passing through the API.
	_, err = (Absence{}).Spec(json.RawMessage(`{"event_name": "heartbeat"}`))
	assert.Error(t, err, "Spec must reject params the sweep could not act on")
}

func TestAbsenceMatches(t *testing.T) {
	spec := AbsenceSpec{EventName: "heartbeat", Window: 0}
	heartbeat := &stellar.DecodedEvent{Topics: []any{"heartbeat"}}
	// The RPC's xdrFormat:"json" path wraps the name, and EventName handles
	// both shapes, so a rule written against one source works with the other.
	wrapped := &stellar.DecodedEvent{Topics: []any{map[string]any{"symbol": "heartbeat"}}}

	assert.True(t, (Absence{}).Matches(heartbeat, spec))
	assert.True(t, (Absence{}).Matches(wrapped, spec))
	assert.False(t, (Absence{}).Matches(&stellar.DecodedEvent{Topics: []any{"mint"}}, spec))
	assert.False(t, (Absence{}).Matches(&stellar.DecodedEvent{}, spec), "no topics means no name")
	assert.False(t, (Absence{}).Matches(nil, spec), "a nil event must not panic the poller")
	assert.False(t, (Absence{}).Matches(heartbeat, AbsenceSpec{}), "an empty spec matches nothing")
}

func TestAbsenceRegisteredAsAbsence(t *testing.T) {
	r := NewRegistry()

	abs, ok := r.Absence(TypeAbsenceOfEvent)
	require.True(t, ok, "absence_of_event must be registered as an absence rule")
	require.NoError(t, abs.Validate(json.RawMessage(`{"event_name": "heartbeat", "window": "30m"}`)))

	// A known type that no event can satisfy answers false, not an error:
	// the poller evaluates every rule of a monitor that saw an event.
	got, err := r.Evaluate(context.Background(), TypeAbsenceOfEvent, transferEvent(1), json.RawMessage(`{"event_name": "heartbeat", "window": "30m"}`))
	require.NoError(t, err)
	assert.False(t, got)

	// Validate routes to the absence evaluator, so the API rejects malformed
	// absence rules at create/update time like any other type.
	assert.Error(t, r.Validate(TypeAbsenceOfEvent, json.RawMessage(`{}`)))
}
