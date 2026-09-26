package store

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMonitorCRUDPaths covers the monitor lifecycle against a real Postgres:
// create/read round trip, update (including the enabled toggle), filtered
// and unfiltered listing, cascading delete, and not-found behaviour.
//
// It skips without TEST_DATABASE_URL like every other database-backed store
// test (see postgres_test.go); CI's test-db job sets it. Setup reuses
// newTestPostgres, which migrates and empties the tables, so each run starts
// from a known state.
func TestMonitorCRUDPaths(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres monitor CRUD tests")
	}
	ctx := context.Background()

	t.Run("CreateReadRoundTrip", func(t *testing.T) {
		st := newTestPostgres(t)
		m := &Monitor{
			Name:        "round trip",
			ContractIDs: []string{"CABC", "CDEF"},
			Enabled:     true,
		}
		require.NoError(t, st.CreateMonitor(ctx, m))
		require.NotZero(t, m.ID, "create must fill the monitor ID")
		require.False(t, m.CreatedAt.IsZero(), "create must fill CreatedAt")

		got, err := st.GetMonitor(ctx, m.ID)
		require.NoError(t, err)
		require.Equal(t, m.ID, got.ID)
		require.Equal(t, "round trip", got.Name)
		require.Equal(t, []string{"CABC", "CDEF"}, got.ContractIDs)
		require.True(t, got.Enabled)
		require.Nil(t, got.LastMatchedAt, "a monitor that never matched has no stamp")
		require.Empty(t, got.ChannelIDs)
	})

	t.Run("UpdateTogglesEnabled", func(t *testing.T) {
		st := newTestPostgres(t)
		m := &Monitor{Name: "toggle me", ContractIDs: []string{"CABC"}, Enabled: true}
		require.NoError(t, st.CreateMonitor(ctx, m))

		m.Name = "toggled"
		m.Enabled = false
		require.NoError(t, st.UpdateMonitor(ctx, m))

		got, err := st.GetMonitor(ctx, m.ID)
		require.NoError(t, err)
		require.Equal(t, "toggled", got.Name)
		require.False(t, got.Enabled)
	})

	t.Run("ListRespectsEnabledFilter", func(t *testing.T) {
		st := newTestPostgres(t)
		on := &Monitor{Name: "on", ContractIDs: []string{"C1"}, Enabled: true}
		off := &Monitor{Name: "off", ContractIDs: []string{"C2"}, Enabled: false}
		require.NoError(t, st.CreateMonitor(ctx, on))
		require.NoError(t, st.CreateMonitor(ctx, off))

		all, err := st.ListMonitors(ctx, false)
		require.NoError(t, err)
		require.Len(t, all, 2)

		enabled, err := st.ListMonitors(ctx, true)
		require.NoError(t, err)
		require.Len(t, enabled, 1)
		require.Equal(t, on.ID, enabled[0].ID)
	})

	t.Run("DeleteCascadesRulesAndAttachments", func(t *testing.T) {
		st := newTestPostgres(t)
		m := &Monitor{Name: "doomed", ContractIDs: []string{"CABC"}, Enabled: true}
		require.NoError(t, st.CreateMonitor(ctx, m))

		rule := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: []byte(`{"event_name":"transfer"}`), Enabled: true}
		require.NoError(t, st.CreateRule(ctx, rule))

		ch := &Channel{Name: "hook", Type: "webhook", Config: []byte(`{"url":"https://example.com/hook"}`), Enabled: true}
		require.NoError(t, st.CreateChannel(ctx, ch))
		require.NoError(t, st.SetMonitorChannels(ctx, m.ID, []int64{ch.ID}))

		require.NoError(t, st.DeleteMonitor(ctx, m.ID))

		// The monitor row is gone...
		_, err := st.GetMonitor(ctx, m.ID)
		require.ErrorIs(t, err, ErrNotFound)
		// ...and the rules and attachments went with it (ON DELETE CASCADE),
		// rather than lingering as orphans.
		_, err = st.GetRule(ctx, rule.ID)
		require.ErrorIs(t, err, ErrNotFound)
		attached, err := st.ListChannelsForMonitor(ctx, m.ID)
		require.NoError(t, err)
		require.Empty(t, attached)
		// The channel itself survives: it may serve other monitors.
		_, err = st.GetChannel(ctx, ch.ID)
		require.NoError(t, err)
	})

	t.Run("NotFound", func(t *testing.T) {
		st := newTestPostgres(t)
		_, err := st.GetMonitor(ctx, 424242)
		require.ErrorIs(t, err, ErrNotFound)
		require.ErrorIs(t, st.DeleteMonitor(ctx, 424242), ErrNotFound)
		require.ErrorIs(t, st.UpdateMonitor(ctx, &Monitor{ID: 424242, Name: "ghost"}), ErrNotFound)
	})
}
