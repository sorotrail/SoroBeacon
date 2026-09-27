package notify

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// fakeDigestQueue is an in-memory DigestQueue. A second Dispatcher sharing the
// same queue stands in for a restart: the rows outlive the dispatcher.
type fakeDigestQueue struct {
	seq  int64
	rows map[int64][]store.DigestAlert
}

func newFakeDigestQueue() *fakeDigestQueue {
	return &fakeDigestQueue{rows: map[int64][]store.DigestAlert{}}
}

func (q *fakeDigestQueue) PushDigestAlert(_ context.Context, channelID int64, payload json.RawMessage) error {
	q.seq++
	row := store.DigestAlert{ID: q.seq, ChannelID: channelID, Payload: payload, CreatedAt: time.Now()}
	q.rows[channelID] = append(q.rows[channelID], row)
	return nil
}

func (q *fakeDigestQueue) ListDigestAlerts(_ context.Context, channelID int64) ([]store.DigestAlert, error) {
	return q.rows[channelID], nil
}

func (q *fakeDigestQueue) DeleteDigestAlerts(_ context.Context, channelID int64, ids []int64) error {
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
