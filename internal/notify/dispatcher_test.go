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

// mockNotifier fails a scripted number of times, then succeeds. errs, when
// set, is a per-call script that takes precedence — it is how a test gets two
// different failure kinds out of one delivery.
type mockNotifier struct {
	failures int
	err      error
	errs     []error
	calls    int
	lastSent Alert
}

func (m *mockNotifier) Send(_ context.Context, a Alert) error {
	m.calls++
	m.lastSent = a
	if len(m.errs) > 0 {
		if i := m.calls - 1; i < len(m.errs) {
			return m.errs[i]
		}
		return nil
	}
	if m.calls <= m.failures {
		if m.err != nil {
			return m.err
		}
		return errors.New("boom")
	}
	return nil
}

// channelHealthCall is one RecordChannelHealth call, kept so a test can assert
// what the dispatcher told the store about a channel's health.
type channelHealthCall struct {
	channelID int64
	update    store.ChannelHealthUpdate
}

// fakeDispatchStore implements DispatchStore in memory. It records health
// updates verbatim: the counters themselves live in SQL, so their arithmetic is
// covered against a real database in internal/store/postgres_test.go, and what
// matters here is which outcomes the dispatcher reports.
type fakeDispatchStore struct {
	channels []store.Channel
	attempts []store.DeliveryAttempt
	health   []channelHealthCall
}

func (f *fakeDispatchStore) ListChannelsForMonitor(_ context.Context, _ int64) ([]store.Channel, error) {
	return f.channels, nil
}

func (f *fakeDispatchStore) RecordDeliveryAttempt(_ context.Context, d *store.DeliveryAttempt) error {
	f.attempts = append(f.attempts, *d)
	return nil
}

func (f *fakeDispatchStore) RecordChannelHealth(_ context.Context, channelID int64, u store.ChannelHealthUpdate) error {
	f.health = append(f.health, channelHealthCall{channelID: channelID, update: u})
	return nil
}

// healthFor returns the health updates recorded for one channel.
func healthFor(st *fakeDispatchStore, channelID int64) []store.ChannelHealthUpdate {
	var out []store.ChannelHealthUpdate
	for _, h := range st.health {
		if h.channelID == channelID {
			out = append(out, h.update)
		}
	}
	return out
}

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

func TestDispatchRecordsHealthOnSuccess(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{mockChannel(1)}}
	d := newTestDispatcher(t, st, &mockNotifier{})

	d.Dispatch(context.Background(), Alert{ID: 30, MonitorID: 2})

	updates := healthFor(st, 1)
	require.Len(t, updates, 1)
	assert.True(t, updates[0].Success, "a delivered alert must clear the channel's failure state")
	assert.Empty(t, updates[0].Error)
}

// Health counts deliveries, not attempts. A threshold of 3 must not park a
// channel because one alert was retried three times.
func TestDispatchCountsOneFailurePerDeliveryNotPerAttempt(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{mockChannel(1)}}
	n := &mockNotifier{failures: 99}
	d := newTestDispatcher(t, st, n)

	d.Dispatch(context.Background(), Alert{ID: 31, MonitorID: 2})

	assert.Equal(t, d.MaxAttempts, n.calls)
	assert.Equal(t, d.MaxAttempts, len(st.attempts), "every attempt is still recorded")
	updates := healthFor(st, 1)
	require.Len(t, updates, 1, "one delivery is one health outcome")
	assert.False(t, updates[0].Success)
	assert.Contains(t, updates[0].Error, "boom")
}

func TestDispatchClassifiesFailures(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		permanent bool
	}{
		{
			name:      "401 means the credential is gone",
			err:       &HTTPStatusError{StatusCode: 401, Body: "unauthorized"},
			permanent: true,
		},
		{
			name:      "403 means the destination is gone",
			err:       &HTTPStatusError{StatusCode: 403, Body: "forbidden"},
			permanent: true,
		},
		{
			name:      "404 means the endpoint is gone",
			err:       &HTTPStatusError{StatusCode: 404, Body: "no such webhook"},
			permanent: true,
		},
		{
			name: "500 is the provider having a bad day",
			err:  &HTTPStatusError{StatusCode: 500, Body: "oops"},
		},
		{
			name: "429 is rate limiting",
			err:  &HTTPStatusError{StatusCode: 429, Body: "slow down"},
		},
		{
			name: "a transport error is transient",
			err:  errors.New("post: dial tcp: connection refused"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &fakeDispatchStore{channels: []store.Channel{mockChannel(1)}}
			n := &mockNotifier{failures: 99, err: tt.err}
			d := newTestDispatcher(t, st, n).WithDisableAfterFailures(4)

			d.Dispatch(context.Background(), Alert{ID: 32, MonitorID: 2})

			updates := healthFor(st, 1)
			require.Len(t, updates, 1)
			assert.Equal(t, tt.permanent, updates[0].Permanent)
			assert.Equal(t, 4, updates[0].DisableAfter,
				"the configured threshold must reach the store that enforces it")
			assert.Contains(t, updates[0].Error, tt.err.Error())
		})
	}
}

// A revoked credential followed by an unrelated 5xx is still a revoked
// credential, so one permanent attempt makes the whole delivery permanent.
func TestDispatchTreatsMixedAttemptsAsPermanent(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{mockChannel(1)}}
	n := &mockNotifier{errs: []error{
		&HTTPStatusError{StatusCode: 503, Body: "unavailable"},
		&HTTPStatusError{StatusCode: 401, Body: "unauthorized"},
	}}
	d := newTestDispatcher(t, st, n)
	d.MaxAttempts = 2

	d.Dispatch(context.Background(), Alert{ID: 33, MonitorID: 2})

	updates := healthFor(st, 1)
	require.Len(t, updates, 1)
	assert.True(t, updates[0].Permanent)
}

// Auto-disable is off by default, and the dispatcher passes that through
// rather than inventing a threshold of its own.
func TestDispatchDefaultNeverAutoDisables(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{mockChannel(1)}}
	n := &mockNotifier{failures: 99, err: &HTTPStatusError{StatusCode: 404, Body: "gone"}}
	d := newTestDispatcher(t, st, n)

	d.Dispatch(context.Background(), Alert{ID: 34, MonitorID: 2})

	updates := healthFor(st, 1)
	require.Len(t, updates, 1)
	assert.Zero(t, updates[0].DisableAfter, "unset CHANNEL_DISABLE_AFTER_FAILURES must disable nothing")
}

func TestDispatchBadConfigCountsAsPermanent(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{
		{ID: 5, Type: "nope", Config: json.RawMessage(`{}`), Enabled: true},
	}}
	d := newTestDispatcher(t, st, &mockNotifier{})

	d.Dispatch(context.Background(), Alert{ID: 35, MonitorID: 2})

	updates := healthFor(st, 5)
	require.Len(t, updates, 1)
	assert.True(t, updates[0].Permanent,
		"a config that no longer builds notifiers never heals on its own")
}

// A delivery interrupted by shutdown says nothing about the channel, and
// counting it would let a deploy disable a channel that is working fine.
func TestDispatchShutdownLeavesHealthAlone(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{mockChannel(1)}}
	n := &mockNotifier{failures: 99}
	d := newTestDispatcher(t, st, n)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.Dispatch(ctx, Alert{ID: 36, MonitorID: 2})

	assert.Empty(t, healthFor(st, 1))
}

func TestRetryRecordsHealth(t *testing.T) {
	t.Run("success clears the failure state", func(t *testing.T) {
		st := &fakeDispatchStore{}
		d := newTestDispatcher(t, st, &mockNotifier{})

		d.Retry(context.Background(), Alert{ID: 40}, mockChannel(3))

		updates := healthFor(st, 3)
		require.Len(t, updates, 1)
		assert.True(t, updates[0].Success)
	})

	t.Run("a still-broken channel keeps climbing", func(t *testing.T) {
		st := &fakeDispatchStore{}
		n := &mockNotifier{failures: 99, err: &HTTPStatusError{StatusCode: 403, Body: "forbidden"}}
		d := newTestDispatcher(t, st, n)

		d.Retry(context.Background(), Alert{ID: 41}, mockChannel(3))

		updates := healthFor(st, 3)
		require.Len(t, updates, 1)
		assert.False(t, updates[0].Success)
		assert.True(t, updates[0].Permanent)
	})
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
