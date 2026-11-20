package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

func TestPushDeviceLifecycle(t *testing.T) {
	st := newTestSQLiteStore(t)
	ctx := context.Background()
	ch := &Channel{Name: "push", Type: "push", Enabled: true}
	require.NoError(t, st.CreateChannel(ctx, ch))

	require.NoError(t, st.UpsertPushDevice(ctx, ch.ID, "phone", "fcm", "tok-1"))
	require.NoError(t, st.UpsertPushDevice(ctx, ch.ID, "phone", "fcm", "tok-2"), "re-register refreshes the token")
	devs, err := st.ListPushDevices(ctx, ch.ID)
	require.NoError(t, err)
	require.Len(t, devs, 1)
	assert.Equal(t, "tok-2", devs[0].Token)

	require.NoError(t, st.RetirePushDevice(ctx, ch.ID, "phone"))
	devs, _ = st.ListPushDevices(ctx, ch.ID)
	assert.Empty(t, devs, "a retired device is not delivered to")
	require.NoError(t, st.UpsertPushDevice(ctx, ch.ID, "phone", "fcm", "tok-3"), "re-registering revives it")
	devs, _ = st.ListPushDevices(ctx, ch.ID)
	assert.Len(t, devs, 1)

	require.NoError(t, st.DeletePushDevice(ctx, ch.ID, "phone"))
	assert.ErrorIs(t, st.DeletePushDevice(ctx, ch.ID, "phone"), ErrNotFound)
	assert.ErrorIs(t, st.UpsertPushDevice(ctx, ch.ID, "", "fcm", "t"), ErrInvalidDevice)
	assert.ErrorIs(t, st.UpsertPushDevice(ctx, ch.ID, "d", "apns", "t"), ErrInvalidDevice)
}

func TestPushDevicesAreWorkspaceScoped(t *testing.T) {
	st := newTestSQLiteStore(t)
	ctx := context.Background()
	ch := &Channel{Name: "push", Type: "push", Enabled: true}
	require.NoError(t, st.CreateChannel(ctx, ch))
	require.NoError(t, st.UpsertPushDevice(ctx, ch.ID, "phone", "fcm", "tok"))

	other := workspace.With(ctx, workspace.ID("beta"))
	assert.ErrorIs(t, st.UpsertPushDevice(other, ch.ID, "x", "fcm", "t"), ErrNotFound)
	devs, err := st.ListPushDevices(other, ch.ID)
	require.NoError(t, err)
	assert.Empty(t, devs)
	assert.ErrorIs(t, st.DeletePushDevice(other, ch.ID, "phone"), ErrNotFound)
}
