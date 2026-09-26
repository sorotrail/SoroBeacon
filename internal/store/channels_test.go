package store

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// channelConfigJSON is written in jsonb's own canonical text form — keys
// sorted shortest-first, ": " and ", " separators — so the byte-for-byte
// round trip below measures the store, not jsonb's normalisation. It stands
// in for the webhook URLs and bot tokens a real channel would carry.
const channelConfigJSON = `{"url": "https://hooks.example.test/T000/B000/s3cr3t", "token": "s3cr3t-token"}`

// assertConfigEqual compares configs behind a boolean: config holds delivery
// secrets, so the failure message may say what broke but must never print
// the bytes themselves.
func assertConfigEqual(t *testing.T, want, got []byte, msg string) {
	t.Helper()
	assert.True(t, bytes.Equal(want, got), msg)
}

// channelIDs narrows channels to their ids so length and order assertions
// report ids instead of dumping the rows — a dumped Channel would put its
// config in the failure message.
func channelIDs(cs []Channel) []int64 {
	ids := make([]int64, len(cs))
	for i, c := range cs {
		ids[i] = c.ID
	}
	return ids
}

// TestChannelCreateReadRoundTrip pins the write and read paths of a single
// channel: every column set at create time comes back unchanged, and the
// config survives byte-for-byte because it carries the credentials delivery
// depends on.
func TestChannelCreateReadRoundTrip(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	created := &Channel{
		Name:    "ops-webhook",
		Type:    "webhook",
		Config:  json.RawMessage(channelConfigJSON),
		Enabled: true,
	}
	require.NoError(t, st.CreateChannel(ctx, created))
	require.NotZero(t, created.ID, "create must fill the new id")
	require.False(t, created.CreatedAt.IsZero(), "create must fill created_at")

	got, err := st.GetChannel(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, created.ID, got.ID)
	assert.Equal(t, "ops-webhook", got.Name)
	assert.Equal(t, "webhook", got.Type)
	assert.True(t, got.Enabled)
	assert.True(t, got.CreatedAt.Equal(created.CreatedAt))
	assertConfigEqual(t, []byte(channelConfigJSON), got.Config,
		"config must round-trip byte-for-byte through the store")
}

// TestChannelUpdateTogglesEnabled covers the mutable columns, above all the
// enabled flag the dispatch path filters on: an update must persist name,
// type and a rotated config, and toggling enabled must stick in both
// directions instead of silently falling back to the stored value.
func TestChannelUpdateTogglesEnabled(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	c := &Channel{
		Name:    "before",
		Type:    "webhook",
		Config:  json.RawMessage(channelConfigJSON),
		Enabled: true,
	}
	require.NoError(t, st.CreateChannel(ctx, c))

	// Rotate the secret, rename and disable in one update.
	rotated := `{"url": "https://hooks.example.test/T000/B000/rotated", "token": "rotated-token"}`
	c.Name = "after"
	c.Type = "slack"
	c.Config = json.RawMessage(rotated)
	c.Enabled = false
	require.NoError(t, st.UpdateChannel(ctx, c))

	got, err := st.GetChannel(ctx, c.ID)
	require.NoError(t, err)
	assert.Equal(t, c.ID, got.ID)
	assert.Equal(t, "after", got.Name)
	assert.Equal(t, "slack", got.Type)
	assert.False(t, got.Enabled, "the enabled toggle must persist")
	assertConfigEqual(t, []byte(rotated), got.Config,
		"an updated config must replace the stored one byte-for-byte")

	// Toggle back the other way: dispatch re-enables through the same path.
	c.Enabled = true
	require.NoError(t, st.UpdateChannel(ctx, c))
	got, err = st.GetChannel(ctx, c.ID)
	require.NoError(t, err)
	assert.True(t, got.Enabled, "re-enabling must persist too")
}

// TestChannelListEnabledOnlyFilter covers both list modes the API exposes:
// without the filter every channel comes back in id order, with it the
// disabled channel is filtered out while the enabled ones stay.
func TestChannelListEnabledOnlyFilter(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	on1 := &Channel{Name: "on-1", Type: "webhook", Config: json.RawMessage(`{}`), Enabled: true}
	off := &Channel{Name: "off", Type: "slack", Config: json.RawMessage(`{}`), Enabled: false}
	on2 := &Channel{Name: "on-2", Type: "discord", Config: json.RawMessage(`{}`), Enabled: true}
	for _, c := range []*Channel{on1, off, on2} {
		require.NoError(t, st.CreateChannel(ctx, c))
	}

	all, err := st.ListChannels(ctx, false)
	require.NoError(t, err)
	require.Len(t, channelIDs(all), 3, "without the filter every channel is listed")
	assert.Equal(t, []int64{on1.ID, off.ID, on2.ID}, channelIDs(all),
		"no filter lists in id order")

	enabled, err := st.ListChannels(ctx, true)
	require.NoError(t, err)
	require.Len(t, channelIDs(enabled), 2, "the enabled-only filter drops the disabled channel")
	assert.Equal(t, []int64{on1.ID, on2.ID}, channelIDs(enabled))
	for _, c := range enabled {
		assert.True(t, c.Enabled)
	}
}

// TestChannelDeleteAttachedToMonitor pins the schema's intent from
// 0001_init: monitor_channels.channel_id is ON DELETE CASCADE, so deleting a
// channel drops its attachment rows while the monitor itself survives — the
// delete must succeed rather than be rejected by the attachment.
func TestChannelDeleteAttachedToMonitor(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	c := &Channel{Name: "c", Type: "webhook", Config: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateChannel(ctx, c))
	require.NoError(t, st.SetMonitorChannels(ctx, m.ID, []int64{c.ID}))

	require.NoError(t, st.DeleteChannel(ctx, c.ID),
		"a channel attached to a monitor must still be deletable")

	_, err := st.GetChannel(ctx, c.ID)
	assert.ErrorIs(t, err, ErrNotFound)

	// The monitor outlives the channel; only the attachment row cascaded.
	got, err := st.GetMonitor(ctx, m.ID)
	require.NoError(t, err)
	assert.Empty(t, got.ChannelIDs, "the cascade must drop the attachment, not the monitor")

	attached, err := st.ListChannelsForMonitor(ctx, m.ID)
	require.NoError(t, err)
	assert.Empty(t, channelIDs(attached), "a deleted channel must stop matching dispatch")
}

// TestChannelNotFound pins ErrNotFound on all three id-based paths — read,
// update and delete — so the API can answer 404 instead of leaking a driver
// error, and so a repeat delete is an error rather than a silent success.
func TestChannelNotFound(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	c := &Channel{Name: "gone", Type: "webhook", Config: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateChannel(ctx, c))
	require.NoError(t, st.DeleteChannel(ctx, c.ID))

	_, err := st.GetChannel(ctx, c.ID)
	assert.ErrorIs(t, err, ErrNotFound, "reading a deleted channel")

	c.Name = "renamed-after-delete"
	c.Enabled = false
	err = st.UpdateChannel(ctx, c)
	assert.ErrorIs(t, err, ErrNotFound, "updating a deleted channel")

	err = st.DeleteChannel(ctx, c.ID)
	assert.ErrorIs(t, err, ErrNotFound, "deleting a deleted channel")
}
