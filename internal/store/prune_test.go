package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/workspace"
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

func TestPruneWorkspaceAlertsOnce_IsolatesWorkspacesAndRecordsMetrics(t *testing.T) {
	ctx := context.Background()
	st := newTestSQLite(t).(*SQLite)
	m := metrics.New()
	st.WithMetrics(m)
	SetInstanceAlertRetention(st, 30*24*time.Hour)

	wsShort := workspace.ID("ws-7d")
	wsLong := workspace.ID("ws-90d")
	wsDefault := workspace.ID("ws-default-30d")
	ctxShort := workspace.With(ctx, wsShort)
	ctxLong := workspace.With(ctx, wsLong)
	ctxDefault := workspace.With(ctx, wsDefault)

	_, err := st.SetWorkspaceRetentionPolicy(ctxShort, SetWorkspaceRetentionInput{
		WorkspaceID:    string(wsShort),
		Retention:      7 * 24 * time.Hour,
		ConfirmShorten: true,
		Actor:          "ops",
	})
	require.NoError(t, err)
	_, err = st.SetWorkspaceRetentionPolicy(ctxLong, SetWorkspaceRetentionInput{
		WorkspaceID: string(wsLong),
		Retention:   90 * 24 * time.Hour,
		Actor:       "ops",
	})
	require.NoError(t, err)
	require.NoError(t, st.EnsureWorkspace(ctx, wsDefault))

	now := time.Now().UTC()
	seedAlertAt := func(wCtx context.Context, name, eventID string, age time.Duration) int64 {
		mon := &Monitor{Name: name, ContractIDs: []string{"CC"}, Enabled: true}
		require.NoError(t, st.CreateMonitor(wCtx, mon))
		rule := &Rule{MonitorID: mon.ID, Type: "event_emitted", Params: json.RawMessage(`{"event_name":"e"}`), Enabled: true}
		require.NoError(t, st.CreateRule(wCtx, rule))
		al := &Alert{MonitorID: mon.ID, RuleID: rule.ID, EventID: eventID, Payload: json.RawMessage(`{}`)}
		_, err := st.CreateAlert(wCtx, al)
		require.NoError(t, err)
		require.NoError(t, st.setAlertCreatedAt(wCtx, al.ID, now.Add(-age)))
		return al.ID
	}

	// ws-7d: one 10d alert (expired) and one 2d alert (kept).
	shortExpiredID := seedAlertAt(ctxShort, "m-short-1", "ev-short-old", 10*24*time.Hour)
	shortRecentID := seedAlertAt(ctxShort, "m-short-2", "ev-short-new", 2*24*time.Hour)
	// ws-90d: one 45d alert (kept under 90d override even though > 30d default).
	longKeptID := seedAlertAt(ctxLong, "m-long-1", "ev-long-45d", 45*24*time.Hour)
	// ws-default-30d: one 45d alert (expired under 30d default) and one 10d alert (kept).
	defExpiredID := seedAlertAt(ctxDefault, "m-def-1", "ev-def-45d", 45*24*time.Hour)
	defKeptID := seedAlertAt(ctxDefault, "m-def-2", "ev-def-10d", 10*24*time.Hour)

	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))

	deleted, err := PruneWorkspaceAlertsOnce(workspace.WithSystem(ctx), st, 30*24*time.Hour, 100, nil, log)
	require.NoError(t, err)
	assert.Equal(t, int64(2), deleted, "only ws-7d's 10d alert and ws-default-30d's 45d alert should be pruned")

	_, err = st.GetAlert(ctxShort, shortExpiredID)
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = st.GetAlert(ctxShort, shortRecentID)
	assert.NoError(t, err)

	_, err = st.GetAlert(ctxLong, longKeptID)
	assert.NoError(t, err, "45d alert in 90d workspace must survive pruning")

	_, err = st.GetAlert(ctxDefault, defExpiredID)
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = st.GetAlert(ctxDefault, defKeptID)
	assert.NoError(t, err)

	// Verify structured logs report workspace_id and policy_source.
	logs := logBuf.String()
	assert.Contains(t, logs, "workspace_id=ws-7d")
	assert.Contains(t, logs, "policy_source=workspace")
	assert.Contains(t, logs, "workspace_id=ws-default-30d")
	assert.Contains(t, logs, "policy_source=instance")
}
