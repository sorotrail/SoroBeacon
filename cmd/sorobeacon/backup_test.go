package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/config"
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

// Timestamps in a backup are RFC 3339 with nanoseconds and the original
// offset; decoding must preserve the instant, the sub-second digits and the
// zone, not silently round to the second or convert to local time.
func TestBackupJSONPreservesTimestampPrecisionAndZone(t *testing.T) {
	zone := time.FixedZone("UTC+2", 2*60*60)
	original := BackupData{
		Version:   backupVersion,
		CreatedAt: time.Date(2026, 2, 3, 4, 5, 6, 123456789, zone),
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	require.NoError(t, enc.Encode(original))

	var decoded BackupData
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))

	require.True(t, original.CreatedAt.Equal(decoded.CreatedAt), "instant changed: %s != %s", original.CreatedAt, decoded.CreatedAt)
	assert.Equal(t, 123456789, decoded.CreatedAt.Nanosecond())
	_, offset := decoded.CreatedAt.Zone()
	assert.Equal(t, 2*60*60, offset)
}

// JSON columns keep key order and are not HTML-escaped by the encoder. The
// stored bytes are compacted/re-indented, so this asserts the properties the
// format actually promises rather than byte equality.
func TestBackupJSONDoesNotReorderOrEscapeConfig(t *testing.T) {
	rawConfig := json.RawMessage(`{"z_first":1,"a_second":"<token>&"}`)
	data := BackupData{
		Version:   backupVersion,
		CreatedAt: time.Now().UTC(),
		Monitors:  []store.Monitor{{ID: 1, Name: "m"}},
		Channels:  []backupChannel{{ID: 1, Name: "c", Type: "webhook", Config: rawConfig}},
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	require.NoError(t, enc.Encode(data))

	out := buf.String()
	assert.NotContains(t, out, `\u003c`, "encoder escaped < in config")
	assert.NotContains(t, out, `\u003e`, "encoder escaped > in config")
	assert.NotContains(t, out, `\u0026`, "encoder escaped & in config")
	assert.Less(t, strings.Index(out, `"z_first"`), strings.Index(out, `"a_second"`), "config key order changed")
	assert.Contains(t, out, `"last_matched_at": null`, "nullable fields must stay null")
}

func TestBackupVersionCheck(t *testing.T) {
	future := BackupData{
		Version:   backupVersion + 1,
		CreatedAt: time.Now().UTC(),
	}
	err := validateBackup(&future)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "newer than supported")

	var zero BackupData
	err = validateBackup(&zero)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no usable version")
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

// --- helpers ---

func sqliteURL(t *testing.T) string {
	t.Helper()
	return "sqlite://" + filepath.Join(t.TempDir(), "sorobeacon.db")
}

func openSQLite(t *testing.T, url string) store.Store {
	t.Helper()
	require.NoError(t, store.Migrate(url))
	st, err := store.NewSQLite(context.Background(), url)
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	return st
}

// storeOpener mimics openStore for a SQLite URL: migrate, then connect.
func storeOpener(url string) func() (config.Config, store.Store, error) {
	return func() (config.Config, store.Store, error) {
		if err := store.Migrate(url); err != nil {
			return config.Config{}, nil, err
		}
		st, err := store.NewSQLite(context.Background(), url)
		if err != nil {
			return config.Config{}, nil, err
		}
		return config.Config{}, st, nil
	}
}

func testLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

type backupFixture struct {
	channelSecret string
	createdAt     time.Time
}

// fixtureCreatedAt is deliberately old and has a non-zero millisecond part.
// A restore that used "now" instead of the backup's timestamp cannot pass by
// landing in the same clock tick as the fixture.
func fixtureCreatedAt() time.Time {
	return time.Date(2025, 11, 12, 13, 14, 15, 678000000, time.UTC)
}

func populateBackupFixture(t *testing.T, st store.Store) backupFixture {
	t.Helper()
	ctx := context.Background()
	secret := "https://hooks.example.com/services/T000/B000/secret-token"
	created := fixtureCreatedAt()

	ch := &store.Channel{
		Name:                "ops-slack",
		Type:                "slack",
		Config:              json.RawMessage(`{"webhook_url":"` + secret + `","headers":{"X-Note":"a<b&c>d"}}`),
		Enabled:             true,
		DigestMode:          store.DigestModeWindow,
		DigestWindowSeconds: 300,
		Timeout:             42 * time.Second,
		CreatedAt:           created,
	}
	require.NoError(t, st.CreateChannel(ctx, ch))

	m := &store.Monitor{
		Name:        "token-watch",
		ContractIDs: []string{"CABC", "CDEF"},
		Enabled:     true,
		Priority:    store.PriorityHigh,
		Network:     "testnet",
		CreatedAt:   created,
	}
	require.NoError(t, st.CreateMonitor(ctx, m))
	require.NoError(t, st.SetMonitorChannels(ctx, m.ID, []int64{ch.ID}))

	rule := &store.Rule{
		MonitorID: m.ID,
		Type:      "event_emitted",
		Params:    json.RawMessage(`{"topic":"transfer","threshold":5}`),
		Enabled:   true,
		Severity:  store.SeverityCritical,
	}
	require.NoError(t, st.CreateRule(ctx, rule))

	search := &store.SavedSearch{
		Name: "critical transfers",
		Filter: store.SavedSearchFilter{
			MonitorID:  m.ID,
			RuleID:     rule.ID,
			ContractID: "CABC",
			Sort:       "created_at_desc",
		},
		IsDefault: true,
		CreatedAt: created,
	}
	require.NoError(t, st.CreateSavedSearch(ctx, search))

	template := &store.MonitorTemplate{
		Name:        "transfer-watch",
		Description: "watch transfers",
		Rules: []store.MonitorTemplateRule{
			{Type: "event_emitted", Params: json.RawMessage(`{"topic":"{{topic}}"}`)},
		},
		ChannelIDs: []int64{ch.ID},
		Parameters: []store.TemplateParameter{
			{Name: "topic", Description: "event topic", Required: true, Default: "transfer"},
		},
		CreatedAt: created,
	}
	require.NoError(t, st.CreateMonitorTemplate(ctx, template))

	return backupFixture{channelSecret: secret, createdAt: created}
}

// backupBytes runs the backup command into a temp file and returns the raw
// bytes plus the decoded envelope.
func backupBytes(t *testing.T, url string) ([]byte, *BackupData) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "backup.json")
	f, err := os.Create(path)
	require.NoError(t, err)
	deps := backupDeps{
		stdout: f,
		stderr: io.Discard,
		open:   storeOpener(url),
		log:    testLogger(&bytes.Buffer{}),
	}
	require.NoError(t, runBackupWith(deps, nil))
	require.NoError(t, f.Close())

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	data, err := readBackup(bytes.NewReader(raw))
	require.NoError(t, err)
	return raw, data
}

// restoreBackup runs the restore command and returns its structured logs.
func restoreBackup(t *testing.T, url string, input []byte, args ...string) (*bytes.Buffer, error) {
	t.Helper()
	var logs bytes.Buffer
	deps := backupDeps{
		stdin:  bytes.NewReader(input),
		stderr: io.Discard,
		open:   storeOpener(url),
		log:    testLogger(&logs),
	}
	err := runRestoreWith(deps, args)
	return &logs, err
}

func logEntry(t *testing.T, logs []byte, msg string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(string(logs)), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &entry))
		if entry["msg"] == msg {
			return entry
		}
	}
	t.Fatalf("log entry %q not found in:\n%s", msg, logs)
	return nil
}

func findChannelByName(t *testing.T, st store.Store, name string) *store.Channel {
	t.Helper()
	channels, err := st.ListChannels(context.Background(), false)
	require.NoError(t, err)
	for i := range channels {
		if channels[i].Name == name {
			full, err := st.GetChannel(context.Background(), channels[i].ID)
			require.NoError(t, err)
			return full
		}
	}
	t.Fatalf("channel %q not found", name)
	return nil
}

func findMonitorByName(t *testing.T, st store.Store, name string) *store.Monitor {
	t.Helper()
	monitors, err := st.ListMonitors(context.Background(), false)
	require.NoError(t, err)
	for i := range monitors {
		if monitors[i].Name == name {
			full, err := st.GetMonitor(context.Background(), monitors[i].ID)
			require.NoError(t, err)
			return full
		}
	}
	t.Fatalf("monitor %q not found", name)
	return nil
}

// assertRestoredFixture compares every exported table against the backup,
// mapping cross-row references to the new ids. Derived state that is not in
// the backup (LastMatchedAt, delivery health) is expected to stay empty.
func assertRestoredFixture(t *testing.T, data *BackupData, st store.Store, fix backupFixture) {
	t.Helper()
	ctx := context.Background()

	require.Len(t, data.Monitors, 1)
	require.Len(t, data.Channels, 1)
	require.Len(t, data.Rules, 1)
	require.Len(t, data.SavedSearches, 1)
	require.Len(t, data.Templates, 1)

	// Monitor attachments are part of the export; before it fetched each
	// monitor in full, this was always null and silently lost.
	require.Equal(t, []int64{data.Channels[0].ID}, data.Monitors[0].ChannelIDs)

	gotMonitor := findMonitorByName(t, st, data.Monitors[0].Name)
	assert.Equal(t, data.Monitors[0].ContractIDs, gotMonitor.ContractIDs)
	assert.Equal(t, data.Monitors[0].Enabled, gotMonitor.Enabled)
	assert.Equal(t, data.Monitors[0].Priority, gotMonitor.Priority)
	assert.Equal(t, data.Monitors[0].Network, gotMonitor.Network)
	assert.True(t, data.Monitors[0].CreatedAt.Equal(gotMonitor.CreatedAt),
		"monitor created_at: want %s, got %s", data.Monitors[0].CreatedAt, gotMonitor.CreatedAt)
	assert.Nil(t, gotMonitor.LastMatchedAt, "never-matched monitor must stay null")

	gotChannel := findChannelByName(t, st, data.Channels[0].Name)
	assert.Equal(t, data.Channels[0].Type, gotChannel.Type)
	assert.Equal(t, data.Channels[0].Enabled, gotChannel.Enabled)
	assert.Equal(t, data.Channels[0].DigestMode, gotChannel.DigestMode)
	assert.Equal(t, data.Channels[0].DigestWindowSeconds, gotChannel.DigestWindowSeconds)
	assert.Equal(t, data.Channels[0].TimeoutSeconds, int64(gotChannel.TimeoutSeconds()))
	assert.True(t, data.Channels[0].CreatedAt.Equal(gotChannel.CreatedAt),
		"channel created_at: want %s, got %s", data.Channels[0].CreatedAt, gotChannel.CreatedAt)
	assert.JSONEq(t, string(data.Channels[0].Config), string(gotChannel.Config))
	assert.Contains(t, string(gotChannel.Config), fix.channelSecret)

	require.Len(t, gotMonitor.ChannelIDs, 1)
	assert.Equal(t, gotChannel.ID, gotMonitor.ChannelIDs[0], "monitor attachment was not remapped")

	rules, err := st.ListRules(ctx, gotMonitor.ID, false)
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, data.Rules[0].Type, rules[0].Type)
	assert.Equal(t, data.Rules[0].Enabled, rules[0].Enabled)
	assert.JSONEq(t, string(data.Rules[0].Params), string(rules[0].Params))

	searches, err := st.ListSavedSearches(ctx)
	require.NoError(t, err)
	require.Len(t, searches, 1)
	assert.Equal(t, data.SavedSearches[0].Name, searches[0].Name)
	assert.Equal(t, data.SavedSearches[0].Filter.ContractID, searches[0].Filter.ContractID)
	assert.Equal(t, data.SavedSearches[0].Filter.Sort, searches[0].Filter.Sort)
	assert.Equal(t, gotMonitor.ID, searches[0].Filter.MonitorID, "saved-search monitor filter was not remapped")
	assert.Equal(t, rules[0].ID, searches[0].Filter.RuleID, "saved-search rule filter was not remapped")
	assert.True(t, searches[0].IsDefault)
	assert.True(t, data.SavedSearches[0].CreatedAt.Equal(searches[0].CreatedAt),
		"saved search created_at: want %s, got %s", data.SavedSearches[0].CreatedAt, searches[0].CreatedAt)

	templates, err := st.ListMonitorTemplates(ctx)
	require.NoError(t, err)
	require.Len(t, templates, 1)
	assert.Equal(t, data.Templates[0].Name, templates[0].Name)
	assert.Equal(t, data.Templates[0].Description, templates[0].Description)
	require.Len(t, templates[0].Rules, len(data.Templates[0].Rules))
	for i := range templates[0].Rules {
		assert.Equal(t, data.Templates[0].Rules[i].Type, templates[0].Rules[i].Type)
		assert.JSONEq(t, string(data.Templates[0].Rules[i].Params), string(templates[0].Rules[i].Params))
	}
	assert.Equal(t, []int64{gotChannel.ID}, templates[0].ChannelIDs, "template channel reference was not remapped")
	assert.Equal(t, data.Templates[0].Parameters, templates[0].Parameters)
	assert.True(t, data.Templates[0].CreatedAt.Equal(templates[0].CreatedAt),
		"template created_at: want %s, got %s", data.Templates[0].CreatedAt, templates[0].CreatedAt)
}

func TestBackupRestoreRoundTripPreservesEveryExportedTable(t *testing.T) {
	srcURL := sqliteURL(t)
	tgtURL := sqliteURL(t)

	src := openSQLite(t, srcURL)
	fix := populateBackupFixture(t, src)

	raw, data := backupBytes(t, srcURL)

	assert.Equal(t, backupVersion, data.Version)
	assert.Equal(t, store.PriorityHigh, data.Monitors[0].Priority)
	assert.Equal(t, "testnet", data.Monitors[0].Network)
	assert.Equal(t, store.DigestModeWindow, data.Channels[0].DigestMode)
	assert.Equal(t, int64(300), data.Channels[0].DigestWindowSeconds)
	assert.Equal(t, int64(42), data.Channels[0].TimeoutSeconds)
	// The export must carry the stored timestamp, not the time of the backup.
	assert.True(t, data.Monitors[0].CreatedAt.Equal(fix.createdAt), "export changed the monitor timestamp")
	assert.True(t, data.Channels[0].CreatedAt.Equal(fix.createdAt), "export changed the channel timestamp")
	assert.True(t, data.SavedSearches[0].CreatedAt.Equal(fix.createdAt), "export changed the saved-search timestamp")
	assert.True(t, data.Templates[0].CreatedAt.Equal(fix.createdAt), "export changed the template timestamp")

	// The backup is a credential file: the decrypted webhook is in it, while
	// the restore's own logs must never echo it.
	assert.Contains(t, string(raw), fix.channelSecret)
	assert.NotContains(t, string(raw), `\u003c`, "config was HTML-escaped on the way out")

	logs, err := restoreBackup(t, tgtURL, raw)
	require.NoError(t, err)
	assert.Contains(t, logs.String(), "restore complete")
	assert.NotContains(t, logs.String(), fix.channelSecret, "restore logs leaked channel config")

	target := openSQLite(t, tgtURL)
	assertRestoredFixture(t, data, target, fix)
}

func TestBackupRestoreEmptyStore(t *testing.T) {
	srcURL := sqliteURL(t)
	tgtURL := sqliteURL(t)

	raw, data := backupBytes(t, srcURL)
	assert.Zero(t, len(data.Monitors))
	assert.Zero(t, len(data.Channels))
	assert.Zero(t, len(data.Rules))
	assert.Zero(t, len(data.SavedSearches))
	assert.Zero(t, len(data.Templates))

	// The file must be valid JSON even when the store is empty.
	var envelope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &envelope))
	for _, key := range []string{"version", "created_at", "monitors", "rules", "channels", "saved_searches", "templates"} {
		assert.Contains(t, envelope, key)
	}

	_, err := restoreBackup(t, tgtURL, raw)
	require.NoError(t, err)

	target := openSQLite(t, tgtURL)
	ctx := context.Background()
	monitors, err := target.ListMonitors(ctx, false)
	require.NoError(t, err)
	assert.Empty(t, monitors)
	channels, err := target.ListChannels(ctx, false)
	require.NoError(t, err)
	assert.Empty(t, channels)
	searches, err := target.ListSavedSearches(ctx)
	require.NoError(t, err)
	assert.Empty(t, searches)
	templates, err := target.ListMonitorTemplates(ctx)
	require.NoError(t, err)
	assert.Empty(t, templates)
}

// The issue asked for partitioned alerts to survive the round trip. They do
// not: the snapshot is configuration-only, so alerts (and the composite
// foreign keys that reference (id, created_at)) are never reinserted at all.
// This test pins that boundary so the omission is visible rather than
// discovered during a restore.
func TestBackupDoesNotExportAlertHistory(t *testing.T) {
	srcURL := sqliteURL(t)
	tgtURL := sqliteURL(t)
	src := openSQLite(t, srcURL)
	populateBackupFixture(t, src)
	ctx := context.Background()

	monitor := findMonitorByName(t, src, "token-watch")
	rules, err := src.ListRules(ctx, monitor.ID, false)
	require.NoError(t, err)
	require.Len(t, rules, 1)

	outcome, err := src.CreateAlert(ctx, &store.Alert{
		MonitorID: monitor.ID,
		RuleID:    rules[0].ID,
		EventID:   "event-that-must-not-be-in-a-config-backup",
		Payload:   json.RawMessage(`{"topic":"transfer"}`),
		Severity:  store.SeverityCritical,
	})
	require.NoError(t, err)
	require.Equal(t, store.AlertCreated, outcome)

	raw, _ := backupBytes(t, srcURL)

	var envelope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &envelope))
	assert.NotContains(t, envelope, "alerts")
	assert.NotContains(t, string(raw), "event-that-must-not-be-in-a-config-backup")

	_, err = restoreBackup(t, tgtURL, raw)
	require.NoError(t, err)

	target := openSQLite(t, tgtURL)
	stats, err := target.GetStats(ctx)
	require.NoError(t, err)
	assert.Zero(t, stats.Alerts, "configuration snapshot must not invent alert history")
}

func TestRestoreAppendsAndRefusesWithoutForce(t *testing.T) {
	srcURL := sqliteURL(t)
	tgtURL := sqliteURL(t)

	src := openSQLite(t, srcURL)
	populateBackupFixture(t, src)
	raw, data := backupBytes(t, srcURL)

	// Seed the target with an unrelated row so the imported ids cannot
	// accidentally coincide with the source ids.
	target := openSQLite(t, tgtURL)
	ctx := context.Background()
	sentinel := &store.Channel{Name: "sentinel", Type: "webhook", Config: json.RawMessage(`{"url":"https://sentinel.example"}`), Enabled: true}
	require.NoError(t, target.CreateChannel(ctx, sentinel))
	sentinelMonitor := &store.Monitor{Name: "sentinel-monitor", ContractIDs: []string{"CSENTINEL"}, Enabled: true, Network: "testnet"}
	require.NoError(t, target.CreateMonitor(ctx, sentinelMonitor))

	// Without --force the real run refuses, and that refusal must not have
	// written anything.
	_, err := restoreBackup(t, tgtURL, raw)
	require.Error(t, err)
	assert.Equal(t, exitUsage, exitCodeFor(err))
	assert.Contains(t, err.Error(), "--force")

	target = openSQLite(t, tgtURL)
	monitors, err := target.ListMonitors(ctx, false)
	require.NoError(t, err)
	assert.Len(t, monitors, 1, "refused restore modified the target")
	channels, err := target.ListChannels(ctx, false)
	require.NoError(t, err)
	assert.Len(t, channels, 1, "refused restore modified the target")

	// With --force the restore appends. Existing rows are kept; imported rows
	// get fresh ids and every reference is remapped to them.
	_, err = restoreBackup(t, tgtURL, raw, "--force")
	require.NoError(t, err)

	target = openSQLite(t, tgtURL)
	monitors, err = target.ListMonitors(ctx, false)
	require.NoError(t, err)
	assert.Len(t, monitors, 2, "append should keep the existing monitor")
	channels, err = target.ListChannels(ctx, false)
	require.NoError(t, err)
	assert.Len(t, channels, 2, "append should keep the existing channel")

	restoredChannel := findChannelByName(t, target, data.Channels[0].Name)
	assert.NotEqual(t, data.Channels[0].ID, restoredChannel.ID, "append should allocate a new channel id")
	assert.Equal(t, "sentinel", channels[0].Name, "existing channel was replaced")

	restoredMonitor := findMonitorByName(t, target, data.Monitors[0].Name)
	assert.NotEqual(t, data.Monitors[0].ID, restoredMonitor.ID, "append should allocate a new monitor id")
	require.Len(t, restoredMonitor.ChannelIDs, 1)
	assert.Equal(t, restoredChannel.ID, restoredMonitor.ChannelIDs[0])

	rules, err := target.ListRules(ctx, restoredMonitor.ID, false)
	require.NoError(t, err)
	require.Len(t, rules, 1)

	searches, err := target.ListSavedSearches(ctx)
	require.NoError(t, err)
	require.Len(t, searches, 1)
	assert.Equal(t, restoredMonitor.ID, searches[0].Filter.MonitorID)
	assert.Equal(t, rules[0].ID, searches[0].Filter.RuleID)

	templates, err := target.ListMonitorTemplates(ctx)
	require.NoError(t, err)
	require.Len(t, templates, 1)
	assert.Equal(t, []int64{restoredChannel.ID}, templates[0].ChannelIDs)
}

// A dry run has to reach the same decision and report the same row counts as
// the real run over the same input. If it does not, an operator trusts a
// preview that lies.
func TestDryRunMatchesTheRealRun(t *testing.T) {
	srcURL := sqliteURL(t)
	src := openSQLite(t, srcURL)
	populateBackupFixture(t, src)
	raw, data := backupBytes(t, srcURL)

	t.Run("empty target", func(t *testing.T) {
		tgtURL := sqliteURL(t)

		dryLogs, err := restoreBackup(t, tgtURL, raw, "--dry-run")
		require.NoError(t, err)
		dry := logEntry(t, dryLogs.Bytes(), "dry run: no rows written")
		assert.Equal(t, "into-empty-database", dry["mode"])
		assert.Equal(t, float64(len(data.Monitors)), dry["monitors_in_backup"])
		assert.Equal(t, float64(len(data.Channels)), dry["channels_in_backup"])

		target := openSQLite(t, tgtURL)
		monitors, err := target.ListMonitors(context.Background(), false)
		require.NoError(t, err)
		assert.Empty(t, monitors, "dry run wrote rows")

		realLogs, err := restoreBackup(t, tgtURL, raw)
		require.NoError(t, err)
		real := logEntry(t, realLogs.Bytes(), "restore complete")
		assert.Equal(t, dry["monitors_in_backup"], real["monitors"])
		assert.Equal(t, dry["channels_in_backup"], real["channels"])
		assert.Equal(t, dry["rules_in_backup"], real["rules"])
		assert.Equal(t, dry["saved_searches_in_backup"], real["saved_searches"])
		assert.Equal(t, dry["templates_in_backup"], real["templates"])
	})

	t.Run("non-empty target", func(t *testing.T) {
		tgtURL := sqliteURL(t)
		target := openSQLite(t, tgtURL)
		require.NoError(t, target.CreateChannel(context.Background(), &store.Channel{
			Name: "existing", Type: "webhook", Config: json.RawMessage(`{"url":"https://existing.example"}`), Enabled: true,
		}))

		// Both runs refuse identically without --force.
		dryLogs, dryErr := restoreBackup(t, tgtURL, raw, "--dry-run")
		require.Error(t, dryErr)
		realLogs, realErr := restoreBackup(t, tgtURL, raw)
		require.Error(t, realErr)
		assert.Equal(t, exitCodeFor(dryErr), exitCodeFor(realErr))
		assert.Equal(t, dryErr.Error(), realErr.Error())
		assert.NotContains(t, dryLogs.String(), "dry run: no rows written")
		assert.NotContains(t, realLogs.String(), "restore complete")

		// With --force the dry run previews the append and writes nothing.
		dryLogs, dryErr = restoreBackup(t, tgtURL, raw, "--dry-run", "--force")
		require.NoError(t, dryErr)
		dry := logEntry(t, dryLogs.Bytes(), "dry run: no rows written")
		assert.Equal(t, "append", dry["mode"])
		assert.Equal(t, float64(1), dry["existing_channels"])

		target = openSQLite(t, tgtURL)
		monitors, err := target.ListMonitors(context.Background(), false)
		require.NoError(t, err)
		assert.Empty(t, monitors, "dry run wrote rows")

		realLogs, err = restoreBackup(t, tgtURL, raw, "--force")
		require.NoError(t, err)
		real := logEntry(t, realLogs.Bytes(), "restore complete")
		assert.Equal(t, dry["monitors_in_backup"], real["monitors"])
		assert.Equal(t, dry["channels_in_backup"], real["channels"])
	})
}

func TestRestoreRejectsTruncatedBackupBeforeOpeningTheStore(t *testing.T) {
	srcURL := sqliteURL(t)
	src := openSQLite(t, srcURL)
	populateBackupFixture(t, src)
	raw, _ := backupBytes(t, srcURL)

	offsets := []int{1, len(raw) / 4, len(raw) / 2, 3 * len(raw) / 4, len(raw) - 5}
	for _, offset := range offsets {
		if offset <= 0 || offset >= len(raw) {
			continue
		}
		t.Run(fmt.Sprintf("offset_%d", offset), func(t *testing.T) {
			opened := false
			deps := backupDeps{
				stdin:  bytes.NewReader(raw[:offset]),
				stderr: io.Discard,
				open: func() (config.Config, store.Store, error) {
					opened = true
					return config.Config{}, nil, errors.New("store must not be opened")
				},
				log: testLogger(&bytes.Buffer{}),
			}
			err := runRestoreWith(deps, nil)
			require.Error(t, err)
			assert.Equal(t, exitInvalidBackup, exitCodeFor(err))
			assert.Contains(t, err.Error(), "backup")
			assert.False(t, opened, "corrupt input opened the database")
		})
	}
}

func TestRestoreRejectsInconsistentBackupBeforeOpeningTheStore(t *testing.T) {
	srcURL := sqliteURL(t)
	src := openSQLite(t, srcURL)
	populateBackupFixture(t, src)
	raw, _ := backupBytes(t, srcURL)

	mutate := func(t *testing.T, change func(*BackupData)) []byte {
		t.Helper()
		data, err := readBackup(bytes.NewReader(raw))
		require.NoError(t, err)
		change(data)
		out, err := json.Marshal(data)
		require.NoError(t, err)
		return out
	}

	cases := []struct {
		name   string
		change func(*BackupData)
	}{
		{"duplicate monitor id", func(d *BackupData) { d.Monitors = append(d.Monitors, d.Monitors[0]) }},
		{"duplicate channel id", func(d *BackupData) { d.Channels = append(d.Channels, d.Channels[0]) }},
		{"duplicate rule id", func(d *BackupData) { d.Rules = append(d.Rules, d.Rules[0]) }},
		{"rule names a missing monitor", func(d *BackupData) { d.Rules[0].MonitorID = 999 }},
		{"monitor names a missing channel", func(d *BackupData) { d.Monitors[0].ChannelIDs = []int64{999} }},
		{"future version", func(d *BackupData) { d.Version = backupVersion + 1 }},
		{"missing version", func(d *BackupData) { d.Version = 0 }},
		{"missing created_at", func(d *BackupData) { d.CreatedAt = time.Time{} }},
		{"missing monitor created_at", func(d *BackupData) { d.Monitors[0].CreatedAt = time.Time{} }},
		{"null channel config", func(d *BackupData) { d.Channels[0].Config = json.RawMessage(`null`) }},
		{"non-object channel config", func(d *BackupData) { d.Channels[0].Config = json.RawMessage(`123`) }},
		{"unknown digest mode", func(d *BackupData) { d.Channels[0].DigestMode = "hourly" }},
		{"window without seconds", func(d *BackupData) { d.Channels[0].DigestWindowSeconds = 0 }},
		{"null rule params", func(d *BackupData) { d.Rules[0].Params = json.RawMessage(`null`) }},
		{"two default searches", func(d *BackupData) {
			second := d.SavedSearches[0]
			second.ID++
			d.SavedSearches = append(d.SavedSearches, second)
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opened := false
			deps := backupDeps{
				stdin:  bytes.NewReader(mutate(t, tc.change)),
				stderr: io.Discard,
				open: func() (config.Config, store.Store, error) {
					opened = true
					return config.Config{}, nil, errors.New("store must not be opened")
				},
				log: testLogger(&bytes.Buffer{}),
			}
			err := runRestoreWith(deps, nil)
			require.Error(t, err)
			assert.Equal(t, exitInvalidBackup, exitCodeFor(err))
			assert.False(t, opened, "invalid backup opened the database")
		})
	}
}

func TestRestoreExitCodes(t *testing.T) {
	validEmpty := BackupData{Version: backupVersion, CreatedAt: time.Now().UTC()}
	valid, err := json.Marshal(validEmpty)
	require.NoError(t, err)

	t.Run("usage error", func(t *testing.T) {
		deps := backupDeps{
			stdin:  bytes.NewReader(valid),
			stderr: io.Discard,
			open: func() (config.Config, store.Store, error) {
				t.Fatal("unknown flag must not open the store")
				return config.Config{}, nil, nil
			},
			log: testLogger(&bytes.Buffer{}),
		}
		err := runRestoreWith(deps, []string{"--no-such-flag"})
		require.Error(t, err)
		assert.Equal(t, exitUsage, exitCodeFor(err))
	})

	t.Run("corrupt backup", func(t *testing.T) {
		deps := backupDeps{
			stdin:  strings.NewReader("{not json"),
			stderr: io.Discard,
			open: func() (config.Config, store.Store, error) {
				t.Fatal("corrupt input must not open the store")
				return config.Config{}, nil, nil
			},
			log: testLogger(&bytes.Buffer{}),
		}
		err := runRestoreWith(deps, nil)
		require.Error(t, err)
		assert.Equal(t, exitInvalidBackup, exitCodeFor(err))
	})

	t.Run("backend failure", func(t *testing.T) {
		deps := backupDeps{
			stdin:  bytes.NewReader(valid),
			stderr: io.Discard,
			open: func() (config.Config, store.Store, error) {
				return config.Config{}, nil, errors.New("database unavailable")
			},
			log: testLogger(&bytes.Buffer{}),
		}
		err := runRestoreWith(deps, nil)
		require.Error(t, err)
		assert.Equal(t, exitFailure, exitCodeFor(err))
	})

	t.Run("success", func(t *testing.T) {
		_, err := restoreBackup(t, sqliteURL(t), valid)
		require.NoError(t, err)
		assert.Equal(t, 0, exitCodeFor(nil))
	})

	t.Run("backup backend failure", func(t *testing.T) {
		deps := backupDeps{
			stdout: io.Discard,
			stderr: io.Discard,
			open: func() (config.Config, store.Store, error) {
				return config.Config{}, nil, errors.New("database unavailable")
			},
			log: testLogger(&bytes.Buffer{}),
		}
		err := runBackupWith(deps, nil)
		require.Error(t, err)
		assert.Equal(t, exitFailure, exitCodeFor(err))
	})

	t.Run("backup usage error", func(t *testing.T) {
		deps := backupDeps{stdout: io.Discard, stderr: io.Discard, log: testLogger(&bytes.Buffer{})}
		err := runBackupWith(deps, []string{"unexpected"})
		require.Error(t, err)
		assert.Equal(t, exitUsage, exitCodeFor(err))
	})
}

func TestOpenStoreSQLite(t *testing.T) {
	t.Setenv("DATABASE_URL", sqliteURL(t))

	_, st, err := openStore()
	require.NoError(t, err)
	require.NotNil(t, st)
	st.Close()
}

// The store's Create methods were previously unable to preserve created_at,
// which made a restored row look newer than the row it replaced. The timestamp
// must survive the round trip exactly (to the backend's precision).
func TestRestorePreservesCreatedAtTimestamps(t *testing.T) {
	srcURL := sqliteURL(t)
	tgtURL := sqliteURL(t)
	src := openSQLite(t, srcURL)
	populateBackupFixture(t, src)
	raw, data := backupBytes(t, srcURL)

	_, err := restoreBackup(t, tgtURL, raw)
	require.NoError(t, err)

	target := openSQLite(t, tgtURL)
	monitor := findMonitorByName(t, target, data.Monitors[0].Name)
	assert.True(t, fixtureCreatedAt().Equal(monitor.CreatedAt), "restore did not preserve created_at")
	assert.True(t, data.Monitors[0].CreatedAt.Equal(monitor.CreatedAt))
	channel := findChannelByName(t, target, data.Channels[0].Name)
	assert.True(t, fixtureCreatedAt().Equal(channel.CreatedAt), "restore did not preserve created_at")
	assert.True(t, data.Channels[0].CreatedAt.Equal(channel.CreatedAt))
}
