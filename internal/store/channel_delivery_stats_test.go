package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testChannelDeliveryStats pins the per-channel delivery aggregate behind
// GET /channels/{id}/stats. It is part of the shared conformance suite, so the
// identical assertions run against Postgres (TEST_DATABASE_URL) and SQLite:
// an aggregate that is right on one backend and wrong on the other would make
// the dashboard disagree with the API depending on the deployment.
//
// Attempted times are pinned through the setDeliveryAttemptAt hook rather
// than left to the server clock, so every window and last-timestamp assertion
// below is exact instead of "some time in the last minute".
func testChannelDeliveryStats(t *testing.T, newStore conformanceFactory) {
	t.Helper()
	ctx := context.Background()
	st := newStore(t)

	m := &Monitor{Name: "delivery-stats", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	r := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, r))

	channel := func(name string) *Channel {
		ch := &Channel{Name: name, Type: "webhook", Config: json.RawMessage(`{}`), Enabled: true}
		require.NoError(t, st.CreateChannel(ctx, ch))
		return ch
	}
	alert := func(ev string) *Alert {
		a := &Alert{MonitorID: m.ID, RuleID: r.ID, EventID: ev}
		_, err := st.CreateAlert(ctx, a)
		require.NoError(t, err)
		return a
	}
	record := func(ch *Channel, a *Alert, status string) int64 {
		d := &DeliveryAttempt{AlertID: a.ID, ChannelID: ch.ID, Status: status}
		require.NoError(t, st.RecordDeliveryAttempt(ctx, d))
		return d.ID
	}
	attemptAt := func(id int64, at time.Time) {
		require.NoError(t, st.setDeliveryAttemptAt(ctx, id, at))
	}

	healthy := channel("stats-healthy")
	failing := channel("stats-failing")
	mixed := channel("stats-mixed")
	quiet := channel("stats-quiet")
	other := channel("stats-other")

	now := time.Now()

	// Healthy: two successes, each on its own alert.
	attemptAt(record(healthy, alert("h-1"), DeliveryStatusSuccess), now.Add(-5*time.Hour))
	attemptAt(record(healthy, alert("h-2"), DeliveryStatusSuccess), now.Add(-1*time.Hour))

	// Failing: two failures.
	attemptAt(record(failing, alert("f-1"), DeliveryStatusFailed), now.Add(-4*time.Hour))
	attemptAt(record(failing, alert("f-2"), DeliveryStatusFailed), now.Add(-2*time.Hour))

	// Mixed: success, failure, and a retry — two attempts against the same
	// alert, which count as two attempts because the aggregate counts tries,
	// not unique alerts. The alert struct is reused exactly as the dispatcher
	// would: the dedup guard means a second CreateAlert for the same event
	// does not fill in an id.
	mixedAlert := alert("m-1")
	attemptAt(record(mixed, mixedAlert, DeliveryStatusSuccess), now.Add(-3*time.Hour))
	attemptAt(record(mixed, mixedAlert, DeliveryStatusFailed), now.Add(-25*time.Minute))
	attemptAt(record(mixed, mixedAlert, DeliveryStatusSuccess), now.Add(-10*time.Minute))

	// Out-of-window history: an old failure on the mixed channel and an old
	// success on the healthy one. Neither may appear in a 24h window.
	attemptAt(record(mixed, alert("m-old"), DeliveryStatusFailed), now.Add(-48*time.Hour))
	attemptAt(record(healthy, alert("h-old"), DeliveryStatusSuccess), now.Add(-48*time.Hour))

	// Independent channel: one success, one failure, to prove one channel's
	// aggregate cannot bleed into another's.
	attemptAt(record(other, alert("o-1"), DeliveryStatusSuccess), now.Add(-6*time.Hour))
	attemptAt(record(other, alert("o-2"), DeliveryStatusFailed), now.Add(-6*time.Hour))

	since := now.Add(-24 * time.Hour)

	t.Run("successful attempts only", func(t *testing.T) {
		cs, err := st.GetChannelDeliveryStats(ctx, healthy.ID, since)
		require.NoError(t, err)
		assert.Equal(t, healthy.ID, cs.ChannelID)
		assert.Equal(t, int64(2), cs.Total, "out-of-window attempts must not be counted")
		assert.Equal(t, int64(2), cs.Succeeded)
		assert.Zero(t, cs.Failed)
		require.NotNil(t, cs.SuccessRate)
		assert.InDelta(t, 1.0, *cs.SuccessRate, 1e-9)
		assert.InDelta(t, 100.0, cs.SuccessPercent(), 1e-9)
		require.NotNil(t, cs.LastSuccessAt)
		assert.WithinDuration(t, now.Add(-1*time.Hour), *cs.LastSuccessAt, time.Second)
		assert.Nil(t, cs.LastFailureAt, "no failure in the window means no last-failure timestamp")
	})

	t.Run("failed attempts only", func(t *testing.T) {
		cs, err := st.GetChannelDeliveryStats(ctx, failing.ID, since)
		require.NoError(t, err)
		assert.Equal(t, int64(2), cs.Total)
		assert.Zero(t, cs.Succeeded)
		assert.Equal(t, int64(2), cs.Failed)
		require.NotNil(t, cs.SuccessRate, "a 0% rate is defined; only zero attempts are undefined")
		assert.InDelta(t, 0.0, *cs.SuccessRate, 1e-9)
		assert.Nil(t, cs.LastSuccessAt)
		require.NotNil(t, cs.LastFailureAt)
		assert.WithinDuration(t, now.Add(-2*time.Hour), *cs.LastFailureAt, time.Second)
	})

	t.Run("mixed outcomes with retries and pinned timestamps", func(t *testing.T) {
		cs, err := st.GetChannelDeliveryStats(ctx, mixed.ID, since)
		require.NoError(t, err)
		assert.Equal(t, int64(3), cs.Total, "a retry is another attempt, not another alert")
		assert.Equal(t, int64(2), cs.Succeeded)
		assert.Equal(t, int64(1), cs.Failed)
		require.NotNil(t, cs.SuccessRate)
		assert.InDelta(t, 2.0/3.0, *cs.SuccessRate, 1e-9)
		require.NotNil(t, cs.LastSuccessAt)
		assert.WithinDuration(t, now.Add(-10*time.Minute), *cs.LastSuccessAt, time.Second)
		require.NotNil(t, cs.LastFailureAt)
		assert.WithinDuration(t, now.Add(-25*time.Minute), *cs.LastFailureAt, time.Second)
	})

	t.Run("no attempts is zeroes without a rate", func(t *testing.T) {
		cs, err := st.GetChannelDeliveryStats(ctx, quiet.ID, since)
		require.NoError(t, err)
		assert.Zero(t, cs.Total)
		assert.Zero(t, cs.Succeeded)
		assert.Zero(t, cs.Failed)
		assert.Nil(t, cs.SuccessRate, "an undefined rate must never become 0 or 1")
		assert.False(t, cs.HasAttempts())
		assert.Equal(t, -1.0, cs.SuccessPercent(), "the display sentinel for \"no attempts\"")
		assert.Nil(t, cs.LastSuccessAt)
		assert.Nil(t, cs.LastFailureAt)
	})

	t.Run("multiple channels keep independent statistics", func(t *testing.T) {
		cs, err := st.GetChannelDeliveryStats(ctx, other.ID, since)
		require.NoError(t, err)
		assert.Equal(t, int64(2), cs.Total)
		assert.Equal(t, int64(1), cs.Succeeded)
		assert.Equal(t, int64(1), cs.Failed)
		require.NotNil(t, cs.SuccessRate)
		assert.InDelta(t, 0.5, *cs.SuccessRate, 1e-9)
	})

	t.Run("a narrower window drops the older attempts", func(t *testing.T) {
		// Only the retry ten minutes ago falls inside this window; the
		// failure 25 minutes ago and the success three hours ago do not.
		cs, err := st.GetChannelDeliveryStats(ctx, mixed.ID, now.Add(-15*time.Minute))
		require.NoError(t, err)
		assert.Equal(t, int64(1), cs.Total)
		assert.Equal(t, int64(1), cs.Succeeded)
		assert.Zero(t, cs.Failed)
	})

	t.Run("a cutoff newer than every attempt reads as quiet, not an error", func(t *testing.T) {
		cs, err := st.GetChannelDeliveryStats(ctx, healthy.ID, now.Add(time.Hour))
		require.NoError(t, err)
		assert.Zero(t, cs.Total)
		assert.Nil(t, cs.SuccessRate)
	})

	t.Run("an unknown channel is ErrNotFound, never a body of zeroes", func(t *testing.T) {
		_, err := st.GetChannelDeliveryStats(ctx, healthy.ID+9999, since)
		assert.ErrorIs(t, err, ErrNotFound)
	})
}
