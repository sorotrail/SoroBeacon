package store

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakePruner struct {
	mu      sync.Mutex
	calls   int
	deleted []int64
}

func (f *fakePruner) DeleteExpiredAlerts(context.Context, time.Time, int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= len(f.deleted) {
		return f.deleted[f.calls-1], nil
	}
	return 0, nil
}

func TestRunAlertPrunerNoOpWhenUnset(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	fp := &fakePruner{}
	RunAlertPruner(context.Background(), fp, 0, time.Millisecond, 10, log)
	assert.Equal(t, 0, fp.calls)
	assert.Empty(t, buf.String())
}

func TestRunAlertPrunerBatchesUntilShortPage(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	fp := &fakePruner{deleted: []int64{1000, 1000, 3}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunAlertPruner(ctx, fp, 24*time.Hour, time.Hour, 1000, log)
	}()
	require.Eventually(t, func() bool {
		fp.mu.Lock()
		defer fp.mu.Unlock()
		return fp.calls >= 3
	}, time.Second, 10*time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pruner did not stop")
	}
	assert.Contains(t, buf.String(), "deleted=2003")
}
