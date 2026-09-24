package notify

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// mockNotifier fails a scripted number of times, then succeeds.
type mockNotifier struct {
	failures int
	calls    int
	lastSent Alert
}

func (m *mockNotifier) Send(_ context.Context, a Alert) error {
	m.calls++
	m.lastSent = a
	if m.calls <= m.failures {
		return errors.New("boom")
	}
	return nil
}

// fakeDispatchStore implements DispatchStore in memory.
type fakeDispatchStore struct {
	channels []store.Channel
	attempts []store.DeliveryAttempt
}

func (f *fakeDispatchStore) ListChannelsForMonitor(_ context.Context, _ int64) ([]store.Channel, error) {
	return f.channels, nil
}

func (f *fakeDispatchStore) RecordDeliveryAttempt(_ context.Context, d *store.DeliveryAttempt) error {
	f.attempts = append(f.attempts, *d)
	return nil
}

func (f *fakeDispatchStore) ListChannelsByIDs(_ context.Context, ids []int64) ([]store.Channel, error) {
	wanted := make(map[int64]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	var out []store.Channel
	for _, ch := range f.channels {
		if wanted[ch.ID] && ch.Enabled {
			out = append(out, ch)
		}
	}
	return out, nil
}

// The dispatch tests exercise the flat fan-out, so no policy is attached.
func (f *fakeDispatchStore) GetEscalationPolicyForMonitor(context.Context, int64) (*store.EscalationPolicy, error) {
	return nil, store.ErrNotFound
}

func (f *fakeDispatchStore) GetEscalationPolicy(context.Context, int64) (*store.EscalationPolicy, error) {
	return nil, store.ErrNotFound
}

func (f *fakeDispatchStore) ScheduleEscalation(context.Context, int64, int64, json.RawMessage, int, time.Time) error {
	return nil
}

func (f *fakeDispatchStore) DueEscalations(context.Context, time.Time, int) ([]store.EscalationRun, error) {
	return nil, nil
}

func (f *fakeDispatchStore) AdvanceEscalation(context.Context, int64, int, time.Time) error {
	return nil
}

func (f *fakeDispatchStore) CompleteEscalation(context.Context, int64) error { return nil }

func newTestDispatcher(t *testing.T, st *fakeDispatchStore, n Notifier) *Dispatcher {
	t.Helper()
	f := &Factory{constructors: map[string]Constructor{}}
	f.Register("mock", func(json.RawMessage) (Notifier, error) { return n, nil })
	d := NewDispatcher(st, f, slog.New(slog.DiscardHandler))
	d.BaseBackoff = time.Millisecond
	return d
}

func mockChannel(id int64) store.Channel {
	return store.Channel{ID: id, Name: "mock", Type: "mock", Config: json.RawMessage(`{}`), Enabled: true}
}

func TestDispatchSuccessFirstTry(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{mockChannel(1)}}
	n := &mockNotifier{}
	d := newTestDispatcher(t, st, n)

	alert := Alert{ID: 10, MonitorID: 2, MonitorName: "m"}
	d.Dispatch(context.Background(), alert)

	assert.Equal(t, 1, n.calls)
	assert.Equal(t, alert, n.lastSent)
	require.Len(t, st.attempts, 1)
	assert.Equal(t, "success", st.attempts[0].Status)
	assert.Equal(t, int64(10), st.attempts[0].AlertID)
	assert.Equal(t, int64(1), st.attempts[0].ChannelID)
}

func TestDispatchRetriesThenSucceeds(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{mockChannel(1)}}
	n := &mockNotifier{failures: 2}
	d := newTestDispatcher(t, st, n)

	d.Dispatch(context.Background(), Alert{ID: 11, MonitorID: 2})

	assert.Equal(t, 3, n.calls)
	require.Len(t, st.attempts, 3, "every attempt must be recorded")
	assert.Equal(t, "failed", st.attempts[0].Status)
	assert.Equal(t, "failed", st.attempts[1].Status)
	assert.Equal(t, "success", st.attempts[2].Status)
	assert.Contains(t, st.attempts[0].ResponseSnippet, "boom")
}

func TestDispatchGivesUpAfterMaxAttempts(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{mockChannel(1)}}
	n := &mockNotifier{failures: 99}
	d := newTestDispatcher(t, st, n)

	d.Dispatch(context.Background(), Alert{ID: 12, MonitorID: 2})

	assert.Equal(t, d.MaxAttempts, n.calls)
	require.Len(t, st.attempts, d.MaxAttempts)
	for _, a := range st.attempts {
		assert.Equal(t, "failed", a.Status)
	}
}

func TestDispatchFansOutToAllChannels(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{mockChannel(1), mockChannel(2)}}
	n := &mockNotifier{}
	d := newTestDispatcher(t, st, n)

	d.Dispatch(context.Background(), Alert{ID: 13, MonitorID: 2})

	assert.Equal(t, 2, n.calls)
	require.Len(t, st.attempts, 2)
	assert.Equal(t, int64(1), st.attempts[0].ChannelID)
	assert.Equal(t, int64(2), st.attempts[1].ChannelID)
}

func TestRetryRecordsNewAttempt(t *testing.T) {
	st := &fakeDispatchStore{}
	n := &mockNotifier{}
	d := newTestDispatcher(t, st, n)

	got := d.Retry(context.Background(), Alert{ID: 20, MonitorID: 2}, mockChannel(3))

	assert.Equal(t, 1, n.calls)
	require.NotNil(t, got)
	assert.Equal(t, "success", got.Status)
	require.Len(t, st.attempts, 1)
	assert.Equal(t, int64(20), st.attempts[0].AlertID)
	assert.Equal(t, int64(3), st.attempts[0].ChannelID)
}

func TestRetryFailedSendStillRecords(t *testing.T) {
	st := &fakeDispatchStore{}
	n := &mockNotifier{failures: 99}
	d := newTestDispatcher(t, st, n)

	got := d.Retry(context.Background(), Alert{ID: 21}, mockChannel(3))

	assert.Equal(t, 1, n.calls, "manual retry is one shot, not the Dispatch backoff loop")
	require.NotNil(t, got)
	assert.Equal(t, "failed", got.Status)
	assert.Contains(t, got.ResponseSnippet, "boom")
}

func TestGateRetry(t *testing.T) {
	ch := mockChannel(3)
	now := time.Date(2026, 9, 22, 2, 0, 0, 0, time.UTC)
	failed := store.DeliveryAttempt{ChannelID: 3, Status: "failed", AttemptedAt: now.Add(-time.Hour)}
	ok := store.DeliveryAttempt{ChannelID: 3, Status: "success", AttemptedAt: now.Add(-time.Hour)}
	recent := store.DeliveryAttempt{ChannelID: 3, Status: "failed", AttemptedAt: now.Add(-time.Second)}
	other := store.DeliveryAttempt{ChannelID: 9, Status: "success", AttemptedAt: now}

	assert.NoError(t, GateRetry([]store.DeliveryAttempt{failed}, 3, ch, now, DefaultRetryCooldown))
	assert.ErrorIs(t, GateRetry([]store.DeliveryAttempt{failed, ok}, 3, ch, now, DefaultRetryCooldown), ErrAlreadySucceeded)
	assert.ErrorIs(t, GateRetry([]store.DeliveryAttempt{failed}, 3, store.Channel{ID: 3, Enabled: false}, now, DefaultRetryCooldown), ErrChannelDisabled)
	assert.ErrorIs(t, GateRetry(nil, 3, ch, now, DefaultRetryCooldown), ErrNoAttempt)
	assert.ErrorIs(t, GateRetry([]store.DeliveryAttempt{other}, 3, ch, now, DefaultRetryCooldown), ErrNoAttempt)
	assert.ErrorIs(t, GateRetry([]store.DeliveryAttempt{recent}, 3, ch, now, DefaultRetryCooldown), ErrRetryCooldown)
	assert.NoError(t, GateRetry([]store.DeliveryAttempt{recent}, 3, ch, now, 0), "zero cooldown disables the bound")
}

func TestDispatchBadConfigRecordsFailure(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{
		{ID: 5, Type: "nope", Config: json.RawMessage(`{}`), Enabled: true},
	}}
	d := newTestDispatcher(t, st, &mockNotifier{})

	d.Dispatch(context.Background(), Alert{ID: 14, MonitorID: 2})

	require.Len(t, st.attempts, 1, "bad config is recorded once, not retried")
	assert.Equal(t, "failed", st.attempts[0].Status)
	assert.Contains(t, st.attempts[0].ResponseSnippet, "unknown channel type")
}
