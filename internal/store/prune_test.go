package store

import (
	"bytes"
	"context"
	"errors"
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
	RunAlertPruner(context.Background(), fp, 0, time.Millisecond, 10, nil, log)
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
		RunAlertPruner(ctx, fp, 24*time.Hour, time.Hour, 1000, nil, log)
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

// archiveSource is a store with the two methods the archiving pruner calls,
// recording the interleaving of reads, archives and deletes in `events` so a
// test can prove the ordering guarantee.
type archiveSource struct {
	mu      sync.Mutex
	batches [][]Alert
	reads   int
	deletes int
	events  *[]string
}

func (f *archiveSource) ExpiredAlerts(context.Context, time.Time, int) ([]Alert, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reads >= len(f.batches) {
		return nil, nil
	}
	b := f.batches[f.reads]
	f.reads++
	if f.events != nil {
		*f.events = append(*f.events, "read")
	}
	return b, nil
}

func (f *archiveSource) DeleteExpiredAlerts(context.Context, time.Time, int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes++
	n := int64(0)
	if f.deletes <= len(f.batches) {
		n = int64(len(f.batches[f.deletes-1]))
	}
	if f.events != nil {
		*f.events = append(*f.events, "delete")
	}
	return n, nil
}

type fakeArchiver struct {
	mu       sync.Mutex
	got      [][]Alert
	err      error
	events   *[]string
	attempts int
}

func (f *fakeArchiver) Archive(_ context.Context, alerts []Alert) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.events != nil {
		*f.events = append(*f.events, "archive")
	}
	if f.err != nil {
		return f.err
	}
	cp := append([]Alert(nil), alerts...)
	f.got = append(f.got, cp)
	return nil
}

// TestRunAlertPrunerArchivesBeforeDeleting pins the ordering: every batch is
// read, then archived, then deleted.
func TestRunAlertPrunerArchivesBeforeDeleting(t *testing.T) {
	var events []string
	src := &archiveSource{
		batches: [][]Alert{{{ID: 1}, {ID: 2}}, {{ID: 3}}},
		events:  &events,
	}
	arch := &fakeArchiver{events: &events}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		// batch=2 makes the first read a full page, so the loop continues to
		// the second (short) batch, which then ends the pass.
		RunAlertPruner(ctx, src, 24*time.Hour, time.Hour, 2, arch, log)
	}()

	require.Eventually(t, func() bool {
		src.mu.Lock()
		defer src.mu.Unlock()
		return src.deletes >= 2
	}, time.Second, 10*time.Millisecond)
	cancel()
	<-done

	arch.mu.Lock()
	defer arch.mu.Unlock()
	require.Len(t, arch.got, 2, "both batches are archived")
	assert.Len(t, arch.got[0], 2)
	assert.Len(t, arch.got[1], 1)
	assert.Equal(t, []string{"read", "archive", "delete", "read", "archive", "delete"}, events)
}

// TestRunAlertPrunerArchiveFailureBlocksDelete is the guarantee that turns
// retention from destruction into tiering: if the archive write fails, the
// rows must survive.
func TestRunAlertPrunerArchiveFailureBlocksDelete(t *testing.T) {
	src := &archiveSource{batches: [][]Alert{{{ID: 1}}}}
	arch := &fakeArchiver{err: errors.New("bucket unreachable")}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunAlertPruner(ctx, src, 24*time.Hour, time.Hour, 1000, arch, log)
	}()

	require.Eventually(t, func() bool {
		arch.mu.Lock()
		defer arch.mu.Unlock()
		return arch.attempts >= 1
	}, time.Second, 10*time.Millisecond)
	cancel()
	<-done

	src.mu.Lock()
	defer src.mu.Unlock()
	assert.Equal(t, 0, src.deletes, "a failed archive must block the delete")
	assert.Contains(t, buf.String(), "archive failed")
}
