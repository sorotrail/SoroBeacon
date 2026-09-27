package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBackupDataRoundTrip(t *testing.T) {
	original := BackupData{
		Version:   backupVersion,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Monitors: []store.Monitor{
			{ID: 1, Name: "test-monitor", ContractIDs: []string{"C123"}, Enabled: true, ChannelIDs: []int64{10}},
		},
		Rules: []store.Rule{
			{ID: 1, MonitorID: 1, Type: "event_emitted", Params: json.RawMessage(`{"topic":"transfer"}`), Enabled: true},
		},
		Channels: []backupChannel{
			{ID: 10, Name: "slack-ops", Type: "slack", Config: json.RawMessage(`{"webhook_url":"https://hooks.example.com/x"}`), Enabled: true},
		},
		SavedSearches: []store.SavedSearch{
			{ID: 1, Name: "recent", Filter: store.SavedSearchFilter{Sort: "created_at_desc"}},
		},
		Templates: []store.MonitorTemplate{
			{ID: 1, Name: "token-watch", Description: "watch a token contract"},
		},
	}

	data, err := json.Marshal(original)
	require.NoError(t, err)

	var restored BackupData
	require.NoError(t, json.Unmarshal(data, &restored))

	assert.Equal(t, original.Version, restored.Version)
	assert.Equal(t, original.CreatedAt, restored.CreatedAt)
	assert.Len(t, restored.Monitors, 1)
	assert.Equal(t, "test-monitor", restored.Monitors[0].Name)
	assert.Len(t, restored.Rules, 1)
	assert.Equal(t, "event_emitted", restored.Rules[0].Type)
	assert.Len(t, restored.Channels, 1)
	assert.Equal(t, "slack-ops", restored.Channels[0].Name)
	assert.JSONEq(t, `{"webhook_url":"https://hooks.example.com/x"}`, string(restored.Channels[0].Config))
	assert.Len(t, restored.SavedSearches, 1)
	assert.Len(t, restored.Templates, 1)
}

func TestBackupVersionCheck(t *testing.T) {
	future := BackupData{Version: backupVersion + 1}
	data, err := json.Marshal(future)
	require.NoError(t, err)

	var parsed BackupData
	require.NoError(t, json.Unmarshal(data, &parsed))
	assert.Greater(t, parsed.Version, backupVersion, "future backup version should exceed current")
}

func TestIsTerminal(t *testing.T) {
	// A temp file is not a terminal.
	f, err := createTempFile(t)
	require.NoError(t, err)
	defer f.Close()
	assert.False(t, isTerminal(f))
}

func createTempFile(t *testing.T) (*os.File, error) {
	return os.CreateTemp(t.TempDir(), "backup-test-*")
}
