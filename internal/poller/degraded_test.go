package poller

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
)

type degradedPollerStore struct {
	Store
	degraded bool
}

func (d *degradedPollerStore) Degraded() bool {
	return d.degraded
}

func (d *degradedPollerStore) ListMonitors(ctx context.Context, enabledOnly bool) ([]store.Monitor, error) {
	if d.degraded {
		return nil, store.ErrUnavailable
	}
	return []store.Monitor{}, nil
}

type stubSource struct{}

func (s *stubSource) FetchEvents(ctx context.Context, startLedger uint32, watch []Watch, cursor string, limit int) (FetchPage, error) {
	return FetchPage{}, nil
}
func (s *stubSource) LatestLedger(ctx context.Context) (uint32, error) { return 100, nil }

type stubDispatcher struct{}

func (d *stubDispatcher) Dispatch(ctx context.Context, alert notify.Alert) {}

func TestPoller_DegradedStoreDoesNotAdvanceCursor(t *testing.T) {
	st := &degradedPollerStore{degraded: true}
	p := New(&stubSource{}, st, rules.NewRegistry(), &stubDispatcher{}, 1*time.Second, slog.Default())

	err := p.Poll(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrUnavailable)

	// Ingest position must not advance
	pos := p.Position()
	assert.False(t, pos.Ready(), "poller must not record a ready position when cycle failed")
	assert.Equal(t, uint32(0), pos.LastProcessedLedger)
}
