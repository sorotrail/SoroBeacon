package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests below are part of the shared conformance suite, so they run against
// both backends: a health counter that is right on Postgres and silently wrong
// on SQLite would be worse than no counter at all. The helpers take the store
// interface rather than a concrete type for the same reason.

// healthChannel creates an enabled channel for the health tests.
func healthChannel(t *testing.T, st conformanceStore) *Channel {
	t.Helper()
	ch := &Channel{Name: "ops", Type: "webhook", Config: json.RawMessage(`{"url":"https://example.invalid"}`), Enabled: true}
	require.NoError(t, st.CreateChannel(context.Background(), ch))
	return ch
}

// fail records one delivery failure and returns the channel as stored after it.
func fail(t *testing.T, st conformanceStore, id int64, permanent bool, disableAfter int, msg string) Channel {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, st.RecordChannelHealth(ctx, id, ChannelHealthUpdate{
		Permanent: permanent, Error: msg, DisableAfter: disableAfter, At: time.Now(),
	}))
	got, err := st.GetChannel(ctx, id)
	require.NoError(t, err)
	return *got
}

// succeed records one successful delivery and returns the stored channel.
func succeed(t *testing.T, st conformanceStore, id int64) Channel {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, st.RecordChannelHealth(ctx, id, ChannelHealthUpdate{Success: true, At: time.Now()}))
	got, err := st.GetChannel(ctx, id)
	require.NoError(t, err)
	return *got
}

func testChannelHealthCountsByKind(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ch := healthChannel(t, st)

	// A transient failure counts and is reported, but it is not the kind of
	// failure a channel can be parked for.
	got := fail(t, st, ch.ID, false, 0, "status 500: upstream exploded")
	assert.Equal(t, int64(1), got.ConsecutiveFailures)
	assert.Zero(t, got.ConsecutivePermanentFailures)
	assert.Equal(t, "status 500: upstream exploded", got.LastError)
	require.NotNil(t, got.LastErrorAt)
	assert.True(t, got.Enabled, "a 5xx must not take a channel out of rotation")
	assert.Nil(t, got.DisabledAt)

	got = fail(t, st, ch.ID, true, 0, "status 401: Unauthorized")
	assert.Equal(t, int64(2), got.ConsecutiveFailures)
	assert.Equal(t, int64(1), got.ConsecutivePermanentFailures)
	assert.Equal(t, "failing", got.HealthStatus())
}

func testChannelHealthSuccessResets(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ch := healthChannel(t, st)

	fail(t, st, ch.ID, true, 0, "status 401: Unauthorized")
	fail(t, st, ch.ID, true, 0, "status 401: Unauthorized")
	got := succeed(t, st, ch.ID)

	assert.Zero(t, got.ConsecutiveFailures, "one success resets the count")
	assert.Zero(t, got.ConsecutivePermanentFailures)
	assert.Empty(t, got.LastError, "a working channel has no last error to show")
	assert.Nil(t, got.LastErrorAt)
	require.NotNil(t, got.LastSuccessAt)
	assert.Equal(t, "ok", got.HealthStatus())
}

func testChannelHealthAutoDisablesOnPermanentFailures(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ch := healthChannel(t, st)

	got := fail(t, st, ch.ID, true, 3, "status 401: Unauthorized")
	assert.True(t, got.Enabled, "one failure is not conclusive")
	assert.Nil(t, got.DisabledAt)

	got = fail(t, st, ch.ID, true, 3, "status 401: Unauthorized")
	assert.True(t, got.Enabled, "two failures are not conclusive either")
	assert.Nil(t, got.DisabledAt)

	got = fail(t, st, ch.ID, true, 3, "status 401: Unauthorized")
	assert.False(t, got.Enabled, "the threshold must take the channel out of rotation")
	require.NotNil(t, got.DisabledAt, "an auto-disabled channel records when it happened")
	assert.Equal(t, "auto_disabled", got.HealthStatus())

	// The dispatcher asks for enabled channels only, so the auto-disabled one
	// is gone from the delivery path.
	enabled, err := st.ListChannels(context.Background(), true)
	require.NoError(t, err)
	assert.Empty(t, enabled)
}

func testChannelHealthWithoutThresholdNeverDisables(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ch := healthChannel(t, st)

	for i := 0; i < 10; i++ {
		fail(t, st, ch.ID, true, 0, "status 401: Unauthorized")
	}

	got, err := st.GetChannel(context.Background(), ch.ID)
	require.NoError(t, err)
	assert.True(t, got.Enabled, "auto-disable is off unless an operator asks for it")
	assert.Nil(t, got.DisabledAt)
	assert.Equal(t, int64(10), got.ConsecutivePermanentFailures)
}

func testChannelHealthTransientFailuresNeverDisable(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ch := healthChannel(t, st)

	for i := 0; i < 5; i++ {
		got := fail(t, st, ch.ID, false, 2, "status 503: Service Unavailable")
		assert.True(t, got.Enabled, "a provider outage must not disable a healthy channel")
		assert.Nil(t, got.DisabledAt)
	}

	got, err := st.GetChannel(context.Background(), ch.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(5), got.ConsecutiveFailures, "transient failures are still reported")
	assert.Zero(t, got.ConsecutivePermanentFailures)
}

// A 5xx in the middle of a revoked-token streak must not hide the streak:
// otherwise provider noise could keep a dead channel in rotation forever.
func testChannelHealthTransientDoesNotMaskPermanentStreak(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ch := healthChannel(t, st)

	fail(t, st, ch.ID, true, 2, "status 401: Unauthorized")
	fail(t, st, ch.ID, false, 2, "status 503: Service Unavailable")
	got := fail(t, st, ch.ID, true, 2, "status 401: Unauthorized")

	assert.False(t, got.Enabled, "the permanent streak survives a transient failure in between")
	assert.Equal(t, int64(3), got.ConsecutiveFailures)
	assert.Equal(t, int64(2), got.ConsecutivePermanentFailures)
}

func testUpdateChannelReenableClearsHealth(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()
	ch := healthChannel(t, st)

	for i := 0; i < 3; i++ {
		fail(t, st, ch.ID, true, 1, "status 401: Unauthorized")
	}
	got, err := st.GetChannel(ctx, ch.ID)
	require.NoError(t, err)
	require.False(t, got.Enabled)
	require.NotNil(t, got.DisabledAt)

	// The only way back: an explicit write that turns the channel on.
	got.Enabled = true
	require.NoError(t, st.UpdateChannel(ctx, got))

	reread, err := st.GetChannel(ctx, ch.ID)
	require.NoError(t, err)
	assert.True(t, reread.Enabled)
	assert.Nil(t, reread.DisabledAt, "a re-enabled channel is no longer auto-disabled")
	assert.Zero(t, reread.ConsecutiveFailures)
	assert.Zero(t, reread.ConsecutivePermanentFailures)
	assert.Empty(t, reread.LastError)
	assert.Nil(t, reread.LastErrorAt)
	assert.Equal(t, "ok", reread.HealthStatus())
}

// Renaming a channel that is still failing must not wipe the evidence, or the
// dashboard would go quiet about a channel that is dropping alerts.
func testUpdateChannelRenameKeepsHealth(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()
	ch := healthChannel(t, st)

	fail(t, st, ch.ID, true, 0, "status 401: Unauthorized")
	got, err := st.GetChannel(ctx, ch.ID)
	require.NoError(t, err)

	got.Name = "ops-renamed"
	require.NoError(t, st.UpdateChannel(ctx, got))

	reread, err := st.GetChannel(ctx, ch.ID)
	require.NoError(t, err)
	assert.Equal(t, "ops-renamed", reread.Name)
	assert.Equal(t, int64(1), reread.ConsecutiveFailures)
	assert.Equal(t, int64(1), reread.ConsecutivePermanentFailures)
	assert.Equal(t, "status 401: Unauthorized", reread.LastError)
}

// A channel deleted while a delivery is in flight is a race, not a bug.
func testChannelHealthUnknownChannelIsIgnored(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	assert.NoError(t, st.RecordChannelHealth(context.Background(), 4242, ChannelHealthUpdate{Permanent: true, Error: "boom"}))
}

// TestChannelHealthStatus pins HealthStatus, which is pure and therefore
// backend-neutral; the store tests above prove the columns it reads are right.
func TestChannelHealthStatus(t *testing.T) {
	when := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		ch   Channel
		want string
	}{
		{name: "never used", ch: Channel{Enabled: true}, want: "ok"},
		{name: "auto-disabled wins over everything", ch: Channel{Enabled: false, DisabledAt: &when, ConsecutiveFailures: 3}, want: "auto_disabled"},
		{name: "turned off by an operator", ch: Channel{Enabled: false}, want: "disabled"},
		{name: "failing but still on", ch: Channel{Enabled: true, ConsecutiveFailures: 2}, want: "failing"},
		{name: "recovered", ch: Channel{Enabled: true, LastSuccessAt: &when}, want: "ok"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.ch.HealthStatus())
		})
	}
}
