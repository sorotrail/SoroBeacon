package poller

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/usage"
)

// TestWithUsageMeter covers the happy path and nil-meter path for
// WithUsageMeter, mirroring the shape of the other options tests in this file.
func TestWithUsageMeter(t *testing.T) {
	meter := usage.NewMeter(nil, nil, nil)
	tests := []struct {
		name  string
		meter *usage.Meter
	}{
		{"attaches a valid meter and returns the same Ingestor", meter},
		{"nil meter is accepted without panic", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			ing := newOptionsIngestor(st, &fakeDispatcher{}, slog.New(slog.DiscardHandler))
			got := ing.WithUsageMeter(tc.meter)
			require.Equal(t, ing, got, "WithUsageMeter must return the same *Ingestor for chaining")
		})
	}
}

// TestWithUsageMeterIngestWorks verifies that ingestion completes correctly
// both with a real meter and with a nil meter – usage recording must never
// prevent an alert from being created and dispatched.
func TestWithUsageMeterIngestWorks(t *testing.T) {
	meter := usage.NewMeter(nil, nil, nil)
	tests := []struct {
		name  string
		meter *usage.Meter
	}{
		{"non-nil meter does not block ingestion", meter},
		{"nil meter does not block ingestion", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			seedMonitor(st, `{"event_name": "transfer"}`)
			d := &fakeDispatcher{}
			ing := newOptionsIngestor(st, d, slog.New(slog.DiscardHandler)).
				WithUsageMeter(tc.meter)

			res := ing.Handle(context.Background(), decodedTransfer(t, "ev-1"), st.monitors, HandleOptions{Deliver: true})

			assert.Equal(t, 1, res.Alerts)
			assert.Equal(t, 1, res.Dispatched)
		})
	}
}
