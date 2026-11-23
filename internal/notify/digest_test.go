package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/store"
)

func scrapeMetricsText(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	return rec.Body.String()
}

// fakeDigestQueue is an in-memory DigestQueue. A second Dispatcher sharing the
// same queue stands in for a restart: the rows outlive the dispatcher.
type fakeDigestQueue struct {
	mu   sync.Mutex
	seq  int64
	rows map[int64][]store.DigestAlert
}

func newFakeDigestQueue() *fakeDigestQueue {
	return &fakeDigestQueue{rows: map[int64][]store.DigestAlert{}}
}

func (q *fakeDigestQueue) PushDigestAlert(_ context.Context, channelID int64, payload json.RawMessage) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.seq++
	row := store.DigestAlert{ID: q.seq, ChannelID: channelID, Payload: payload, CreatedAt: time.Now()}
	q.rows[channelID] = append(q.rows[channelID], row)
	return nil
}

func (q *fakeDigestQueue) ListDigestAlerts(_ context.Context, channelID int64) ([]store.DigestAlert, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]store.DigestAlert, len(q.rows[channelID]))
	copy(out, q.rows[channelID])
	return out, nil
}

func (q *fakeDigestQueue) DeleteDigestAlerts(_ context.Context, channelID int64, ids []int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	drop := map[int64]bool{}
	for _, id := range ids {
		drop[id] = true
	}
	kept := q.rows[channelID][:0]
	for _, row := range q.rows[channelID] {
		if !drop[row.ID] {
			kept = append(kept, row)
		}
	}
	q.rows[channelID] = kept
	return nil
}

func (q *fakeDigestQueue) count(channelID int64) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.rows[channelID])
}

func digestChannel(id int64, windowSeconds int64) store.Channel {
	return store.Channel{
		ID: id, Name: "digest", Type: "mock", Config: []byte(`{}`), Enabled: true,
		DigestMode: store.DigestModeWindow, DigestWindowSeconds: windowSeconds,
	}
}

func TestDispatchWithDigestQueuesInsteadOfSending(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{digestChannel(1, 60)}}
	q := newFakeDigestQueue()
	n := &mockNotifier{}
	d := newTestDispatcher(t, st, n).WithDigestQueue(q)

	d.Dispatch(context.Background(), Alert{ID: 1, MonitorID: 2, MonitorName: "m", EventName: "e"})

	assert.Zero(t, n.calls, "a digest channel must not send immediately")
	require.Len(t, q.rows[1], 1)
}

func TestFlushDigestGroupsAndClearsWindow(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{digestChannel(1, 60)}}
	q := newFakeDigestQueue()
	n := &mockNotifier{}
	d := newTestDispatcher(t, st, n).WithDigestQueue(q)

	for i := 0; i < 3; i++ {
		d.Dispatch(context.Background(), Alert{ID: int64(i + 1), MonitorID: 2, MonitorName: "payments", EventName: "transfer", Ledger: uint32(i)})
	}
	require.Len(t, q.rows[1], 3)

	// Window has not elapsed: nothing is sent and the window is kept.
	d.FlushDigests(context.Background(), time.Now().Add(30*time.Second))
	assert.Zero(t, n.calls)
	require.Len(t, q.rows[1], 3)

	// Window elapsed: one message, and the flushed rows are gone.
	d.FlushDigests(context.Background(), time.Now().Add(2*time.Minute))
	assert.Equal(t, 1, n.calls)
	assert.Contains(t, n.lastSent.Digest, "3 alert(s)")
	assert.Contains(t, n.lastSent.Digest, "payments")
	assert.Empty(t, q.rows[1])
}

// TestDigestSurvivesRestart proves the pending window is not tied to the
// Dispatcher: a fresh one sharing the queue still flushes it.
func TestDigestSurvivesRestart(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{digestChannel(1, 60)}}
	q := newFakeDigestQueue()
	n := &mockNotifier{}
	d := newTestDispatcher(t, st, n).WithDigestQueue(q)
	d.Dispatch(context.Background(), Alert{ID: 1, MonitorName: "m", EventName: "e"})
	require.Len(t, q.rows[1], 1)

	// "Restart": a new dispatcher, same queue.
	d2 := newTestDispatcher(t, st, n).WithDigestQueue(q)
	d2.FlushDigests(context.Background(), time.Now().Add(2*time.Minute))

	assert.Equal(t, 1, n.calls, "a restart must not drop the pending window")
	assert.Empty(t, q.rows[1])
}

func TestEmptyWindowSendsNothing(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{digestChannel(1, 1)}}
	n := &mockNotifier{}
	d := newTestDispatcher(t, st, n).WithDigestQueue(newFakeDigestQueue())

	d.FlushDigests(context.Background(), time.Now().Add(time.Hour))
	assert.Zero(t, n.calls)
}

func TestDigestDisabledByDefault(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{mockChannel(1)}}
	n := &mockNotifier{}
	d := newTestDispatcher(t, st, n).WithDigestQueue(newFakeDigestQueue())

	d.Dispatch(context.Background(), Alert{ID: 1, MonitorID: 2})
	assert.Equal(t, 1, n.calls, "a channel with no digest settings delivers immediately")
}

func TestRenderDigestGroupsTruncatesAndEmpty(t *testing.T) {
	assert.Empty(t, RenderDigest(nil, 0))

	alerts := []Alert{
		{MonitorName: "a", EventName: "one", Ledger: 1},
		{MonitorName: "a", EventName: "two", Ledger: 2},
		{MonitorName: "b", EventName: "three", Ledger: 3},
	}
	got := RenderDigest(alerts, 0)
	assert.Contains(t, got, "3 alert(s) across 2 monitor(s)")
	assert.Contains(t, got, "a (2)")
	assert.Contains(t, got, "b (1)")

	// A tiny maximum truncates to whole lines and says how many were dropped.
	var many []Alert
	for i := 0; i < 50; i++ {
		many = append(many, Alert{MonitorName: "m", EventName: strings.Repeat("x", 40), Ledger: uint32(i)})
	}
	got = RenderDigest(many, 300)
	assert.Contains(t, got, "and ")
	assert.Contains(t, got, "more")
	assert.LessOrEqual(t, len(got), 400, "truncation must stay near the documented maximum")
}

func TestRenderTextReturnsDigestVerbatim(t *testing.T) {
	got, err := RenderText(Alert{Digest: "summary text"})
	require.NoError(t, err)
	assert.Equal(t, "summary text", got)
}

// syncMockNotifier is a concurrency-safe recording notifier for background
// RunDigestFlusher tests under -race.
type syncMockNotifier struct {
	mu       sync.Mutex
	err      error
	calls    int
	lastSent Alert
	sentCh   chan Alert
}

func newSyncMockNotifier(err error) *syncMockNotifier {
	return &syncMockNotifier{
		err:    err,
		sentCh: make(chan Alert, 16),
	}
}

func (m *syncMockNotifier) Send(_ context.Context, a Alert) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	m.lastSent = a
	if m.err != nil {
		return m.err
	}
	select {
	case m.sentCh <- a:
	default:
	}
	return nil
}

func (m *syncMockNotifier) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *syncMockNotifier) setError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

func TestRunDigestFlusherLoopWithInjectedClockAndMetrics(t *testing.T) {
	const secretConfig = `{"webhook_url":"https://hooks.slack.com/services/T000/B000/SUPER_SECRET_WEBHOOK_KEY"}`
	ch := digestChannel(1, 60)
	ch.Config = []byte(secretConfig)

	st := &fakeDispatchStore{channels: []store.Channel{ch}}
	q := newFakeDigestQueue()
	n := newSyncMockNotifier(nil)

	var logBuf bytes.Buffer
	f := DefaultFactory()
	f.Register("mock", func(json.RawMessage) (Notifier, error) { return n, nil })
	m := metrics.New()
	d := NewDispatcher(st, f, slog.New(slog.NewTextHandler(&logBuf, nil))).
		WithDigestQueue(q).
		WithMetrics(m).
		WithDisableAfterFailures(5)

	assert.Equal(t, 5, d.DisableAfterFailures())

	// Advance the injected clock past the 60s digest window so the first tick flushes.
	d.WithClock(func() time.Time { return time.Now().Add(2 * time.Minute) })

	d.Dispatch(context.Background(), Alert{ID: 1, MonitorID: 2, MonitorName: "payments", EventName: "transfer"})
	d.Dispatch(context.Background(), Alert{ID: 2, MonitorID: 2, MonitorName: "payments", EventName: "mint"})
	require.Equal(t, 2, q.count(1))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.RunDigestFlusher(ctx, 5*time.Millisecond)
	}()

	select {
	case sent := <-n.sentCh:
		assert.Contains(t, sent.Digest, "2 alert(s)")
		assert.Contains(t, sent.Digest, "payments")
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("timed out waiting for RunDigestFlusher to flush pending digest")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunDigestFlusher did not exit promptly after context cancellation")
	}

	assert.Equal(t, 0, q.count(1), "flushed rows must be deleted from the queue")
	assert.Contains(t, scrapeMetricsText(t, m), `sorobeacon_alert_deliveries_total{channel="mock",outcome="ok"} 1`)
	assert.NotContains(t, logBuf.String(), "SUPER_SECRET_WEBHOOK_KEY", "logs must never contain channel config")
}

func TestRunDigestFlusherEmptyIntervalAndFailedFlushRetainsBatch(t *testing.T) {
	const secretConfig = `{"bot_token":"telegram-secret-token-999"}`
	ch := digestChannel(1, 60)
	ch.Config = []byte(secretConfig)

	st := &fakeDispatchStore{channels: []store.Channel{ch}}
	q := newFakeDigestQueue()
	n := newSyncMockNotifier(errors.New("503 service unavailable"))

	var logBuf bytes.Buffer
	f := DefaultFactory()
	f.Register("mock", func(json.RawMessage) (Notifier, error) { return n, nil })
	m := metrics.New()
	d := NewDispatcher(st, f, slog.New(slog.NewTextHandler(&logBuf, nil))).
		WithDigestQueue(q).
		WithMetrics(m).
		WithClock(func() time.Time { return time.Now().Add(2 * time.Minute) })

	// 1. Empty queue produces no notification across ticks.
	ctxEmpty, cancelEmpty := context.WithCancel(context.Background())
	doneEmpty := make(chan struct{})
	go func() {
		defer close(doneEmpty)
		d.RunDigestFlusher(ctxEmpty, 5*time.Millisecond)
	}()
	time.Sleep(20 * time.Millisecond)
	cancelEmpty()
	<-doneEmpty
	assert.Zero(t, n.callCount(), "an empty digest queue must not send any message")

	// 2. Failed flush retains the batch in the queue for the next attempt and records failure metric.
	d.Dispatch(context.Background(), Alert{ID: 10, MonitorID: 1, MonitorName: "vault", EventName: "withdraw"})
	require.Equal(t, 1, q.count(1))

	d.FlushDigests(context.Background(), time.Now().Add(2*time.Minute))
	assert.Equal(t, 1, n.callCount())
	assert.Equal(t, 1, q.count(1), "a failed digest flush must retain the batch for the next tick")
	assert.Contains(t, scrapeMetricsText(t, m), `sorobeacon_alert_deliveries_total{channel="mock",outcome="error"} 1`)

	// 3. Subsequent tick after channel recovery delivers the retained batch and clears it.
	n.setError(nil)
	d.FlushDigests(context.Background(), time.Now().Add(2*time.Minute))
	assert.Equal(t, 2, n.callCount())
	assert.Equal(t, 0, q.count(1), "retained batch is cleared once the next flush succeeds")
	assert.NotContains(t, logBuf.String(), "telegram-secret-token-999", "logs must never contain channel config")
}

func TestRunDigestFlusherShutdownPreservesPendingDigestForRestartAndNilQueueExits(t *testing.T) {
	// With no DigestQueue attached, RunDigestFlusher returns immediately.
	st := &fakeDispatchStore{channels: []store.Channel{digestChannel(1, 60)}}
	n := newSyncMockNotifier(nil)
	dNoQueue := newTestDispatcher(t, st, n)
	doneNoQueue := make(chan struct{})
	go func() {
		defer close(doneNoQueue)
		dNoQueue.RunDigestFlusher(context.Background(), 5*time.Millisecond)
	}()
	select {
	case <-doneNoQueue:
	case <-time.After(time.Second):
		t.Fatal("RunDigestFlusher without a DigestQueue must return immediately")
	}

	// When ctx is cancelled before the window elapses, RunDigestFlusher exits
	// without delivering after shutdown and leaves the persisted batch in
	// DigestQueue so the next process flushes it on restart.
	q := newFakeDigestQueue()
	d := newTestDispatcher(t, st, n).
		WithDigestQueue(q).
		WithClock(time.Now) // window not yet elapsed

	d.Dispatch(context.Background(), Alert{ID: 1, MonitorID: 1, MonitorName: "core", EventName: "upgrade"})
	require.Equal(t, 1, q.count(1))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled context
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.RunDigestFlusher(ctx, 5*time.Millisecond)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunDigestFlusher must exit immediately when context is cancelled")
	}
	assert.Zero(t, n.callCount(), "flusher must not deliver after shutdown has begun")
	assert.Equal(t, 1, q.count(1), "pending digest remains persisted in the queue across shutdown")
}

func TestDisableAfterFailuresThresholdAndPendingDigestPreservation(t *testing.T) {
	const secretConfig = `{"webhook_url":"https://hooks.example.com/secret-endpoint-token"}`
	ch := mockChannel(1)
	ch.Config = []byte(secretConfig)

	st := &fakeDispatchStore{channels: []store.Channel{ch}}
	q := newFakeDigestQueue()
	n := &mockNotifier{
		failures: 10,
		err:      &StatusError{Code: 401, Snippet: "unauthorized token"},
	}

	var logBuf bytes.Buffer
	f := DefaultFactory()
	f.Register("mock", func(json.RawMessage) (Notifier, error) { return n, nil })
	d := NewDispatcher(st, f, slog.New(slog.NewTextHandler(&logBuf, nil))).
		WithDigestQueue(q).
		WithDisableAfterFailures(2)
	d.MaxAttempts = 1
	d.BaseBackoff = time.Millisecond

	// Queue a pending digest on channel 1 before immediate deliveries trigger auto-disable.
	require.NoError(t, q.PushDigestAlert(context.Background(), 1, json.RawMessage(`{"id":99,"monitor_name":"m","event_name":"e"}`)))

	d.Dispatch(context.Background(), Alert{ID: 1, MonitorID: 1, MonitorName: "m", EventName: "e1"})
	d.Dispatch(context.Background(), Alert{ID: 2, MonitorID: 1, MonitorName: "m", EventName: "e2"})

	require.Len(t, st.health, 2)
	for _, hc := range st.health {
		assert.Equal(t, int64(1), hc.channelID)
		assert.True(t, hc.update.Permanent, "401 status is a permanent failure")
		assert.Equal(t, 2, hc.update.DisableAfter, "DisableAfter threshold must be propagated to store.RecordChannelHealth")
	}
	require.Len(t, st.deadLetters, 2, "permanently failed deliveries are recorded in dead letters")
	assert.Equal(t, 1, q.count(1), "pending digest for a channel remains accounted for in DigestQueue")
	assert.NotContains(t, logBuf.String(), "secret-endpoint-token", "logs must never contain channel config")
}
