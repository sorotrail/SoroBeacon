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

	"github.com/sorotrail/sorobeacon/internal/alerts"
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
	// deadLetters records the dead letters the dispatcher wrote.
	deadLetters []store.DeadLetter
	// window, when set, is returned by ActiveMaintenanceWindow.
	window *store.MaintenanceWindow
	// suppressed records the alert ID and reason of each suppression.
	suppressed []string
	// inhibitions feeds the inhibition check; firing answers RuleFiredWithin.
	inhibitions []store.Inhibition
	firing      map[int64]bool
	alertTimes  map[int64][]time.Time
	inhibited   []inhibitedMark
}

type inhibitedMark struct {
	alertID  int64
	sourceID int64
}

func (f *fakeDispatchStore) ListChannelsForMonitor(_ context.Context, _ int64) ([]store.Channel, error) {
	return f.channels, nil
}

func (f *fakeDispatchStore) RecordDeliveryAttempt(_ context.Context, d *store.DeliveryAttempt) error {
	f.attempts = append(f.attempts, *d)
	return nil
}

func (f *fakeDispatchStore) CreateDeadLetter(_ context.Context, d *store.DeadLetter) error {
	f.deadLetters = append(f.deadLetters, *d)
	return nil
}

func (f *fakeDispatchStore) GetDeadLetter(_ context.Context, _ int64) (*store.DeadLetter, error) {
	return nil, store.ErrNotFound
}

func (f *fakeDispatchStore) ListDeadLetters(_ context.Context, _ store.DeadLetterFilter) ([]store.DeadLetter, error) {
	return nil, nil
}

func (f *fakeDispatchStore) DeleteDeadLetter(_ context.Context, _ int64) error {
	return nil
}

func (f *fakeDispatchStore) GetAlert(_ context.Context, _ int64) (*store.Alert, error) {
	return nil, store.ErrNotFound
}

func (f *fakeDispatchStore) GetChannel(_ context.Context, _ int64) (*store.Channel, error) {
	return nil, store.ErrNotFound
}

func (f *fakeDispatchStore) ListDeliveryAttempts(_ context.Context, _ int64, _ string) ([]store.DeliveryAttempt, error) {
	return nil, nil
}

func (f *fakeDispatchStore) ActiveMaintenanceWindow(_ context.Context, _ int64, _ string, _ time.Time) (*store.MaintenanceWindow, error) {
	return f.window, nil
}

func (f *fakeDispatchStore) SetAlertSuppressed(_ context.Context, _ int64, reason string) error {
	f.suppressed = append(f.suppressed, reason)
	return nil
}

func (f *fakeDispatchStore) ListChannels(_ context.Context, _ bool) ([]store.Channel, error) {
	return f.channels, nil
}

func (f *fakeDispatchStore) ListInhibitionsForTarget(_ context.Context, target int64) ([]store.Inhibition, error) {
	var out []store.Inhibition
	for _, in := range f.inhibitions {
		if in.TargetRuleID == target {
			out = append(out, in)
		}
	}
	return out, nil
}

func (f *fakeDispatchStore) RuleFiredWithin(_ context.Context, ruleID int64, _ time.Duration) (bool, error) {
	return f.firing[ruleID], nil
}

func (f *fakeDispatchStore) ListRuleAlertTimes(_ context.Context, ruleID int64, _ time.Time) ([]time.Time, error) {
	return f.alertTimes[ruleID], nil
}

func (f *fakeDispatchStore) MarkAlertInhibited(_ context.Context, alertID, sourceID int64) error {
	f.inhibited = append(f.inhibited, inhibitedMark{alertID: alertID, sourceID: sourceID})
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

func TestDispatchDampsFlappingRule(t *testing.T) {
	base := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	st := &fakeDispatchStore{
		channels:   []store.Channel{mockChannel(1)},
		alertTimes: map[int64][]time.Time{},
	}
	n := &mockNotifier{}
	d := newTestDispatcher(t, st, n).WithFlapping(alerts.FlappingConfig{
		Episodes: 3, Window: 15 * time.Minute, QuietAfter: 2 * time.Minute,
	}).WithClock(func() time.Time { return base.Add(11 * time.Minute) })

	alertTimes := []time.Time{
		base,
		base.Add(5 * time.Minute),
		base.Add(10 * time.Minute),
		base.Add(10*time.Minute + 30*time.Second),
		base.Add(11 * time.Minute),
	}
	for i, at := range alertTimes {
		st.alertTimes[9] = append(st.alertTimes[9], at)
		d.Dispatch(context.Background(), Alert{
			ID: int64(i + 1), MonitorID: 2, RuleID: 9, CreatedAt: at,
		})
	}

	require.Equal(t, 4, n.calls, "episode starts and one summary are delivered")
	assert.Contains(t, n.lastSent.Digest, "4 alerts in the last 15 minutes across 3 episodes")
	require.Len(t, st.suppressed, 1)
	assert.Contains(t, st.suppressed[0], "flapping")
	require.Len(t, st.attempts, 4)
	assert.Len(t, st.alertTimes[9], 5, "all alert history remains available to detection")
}

func TestDispatchNonFlappingRuleUnchanged(t *testing.T) {
	base := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	st := &fakeDispatchStore{
		channels:   []store.Channel{mockChannel(1)},
		alertTimes: map[int64][]time.Time{9: {base}},
	}
	n := &mockNotifier{}
	d := newTestDispatcher(t, st, n)
	alert := Alert{ID: 20, MonitorID: 2, RuleID: 9, CreatedAt: base.Add(time.Minute)}

	d.Dispatch(context.Background(), alert)

	require.Equal(t, 1, n.calls)
	assert.Equal(t, alert, n.lastSent)
	require.Len(t, st.attempts, 1)
	assert.Empty(t, st.suppressed)
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

func TestDispatchSuppressedByMaintenanceWindow(t *testing.T) {
	st := &fakeDispatchStore{
		channels: []store.Channel{mockChannel(1)},
		window:   &store.MaintenanceWindow{ID: 9, Reason: "upgrade", Scope: store.MaintenanceScopeGlobal},
	}
	n := &mockNotifier{}
	d := newTestDispatcher(t, st, n)

	d.Dispatch(context.Background(), Alert{ID: 15, MonitorID: 2})

	assert.Equal(t, 0, n.calls, "a suppressed alert must not be delivered")
	assert.Empty(t, st.attempts, "no delivery attempt should be recorded")
	require.Equal(t, []string{"upgrade"}, st.suppressed, "the alert is marked with the window reason")
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

func TestDispatchInhibitedSkipsDeliveryAndMarks(t *testing.T) {
	st := &fakeDispatchStore{
		channels: []store.Channel{mockChannel(1)},
		inhibitions: []store.Inhibition{
			{SourceRuleID: 7, TargetRuleID: 9, FiringWindowSeconds: 300},
		},
		firing: map[int64]bool{7: true},
	}
	n := &mockNotifier{}
	d := newTestDispatcher(t, st, n)

	d.Dispatch(context.Background(), Alert{ID: 20, MonitorID: 2, RuleID: 9})

	assert.Equal(t, 0, n.calls, "inhibited alert must not reach any notifier")
	assert.Empty(t, st.attempts, "inhibited alert records no delivery attempts")
	require.Len(t, st.inhibited, 1, "the suppression must be recorded on the alert")
	assert.Equal(t, int64(20), st.inhibited[0].alertID)
	assert.Equal(t, int64(7), st.inhibited[0].sourceID)
}

func TestDispatchQuietSourceDelivers(t *testing.T) {
	st := &fakeDispatchStore{
		channels: []store.Channel{mockChannel(1)},
		inhibitions: []store.Inhibition{
			{SourceRuleID: 7, TargetRuleID: 9, FiringWindowSeconds: 300},
		},
		firing: map[int64]bool{7: false},
	}
	n := &mockNotifier{}
	d := newTestDispatcher(t, st, n)

	d.Dispatch(context.Background(), Alert{ID: 21, MonitorID: 2, RuleID: 9})

	assert.Equal(t, 1, n.calls, "a quiet source must not suppress delivery")
	assert.Empty(t, st.inhibited)
}

// timeoutNotifier captures the context deadline it was called with.
type timeoutNotifier struct {
	gotDeadline time.Time
	hasDeadline bool
}

func (n *timeoutNotifier) Send(ctx context.Context, _ Alert) error {
	dl, ok := ctx.Deadline()
	n.hasDeadline = ok
	n.gotDeadline = dl
	return nil
}

func TestDispatchAppliesPerChannelTimeout(t *testing.T) {
	ch := mockChannel(1)
	ch.Timeout = 5 * time.Second
	st := &fakeDispatchStore{channels: []store.Channel{ch}}
	n := &timeoutNotifier{}
	d := newTestDispatcher(t, st, n)

	before := time.Now()
	d.Dispatch(context.Background(), Alert{ID: 15, MonitorID: 2})

	require.True(t, n.hasDeadline, "Send must receive a context with deadline")
	assert.WithinDuration(t, before.Add(5*time.Second), n.gotDeadline, 500*time.Millisecond)
}

func TestDispatchAppliesDefaultTimeoutWhenUnset(t *testing.T) {
	ch := mockChannel(1)
	ch.Timeout = 0 // unset
	st := &fakeDispatchStore{channels: []store.Channel{ch}}
	n := &timeoutNotifier{}
	d := newTestDispatcher(t, st, n)

	before := time.Now()
	d.Dispatch(context.Background(), Alert{ID: 16, MonitorID: 2})

	require.True(t, n.hasDeadline, "Send must receive a context with deadline")
	assert.WithinDuration(t, before.Add(DefaultTimeout), n.gotDeadline, 500*time.Millisecond)
}

func TestRetryAppliesPerChannelTimeout(t *testing.T) {
	ch := mockChannel(1)
	ch.Timeout = 8 * time.Second
	st := &fakeDispatchStore{channels: []store.Channel{ch}}
	n := &timeoutNotifier{}
	d := newTestDispatcher(t, st, n)

	before := time.Now()
	da := d.Retry(context.Background(), Alert{ID: 17, MonitorID: 2}, ch)

	require.NotNil(t, da)
	assert.Equal(t, "success", da.Status)
	require.True(t, n.hasDeadline, "Retry must apply channel timeout to ctx")
	assert.WithinDuration(t, before.Add(8*time.Second), n.gotDeadline, 500*time.Millisecond)
}

func TestDispatchDisableAfterFailuresBasic(t *testing.T) {
	// Default (0) never auto-disables
	st := &fakeDispatchStore{channels: []store.Channel{mockChannel(1)}}
	n := &mockNotifier{failures: 99, err: &HTTPStatusError{StatusCode: 404, Body: "gone"}}
	d := newTestDispatcher(t, st, n)

	d.Dispatch(context.Background(), Alert{ID: 34, MonitorID: 2})

	updates := healthFor(st, 1)
	require.Len(t, updates, 1)
	assert.Zero(t, updates[0].DisableAfter, "default (0) never auto-disables")
}

// Threshold 4 with 404 permanent failures - after 4 failures, channel auto-disables
func TestDispatchDisableAfterFailuresThreshold(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{mockChannel(1)}}
	n := &mockNotifier{failures: 99, err: &HTTPStatusError{StatusCode: 404, Body: "gone"}}
	d := newTestDispatcher(t, st, n)
	d.WithDisableAfterFailures(4)

	// Each dispatch records health with DisableAfter=4 (the threshold),
	// but the auto-disable behavior depends on whether the channel has
	// already accumulated enough permanent failures in the store.
	// First dispatch: health recorded with DisableAfter=4
	d.Dispatch(context.Background(), Alert{ID: 34, MonitorID: 2})
	updates := healthFor(st, 1)
	require.Len(t, updates, 1)
	// The threshold value is always 4 when configured; what matters is
	// whether the store's auto-disable logic triggers after N permanent failures.
	assert.Equal(t, 4, updates[0].DisableAfter, "DisableAfter is set to the configured threshold")

	// A 404 is not retried — the receiver rejected the request itself, and
	// sending it again unchanged fails the same way — so a delivery is one
	// attempt and the circuit breaker counts one failure per delivery. The
	// second and third dispatches still reach the channel.
	d.Dispatch(context.Background(), Alert{ID: 35, MonitorID: 2})
	updates = healthFor(st, 1)
	require.Len(t, updates, 2)
	assert.Equal(t, 4, updates[1].DisableAfter, "DisableAfter remains at threshold value")

	// Third dispatch
	d.Dispatch(context.Background(), Alert{ID: 36, MonitorID: 2})
	updates = healthFor(st, 1)
	require.Len(t, updates, 3)
	assert.Equal(t, 4, updates[2].DisableAfter, "DisableAfter remains at threshold value")

	// Fourth dispatch: the breaker is open by now, so the channel is skipped
	// and the health is left alone.
	d.Dispatch(context.Background(), Alert{ID: 37, MonitorID: 2})
	updates = healthFor(st, 1)
	require.Len(t, updates, 3)
	assert.Equal(t, 4, updates[2].DisableAfter, "DisableAfter remains at threshold value")

	// Fifth dispatch
	d.Dispatch(context.Background(), Alert{ID: 38, MonitorID: 2})
	updates = healthFor(st, 1)
	require.Len(t, updates, 3)
	assert.Equal(t, 4, updates[0].DisableAfter, "DisableAfter remains at threshold value")
}

// WithMetrics(nil) should be nil-safe
func TestDispatchWithMetricsNilSafe(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{mockChannel(1)}}
	n := &mockNotifier{}
	d := newTestDispatcher(t, st, n)
	d.WithMetrics(nil) // should not panic

	d.Dispatch(context.Background(), Alert{ID: 39, MonitorID: 2})
	assert.Equal(t, 1, n.calls, "dispatch with nil metrics should still work")
}
