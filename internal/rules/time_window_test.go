package rules

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// at returns a decoded event whose ledger close time is the given UTC wall
// time. 2026-09-23 is a Wednesday, which the day-filter cases rely on.
func at(y int, mo time.Month, d, h, mi int) *stellar.DecodedEvent {
	return &stellar.DecodedEvent{
		Topics:         []any{"transfer"},
		LedgerClosedAt: time.Date(y, mo, d, h, mi, 0, 0, time.UTC),
	}
}

func TestTimeWindowEvaluate(t *testing.T) {
	tests := []struct {
		name   string
		ev     *stellar.DecodedEvent
		params string
		want   bool
	}{
		{"inside the window", at(2026, 9, 23, 10, 0), `{"start":"09:00","end":"17:00"}`, true},
		{"before the window", at(2026, 9, 23, 8, 0), `{"start":"09:00","end":"17:00"}`, false},
		{"after the window", at(2026, 9, 23, 18, 0), `{"start":"09:00","end":"17:00"}`, false},
		{"boundary at start is inside", at(2026, 9, 23, 9, 0), `{"start":"09:00","end":"17:00"}`, true},
		{"boundary at end is outside", at(2026, 9, 23, 17, 0), `{"start":"09:00","end":"17:00"}`, false},
		{"outside inversion matches before", at(2026, 9, 23, 8, 0), `{"start":"09:00","end":"17:00","outside":true}`, true},
		{"outside inversion skips inside", at(2026, 9, 23, 10, 0), `{"start":"09:00","end":"17:00","outside":true}`, false},
		{"midnight crossing late", at(2026, 9, 23, 23, 30), `{"start":"22:00","end":"06:00"}`, true},
		{"midnight crossing early", at(2026, 9, 23, 3, 0), `{"start":"22:00","end":"06:00"}`, true},
		{"midnight crossing midday miss", at(2026, 9, 23, 12, 0), `{"start":"22:00","end":"06:00"}`, false},
		{"day filter excluded", at(2026, 9, 23, 10, 0), `{"start":"09:00","end":"17:00","days":["mon"]}`, false},
		{"day filter included", at(2026, 9, 23, 10, 0), `{"start":"09:00","end":"17:00","days":["wed"]}`, true},
		{"day filter inverted outside", at(2026, 9, 23, 10, 0), `{"start":"09:00","end":"17:00","days":["mon"],"outside":true}`, true},
		{"days omitted means every day", at(2026, 9, 27, 10, 0), `{"start":"09:00","end":"17:00"}`, true},
		{"zero close time does not match", &stellar.DecodedEvent{Topics: []any{"transfer"}}, `{"start":"00:00","end":"23:59"}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := (TimeWindow{}).Evaluate(context.Background(), tt.ev, json.RawMessage(tt.params))
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestTimeWindowValidate(t *testing.T) {
	ok := []string{
		`{"start":"09:00","end":"17:00"}`,
		`{"start":"22:00","end":"06:00"}`,
		`{"start":"00:00","end":"23:59","days":["sat","sun"],"outside":true}`,
	}
	for _, p := range ok {
		assert.NoError(t, (TimeWindow{}).Validate(json.RawMessage(p)), "params %s", p)
	}

	bad := map[string]string{
		`{}`:                                                "start is required",
		`{"start":"09:00"}`:                                 "end is required",
		`{"start":"9:00","end":"17:00"}`:                    "not a valid HH:MM",
		`{"start":"09:60","end":"17:00"}`:                   "not a valid HH:MM",
		`{"start":"0900","end":"17:00"}`:                    "not a valid HH:MM",
		`{"start":"aa:00","end":"17:00"}`:                   "not a valid HH:MM",
		`{"start":"09:00","end":"17:00","days":[]}`:         "days must not be empty",
		`{"start":"09:00","end":"17:00","days":["funday"]}`: "unknown day",
	}
	for p, want := range bad {
		err := (TimeWindow{}).Validate(json.RawMessage(p))
		require.Error(t, err, "params %s", p)
		assert.Contains(t, err.Error(), want, "params %s", p)
	}
}

func TestTimeWindowRegistered(t *testing.T) {
	if err := NewRegistry().Validate(TypeTimeWindow, json.RawMessage(`{"start":"09:00","end":"17:00"}`)); err != nil {
		t.Fatalf("time_window not registered: %v", err)
	}
}
