package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// conformanceStore is a Store plus the small test-only hooks a backend must
// provide so the shared suite can inspect raw state and move clocks without
// knowing which database it is talking to. Both *Postgres and *SQLite
// implement it (see postgres_test.go and sqlite_test.go), which is what makes
// the two backends impossible to drift: every assertion below runs twice.
type conformanceStore interface {
	Store
	// resetConformance empties every table and restores ingest_state.
	resetConformance(ctx context.Context) error
	// setAlertCreatedAt backdates one alert row.
	setAlertCreatedAt(ctx context.Context, id int64, at time.Time) error
	// backdateRuleLastAlert moves a rule's cooldown window into the past,
	// so cooldown-expiry tests do not have to sleep.
	backdateRuleLastAlert(ctx context.Context, id int64, d time.Duration) error
	// rawChannelConfig returns channels.config exactly as stored, before any
	// decryption, so the encryption tests can assert on the column itself.
	rawChannelConfig(ctx context.Context, id int64) ([]byte, error)
	// setCipher installs (or replaces) the config cipher.
	setCipher(c ConfigCipher)
}

// conformanceFactory returns a fresh, empty store for one test.
type conformanceFactory func(t *testing.T) conformanceStore

// runStoreConformance executes the backend-neutral store suite. A backend is
// conformant when it passes every subtest unchanged; the Postgres and SQLite
// implementations are the two callers.
func runStoreConformance(t *testing.T, newStore conformanceFactory) {
	t.Helper()
	t.Run("MonitorCRUD", func(t *testing.T) { testMonitorCRUD(t, newStore) })
	t.Run("MonitorPriority", func(t *testing.T) { testMonitorPriority(t, newStore) })
	t.Run("SetMonitorsEnabledAtomicUnknownIDs", func(t *testing.T) { testSetMonitorsEnabled(t, newStore) })
	t.Run("MonitorsAndChannelsKeysetPagination", func(t *testing.T) { testKeysetPagination(t, newStore) })
	t.Run("ListMonitorsPageSearchFilterSort", func(t *testing.T) { testListMonitorsPage(t, newStore) })
	t.Run("RuleCRUDAndCascade", func(t *testing.T) { testRuleCRUD(t, newStore) })
	t.Run("CreateRulesAtomic", func(t *testing.T) { testCreateRulesAtomic(t, newStore) })
	t.Run("ChannelsAndAttachments", func(t *testing.T) { testChannelsAndAttachments(t, newStore) })
	t.Run("ListChannelsTypeAndEnabledFilters", func(t *testing.T) { testListChannelsFilters(t, newStore) })
	t.Run("MonitorLastMatchedAt", func(t *testing.T) { testMonitorLastMatchedAt(t, newStore) })
	t.Run("AlertDedupAndListing", func(t *testing.T) { testAlertDedupAndListing(t, newStore) })
	t.Run("CreateAlertCooldown", func(t *testing.T) { testCreateAlertCooldown(t, newStore) })
	t.Run("CreateAlertCooldownConcurrent", func(t *testing.T) { testCreateAlertCooldownConcurrent(t, newStore) })
	t.Run("ListAlertsSearchFilterSort", func(t *testing.T) { testListAlertsSearchFilterSort(t, newStore) })
	t.Run("DeliveryAttempts", func(t *testing.T) { testDeliveryAttempts(t, newStore) })
	t.Run("DeleteExpiredAlertsKeepsRecentAndCascades", func(t *testing.T) { testDeleteExpiredAlertsCascade(t, newStore) })
	t.Run("DeleteExpiredAlertsBatches", func(t *testing.T) { testDeleteExpiredAlertsBatches(t, newStore) })
	t.Run("IngestStateRoundTrip", func(t *testing.T) { testIngestState(t, newStore) })
	t.Run("GetStats", func(t *testing.T) { testGetStats(t, newStore) })
	t.Run("AlertCountsByDayZeroFillAndWindow", func(t *testing.T) { testAlertCountsByDay(t, newStore) })
	t.Run("DuplicateMonitorCopiesRulesChannelsDisabledUniqueName", func(t *testing.T) { testDuplicateMonitor(t, newStore) })
	t.Run("LedgerHashesAndAlertRetraction", func(t *testing.T) { testLedgerHashesAndRetraction(t, newStore) })
	t.Run("ChannelConfigNoKeyStaysPlaintext", func(t *testing.T) { testChannelConfigNoKey(t, newStore) })
	t.Run("ChannelConfigEncryptedAtRest", func(t *testing.T) { testChannelConfigEncrypted(t, newStore) })
	t.Run("ChannelConfigLegacyPlaintextThenReencrypts", func(t *testing.T) { testChannelConfigLegacy(t, newStore) })
	t.Run("ChannelConfigDecryptFailureNamesChannel", func(t *testing.T) { testChannelConfigDecryptFailure(t, newStore) })
	t.Run("AuditLogAppendOnlyFiltersAndNoSecrets", func(t *testing.T) { testAuditLog(t, newStore) })
	t.Run("ChannelDigestSettingsAndQueue", func(t *testing.T) { testChannelDigest(t, newStore) })
}

// testChannelDigest pins the digest settings round trip and the pending
// queue, including that rows cascade when their channel is deleted.
func testChannelDigest(t *testing.T, newStore conformanceFactory) {
	ctx := context.Background()
	st := newStore(t)

	ch := &Channel{
		Name: "digest", Type: "slack", Config: json.RawMessage(`{}`), Enabled: true,
		DigestMode: DigestModeWindow, DigestWindowSeconds: 300,
	}
	require.NoError(t, st.CreateChannel(ctx, ch))

	got, err := st.GetChannel(ctx, ch.ID)
	require.NoError(t, err)
	assert.Equal(t, DigestModeWindow, got.DigestMode)
	assert.Equal(t, int64(300), got.DigestWindowSeconds)

	got.DigestMode = DigestModeOff
	got.DigestWindowSeconds = 0
	require.NoError(t, st.UpdateChannel(ctx, got))
	got, err = st.GetChannel(ctx, ch.ID)
	require.NoError(t, err)
	assert.Equal(t, DigestModeOff, got.DigestMode)
	assert.Zero(t, got.DigestWindowSeconds)

	require.NoError(t, st.PushDigestAlert(ctx, ch.ID, json.RawMessage(`{"id":1}`)))
	require.NoError(t, st.PushDigestAlert(ctx, ch.ID, json.RawMessage(`{"id":2}`)))
	rows, err := st.ListDigestAlerts(ctx, ch.ID)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.JSONEq(t, `{"id":1}`, string(rows[0].Payload))

	require.NoError(t, st.DeleteDigestAlerts(ctx, ch.ID, []int64{rows[0].ID}))
	rows, err = st.ListDigestAlerts(ctx, ch.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	// Deleting the channel cascades to its pending rows.
	require.NoError(t, st.DeleteChannel(ctx, ch.ID))
	rows, err = st.ListDigestAlerts(ctx, ch.ID)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// testAuditLog proves the audit table is append-only, filters correctly, and
// never carries the secret values a channel config holds.
func testAuditLog(t *testing.T, newStore conformanceFactory) {
	ctx := context.Background()
	st := newStore(t)

	ch := &Channel{
		Name:    "audit channel",
		Type:    "slack",
		Config:  json.RawMessage(`{"webhook_url":"https://hooks.example/SECRET-TOKEN"}`),
		Enabled: true,
	}
	require.NoError(t, st.CreateChannel(ctx, ch))

	// The middleware records field names, never values; this mirrors what it
	// writes so the store contract (and the absence of the secret) is pinned.
	entries := []*AuditEntry{
		{Actor: "req-1", Action: AuditActionCreate, TargetType: "channel", TargetID: ch.ID, Diff: json.RawMessage(`{"fields":["config","name"]}`)},
		{Actor: "req-2", Action: AuditActionUpdate, TargetType: "channel", TargetID: ch.ID, Diff: json.RawMessage(`{"fields":["config"]}`)},
		{Actor: "req-3", Action: AuditActionDelete, TargetType: "monitor", TargetID: 42},
	}
	for _, e := range entries {
		require.NoError(t, st.CreateAuditEntry(ctx, e))
		assert.NotZero(t, e.ID)
		assert.False(t, e.CreatedAt.IsZero())
	}

	// Append-only: all three survive, newest first.
	list, err := st.ListAuditEntries(ctx, AuditFilter{})
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, entries[2].ID, list[0].ID)
	for _, e := range list {
		assert.NotContains(t, string(e.Diff), "SECRET-TOKEN")
	}
	// An entry with no diff is stored as an empty JSON object, never NULL.
	assert.JSONEq(t, `{}`, string(list[0].Diff))

	byType, err := st.ListAuditEntries(ctx, AuditFilter{TargetType: "channel"})
	require.NoError(t, err)
	require.Len(t, byType, 2)

	byTarget, err := st.ListAuditEntries(ctx, AuditFilter{TargetType: "channel", TargetID: ch.ID})
	require.NoError(t, err)
	require.Len(t, byTarget, 2)

	limited, err := st.ListAuditEntries(ctx, AuditFilter{Limit: 1})
	require.NoError(t, err)
	require.Len(t, limited, 1)

	// A time window that excludes everything returns nothing.
	none, err := st.ListAuditEntries(ctx, AuditFilter{From: time.Now().Add(time.Hour)})
	require.NoError(t, err)
	assert.Empty(t, none)
}

// TestClampAlertSeriesDays pins the overview-chart window bounds. It is pure
// and therefore backend-neutral.
func TestClampAlertSeriesDays(t *testing.T) {
	assert.Equal(t, AlertSeriesDays, ClampAlertSeriesDays(0))
	assert.Equal(t, AlertSeriesDays, ClampAlertSeriesDays(-3))
	assert.Equal(t, 7, ClampAlertSeriesDays(7))
	assert.Equal(t, 90, ClampAlertSeriesDays(1000))
}

// TestCopyMonitorName covers the duplicate-naming helper, which both backends
// use when duplicating a monitor.
func TestCopyMonitorName(t *testing.T) {
	assert.Equal(t, "m (copy)", CopyMonitorName("m", nil))
	assert.Equal(t, "m (copy)", CopyMonitorName("m", []string{"m"}))
	assert.Equal(t, "m (copy 2)", CopyMonitorName("m", []string{"m", "m (copy)"}))
	assert.Equal(t, "m (copy 3)", CopyMonitorName("m", []string{"m", "m (copy)", "m (copy 2)"}))
}

func testMonitorCRUD(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "m1", ContractIDs: []string{"CAAA", "CBBB"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	assert.NotZero(t, m.ID)
	assert.False(t, m.CreatedAt.IsZero())

	got, err := st.GetMonitor(ctx, m.ID)
	require.NoError(t, err)
	assert.Equal(t, "m1", got.Name)
	assert.Equal(t, []string{"CAAA", "CBBB"}, got.ContractIDs)
	assert.Nil(t, got.LastMatchedAt, "new monitors must stay unmatched")

	m.Name = "renamed"
	m.Enabled = false
	require.NoError(t, st.UpdateMonitor(ctx, m))

	list, err := st.ListMonitors(ctx, true)
	require.NoError(t, err)
	assert.Empty(t, list, "disabled monitor filtered out")
	list, err = st.ListMonitors(ctx, false)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "renamed", list[0].Name)

	require.NoError(t, st.DeleteMonitor(ctx, m.ID))
	_, err = st.GetMonitor(ctx, m.ID)
	assert.ErrorIs(t, err, ErrNotFound)
	assert.ErrorIs(t, st.DeleteMonitor(ctx, m.ID), ErrNotFound)
}

// testMonitorPriority pins the priority column across both backends: an
// unset priority is stored as the middle tier so pre-priority monitors are
// unchanged, the value round-trips, and a duplicate keeps it.
func testMonitorPriority(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	// No priority set: the zero value must read back as normal, not empty.
	plain := &Monitor{Name: "plain", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, plain))
	got, err := st.GetMonitor(ctx, plain.ID)
	require.NoError(t, err)
	assert.Equal(t, PriorityNormal, got.Priority)

	high := &Monitor{Name: "high", ContractIDs: []string{"C"}, Enabled: true, Priority: PriorityHigh}
	require.NoError(t, st.CreateMonitor(ctx, high))
	got, err = st.GetMonitor(ctx, high.ID)
	require.NoError(t, err)
	assert.Equal(t, PriorityHigh, got.Priority)

	// ListMonitors and the paged listing carry the value too.
	list, err := st.ListMonitors(ctx, false)
	require.NoError(t, err)
	require.Len(t, list, 2)
	byID := map[int64]Priority{}
	for _, m := range list {
		byID[m.ID] = m.Priority
	}
	assert.Equal(t, PriorityNormal, byID[plain.ID])
	assert.Equal(t, PriorityHigh, byID[high.ID])

	page, err := st.ListMonitorsPage(ctx, ListFilter{Sort: "id", Limit: 50})
	require.NoError(t, err)
	require.Len(t, page, 2)
	assert.Equal(t, PriorityHigh, page[0].Priority, "newest first")

	// Update moves the tier, and an explicit empty value normalises back.
	high.Priority = PriorityLow
	require.NoError(t, st.UpdateMonitor(ctx, high))
	got, err = st.GetMonitor(ctx, high.ID)
	require.NoError(t, err)
	assert.Equal(t, PriorityLow, got.Priority)

	high.Priority = ""
	require.NoError(t, st.UpdateMonitor(ctx, high))
	got, err = st.GetMonitor(ctx, high.ID)
	require.NoError(t, err)
	assert.Equal(t, PriorityNormal, got.Priority)

	// A duplicate keeps the source priority: it is queue position, not the
	// safety switch that forces the copy disabled.
	high.Priority = PriorityHigh
	require.NoError(t, st.UpdateMonitor(ctx, high))
	copy, err := st.DuplicateMonitor(ctx, high.ID)
	require.NoError(t, err)
	assert.Equal(t, PriorityHigh, copy.Priority)
	assert.False(t, copy.Enabled)
}

func testSetMonitorsEnabled(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	a := &Monitor{Name: "a", ContractIDs: []string{"C"}, Enabled: true}
	b := &Monitor{Name: "b", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, a))
	require.NoError(t, st.CreateMonitor(ctx, b))

	updated, unknown, err := st.SetMonitorsEnabled(ctx, []int64{a.ID, b.ID, 99999, a.ID}, false)
	require.NoError(t, err)
	assert.Equal(t, 2, updated)
	require.Equal(t, []int64{99999}, unknown)

	gotA, err := st.GetMonitor(ctx, a.ID)
	require.NoError(t, err)
	gotB, err := st.GetMonitor(ctx, b.ID)
	require.NoError(t, err)
	assert.False(t, gotA.Enabled)
	assert.False(t, gotB.Enabled)

	updated, unknown, err = st.SetMonitorsEnabled(ctx, []int64{a.ID}, true)
	require.NoError(t, err)
	assert.Equal(t, 1, updated)
	assert.Empty(t, unknown)
	gotA, err = st.GetMonitor(ctx, a.ID)
	require.NoError(t, err)
	assert.True(t, gotA.Enabled)
}

func testKeysetPagination(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	// Seven monitors, newest last by id. Mix enabled so the enabled-only
	// filter has something to compose with the cursor.
	for i := 0; i < 7; i++ {
		m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: i%2 == 0}
		require.NoError(t, st.CreateMonitor(ctx, m))
	}
	for i := 0; i < 7; i++ {
		c := &Channel{Name: "c", Type: "webhook", Config: json.RawMessage(`{}`), Enabled: i%2 == 0}
		require.NoError(t, st.CreateChannel(ctx, c))
	}

	collectIDs := func(page func(after int64) []int64) []int64 {
		var all []int64
		seen := map[int64]bool{}
		var after int64
		for {
			ids := page(after)
			if len(ids) == 0 {
				break
			}
			for _, id := range ids {
				if seen[id] {
					t.Fatalf("duplicate id %d across pages", id)
				}
				seen[id] = true
				all = append(all, id)
			}
			if len(ids) < 3 {
				break
			}
			after = ids[len(ids)-1]
		}
		return all
	}

	monIDs := collectIDs(func(after int64) []int64 {
		list, err := st.ListMonitorsPage(ctx, ListFilter{Limit: 3, AfterID: after, Sort: "id"})
		require.NoError(t, err)
		ids := make([]int64, len(list))
		for i, m := range list {
			ids[i] = m.ID
		}
		return ids
	})
	require.Len(t, monIDs, 7, "every monitor must appear exactly once")
	for i := 1; i < len(monIDs); i++ {
		assert.Greater(t, monIDs[i-1], monIDs[i], "newest-first, no gaps in order")
	}

	chIDs := collectIDs(func(after int64) []int64 {
		list, err := st.ListChannelsPage(ctx, ListFilter{Limit: 3, AfterID: after})
		require.NoError(t, err)
		ids := make([]int64, len(list))
		for i, c := range list {
			ids[i] = c.ID
		}
		return ids
	})
	require.Len(t, chIDs, 7, "every channel must appear exactly once")

	enabled, err := st.ListMonitorsPage(ctx, ListFilter{EnabledOnly: true, Limit: 50, Sort: "id"})
	require.NoError(t, err)
	require.Len(t, enabled, 4)
	for _, m := range enabled {
		assert.True(t, m.Enabled)
	}
}

func testListMonitorsPage(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()
	names := []struct {
		name    string
		enabled bool
	}{
		{"Alpha treasury", true},
		{"beta vault", false},
		{"Gamma treasury", true},
		{"other", true},
	}
	for _, n := range names {
		m := &Monitor{Name: n.name, ContractIDs: []string{"C"}, Enabled: n.enabled}
		require.NoError(t, st.CreateMonitor(ctx, m))
	}

	on := true
	off := false

	list, err := st.ListMonitorsPage(ctx, ListFilter{Query: "TREASURY", Limit: 50})
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "Alpha treasury", list[0].Name)
	assert.Equal(t, "Gamma treasury", list[1].Name)

	list, err = st.ListMonitorsPage(ctx, ListFilter{Query: "treasury", Enabled: &on, Limit: 50})
	require.NoError(t, err)
	require.Len(t, list, 2)

	list, err = st.ListMonitorsPage(ctx, ListFilter{Enabled: &off, Limit: 50})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "beta vault", list[0].Name)

	list, err = st.ListMonitorsPage(ctx, ListFilter{Query: "100%", Limit: 50})
	require.NoError(t, err)
	assert.Empty(t, list, "LIKE metacharacters must not become wildcards")

	list, err = st.ListMonitorsPage(ctx, ListFilter{Sort: "name", Limit: 50})
	require.NoError(t, err)
	require.Len(t, list, 4)
	assert.Equal(t, "Alpha treasury", list[0].Name)
	assert.Equal(t, "beta vault", list[1].Name, "name sort is case-insensitive")
	assert.Equal(t, "Gamma treasury", list[2].Name)
	assert.Equal(t, "other", list[3].Name)

	// Name-sort keyset: after Alpha, the next page starts at beta.
	page1, err := st.ListMonitorsPage(ctx, ListFilter{Sort: "name", Limit: 1})
	require.NoError(t, err)
	require.Len(t, page1, 1)
	page2, err := st.ListMonitorsPage(ctx, ListFilter{Sort: "name", Limit: 1, AfterID: page1[0].ID})
	require.NoError(t, err)
	require.Len(t, page2, 1)
	assert.Equal(t, "beta vault", page2[0].Name)
}

func testRuleCRUD(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))

	r := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{"event_name":"transfer"}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, r))

	rules, err := st.ListRules(ctx, m.ID, true)
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.JSONEq(t, `{"event_name":"transfer"}`, string(rules[0].Params))

	r.Enabled = false
	require.NoError(t, st.UpdateRule(ctx, r))
	rules, err = st.ListRules(ctx, m.ID, true)
	require.NoError(t, err)
	assert.Empty(t, rules)

	// Deleting the monitor cascades to its rules.
	require.NoError(t, st.DeleteMonitor(ctx, m.ID))
	_, err = st.GetRule(ctx, r.ID)
	assert.ErrorIs(t, err, ErrNotFound)
}

func testCreateRulesAtomic(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))

	ok := []*Rule{
		{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{"event_name":"transfer"}`), Enabled: true},
		{MonitorID: m.ID, Type: "token_event", Params: json.RawMessage(`{"event":"mint"}`), Enabled: false},
	}
	require.NoError(t, st.CreateRules(ctx, ok))
	require.NotZero(t, ok[0].ID)
	require.NotZero(t, ok[1].ID)
	require.Greater(t, ok[1].ID, ok[0].ID)

	list, err := st.ListRules(ctx, m.ID, false)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, ok[0].ID, list[0].ID)
	assert.Equal(t, ok[1].ID, list[1].ID)

	// A later row that fails the monitor FK must not leave the first row of
	// this batch in the table — the insert is one transaction.
	bad := []*Rule{
		{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{"event_name":"burn"}`), Enabled: true},
		{MonitorID: m.ID + 999, Type: "event_emitted", Params: json.RawMessage(`{"event_name":"clawback"}`), Enabled: true},
	}
	err = st.CreateRules(ctx, bad)
	require.Error(t, err)

	list, err = st.ListRules(ctx, m.ID, false)
	require.NoError(t, err)
	require.Len(t, list, 2, "failed batch must not insert a prefix of the rules")
}

func testChannelsAndAttachments(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))

	c1 := &Channel{Name: "c1", Type: "webhook", Config: json.RawMessage(`{"url":"u","secret":"s"}`), Enabled: true}
	c2 := &Channel{Name: "c2", Type: "slack", Config: json.RawMessage(`{"webhook_url":"u"}`), Enabled: false}
	require.NoError(t, st.CreateChannel(ctx, c1))
	require.NoError(t, st.CreateChannel(ctx, c2))

	require.NoError(t, st.SetMonitorChannels(ctx, m.ID, []int64{c1.ID, c2.ID}))
	got, err := st.GetMonitor(ctx, m.ID)
	require.NoError(t, err)
	assert.Equal(t, []int64{c1.ID, c2.ID}, got.ChannelIDs)

	attached, err := st.ListChannelsForMonitor(ctx, m.ID)
	require.NoError(t, err)
	require.Len(t, attached, 1, "disabled channels are excluded from dispatch")
	assert.Equal(t, c1.ID, attached[0].ID)

	require.NoError(t, st.SetMonitorChannels(ctx, m.ID, []int64{c2.ID}))
	got, err = st.GetMonitor(ctx, m.ID)
	require.NoError(t, err)
	assert.Equal(t, []int64{c2.ID}, got.ChannelIDs)
}

func testListChannelsFilters(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	slackOn := &Channel{Name: "ops-slack", Type: "slack", Config: json.RawMessage(`{"webhook_url":"u"}`), Enabled: true}
	webhookOn := &Channel{Name: "ops-hook", Type: "webhook", Config: json.RawMessage(`{"url":"u","secret":"s"}`), Enabled: true}
	slackOff := &Channel{Name: "quiet-slack", Type: "slack", Config: json.RawMessage(`{"webhook_url":"u"}`), Enabled: false}
	require.NoError(t, st.CreateChannel(ctx, slackOn))
	require.NoError(t, st.CreateChannel(ctx, webhookOn))
	require.NoError(t, st.CreateChannel(ctx, slackOff))

	all, err := st.ListChannelsPage(ctx, ListFilter{})
	require.NoError(t, err)
	require.Len(t, all, 3)

	byType, err := st.ListChannelsPage(ctx, ListFilter{Type: "slack"})
	require.NoError(t, err)
	require.Len(t, byType, 2, "type filter is applied in SQL, not after fetch")
	assert.Equal(t, "slack", byType[0].Type)
	assert.Equal(t, "slack", byType[1].Type)

	composed, err := st.ListChannelsPage(ctx, ListFilter{Type: "slack", EnabledOnly: true})
	require.NoError(t, err)
	require.Len(t, composed, 1)
	assert.Equal(t, slackOn.ID, composed[0].ID)

	unknown, err := st.ListChannelsPage(ctx, ListFilter{Type: "not-a-real-type"})
	require.NoError(t, err)
	assert.Empty(t, unknown, "unknown types return an empty list, not an error")
}

func testMonitorLastMatchedAt(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	r := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, r))

	got, err := st.GetMonitor(ctx, m.ID)
	require.NoError(t, err)
	assert.Nil(t, got.LastMatchedAt)

	older := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	created, err := st.CreateAlert(ctx, &Alert{
		MonitorID: m.ID, RuleID: r.ID, EventID: "ev-new", LedgerClosedAt: newer,
	})
	require.NoError(t, err)
	assert.Equal(t, AlertCreated, created)

	got, err = st.GetMonitor(ctx, m.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastMatchedAt)
	assert.True(t, got.LastMatchedAt.Equal(newer), "got %v", got.LastMatchedAt)

	created, err = st.CreateAlert(ctx, &Alert{
		MonitorID: m.ID, RuleID: r.ID, EventID: "ev-old", LedgerClosedAt: older,
	})
	require.NoError(t, err)
	assert.Equal(t, AlertCreated, created)
	got, err = st.GetMonitor(ctx, m.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastMatchedAt)
	assert.True(t, got.LastMatchedAt.Equal(newer), "older ledger close must not overwrite")

	created, err = st.CreateAlert(ctx, &Alert{
		MonitorID: m.ID, RuleID: r.ID, EventID: "ev-new", LedgerClosedAt: newer.Add(time.Hour),
	})
	require.NoError(t, err)
	assert.Equal(t, AlertDuplicate, created, "dedup must not restamp")
	got, err = st.GetMonitor(ctx, m.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastMatchedAt)
	assert.True(t, got.LastMatchedAt.Equal(newer))

	created, err = st.CreateAlert(ctx, &Alert{
		MonitorID: m.ID, RuleID: r.ID, EventID: "ev-plain",
	})
	require.NoError(t, err)
	assert.Equal(t, AlertCreated, created)
	got, err = st.GetMonitor(ctx, m.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastMatchedAt)
	assert.True(t, got.LastMatchedAt.Equal(newer), "zero LedgerClosedAt must not stamp wall clock")
}

func testAlertDedupAndListing(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	r := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, r))

	a := &Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "ev-1", Payload: json.RawMessage(`{"k":"v"}`)}
	created, err := st.CreateAlert(ctx, a)
	require.NoError(t, err)
	assert.Equal(t, AlertCreated, created)
	assert.NotZero(t, a.ID)

	got, err := st.GetAlert(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, a.ID, got.ID)
	assert.Equal(t, "ev-1", got.EventID)
	assert.JSONEq(t, `{"k":"v"}`, string(got.Payload))

	_, err = st.GetAlert(ctx, a.ID+9999)
	require.ErrorIs(t, err, ErrNotFound)

	dup := &Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "ev-1"}
	created, err = st.CreateAlert(ctx, dup)
	require.NoError(t, err)
	assert.Equal(t, AlertDuplicate, created, "same (rule_id, event_id) must dedup")

	b := &Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "ev-2"}
	created, err = st.CreateAlert(ctx, b)
	require.NoError(t, err)
	assert.Equal(t, AlertCreated, created)

	list, err := st.ListAlerts(ctx, AlertFilter{MonitorID: m.ID})
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "ev-2", list[0].EventID, "newest first")

	// Keyset pagination.
	list, err = st.ListAlerts(ctx, AlertFilter{AfterID: list[0].ID})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "ev-1", list[0].EventID)

	// Time range filter.
	list, err = st.ListAlerts(ctx, AlertFilter{To: time.Now().Add(-time.Hour)})
	require.NoError(t, err)
	assert.Empty(t, list)
}

// testCreateAlertCooldown covers the DB-side half of the cooldown: the window
// is enforced against the rule row, the suppressed count is surfaced on the
// next alert, and a zero cooldown leaves behaviour unchanged. Postgres does it
// with SELECT ... FOR UPDATE; SQLite holds the whole-database write lock. Both
// must produce exactly these outcomes.
func testCreateAlertCooldown(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	r := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, r))

	her := func(eventID string) *Alert {
		return &Alert{
			MonitorID: m.ID, RuleID: r.ID, EventID: eventID,
			Payload: json.RawMessage(`{"event_name":"transfer"}`), Cooldown: 5 * time.Minute,
		}
	}

	first := her("ev-1")
	outcome, err := st.CreateAlert(ctx, first)
	require.NoError(t, err)
	assert.Equal(t, AlertCreated, outcome)
	assert.Zero(t, first.SuppressedSinceLast)

	// The burst is suppressed and counted, and nothing is written for it.
	for _, id := range []string{"ev-2", "ev-3", "ev-4"} {
		outcome, err := st.CreateAlert(ctx, her(id))
		require.NoError(t, err)
		assert.Equal(t, AlertSuppressed, outcome)
	}

	// The event that opened the window is a duplicate, not a fresh match, so a
	// replay must not inflate the count.
	outcome, err = st.CreateAlert(ctx, her("ev-1"))
	require.NoError(t, err)
	assert.Equal(t, AlertDuplicate, outcome)

	// Move the window into the past instead of sleeping.
	require.NoError(t, st.backdateRuleLastAlert(ctx, r.ID, 10*time.Minute))

	next := her("ev-5")
	outcome, err = st.CreateAlert(ctx, next)
	require.NoError(t, err)
	require.Equal(t, AlertCreated, outcome)
	assert.EqualValues(t, 3, next.SuppressedSinceLast, "the count of the three suppressed matches")

	got, err := st.GetAlert(ctx, next.ID)
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(got.Payload, &payload))
	assert.EqualValues(t, 3, payload["suppressed_since_last"], "the stored payload reports the count")

	// A rule without a cooldown is unaffected even right after an alert.
	plain := &Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "ev-plain", Payload: json.RawMessage(`{}`)}
	outcome, err = st.CreateAlert(ctx, plain)
	require.NoError(t, err)
	assert.Equal(t, AlertCreated, outcome)
}

// testCreateAlertCooldownConcurrent models two pollers racing on the same rule:
// exactly one alert may fire inside the window. SQLite achieves the same
// guarantee as Postgres' rule-row lock through its single writer.
func testCreateAlertCooldownConcurrent(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	r := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, r))

	const n = 8
	outcomes := make([]AlertOutcome, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outcomes[i], errs[i] = st.CreateAlert(ctx, &Alert{
				MonitorID: m.ID, RuleID: r.ID, EventID: fmt.Sprintf("ev-%d", i),
				Payload: json.RawMessage(`{}`), Cooldown: time.Minute,
			})
		}()
	}
	wg.Wait()

	created := 0
	for i := range outcomes {
		require.NoError(t, errs[i])
		if outcomes[i] == AlertCreated {
			created++
		}
	}
	assert.Equal(t, 1, created, "exactly one alert may fire inside the window")
}

func testListAlertsSearchFilterSort(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	m1 := &Monitor{Name: "m1", ContractIDs: []string{"CAAA"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m1))
	m2 := &Monitor{Name: "m2", ContractIDs: []string{"CBBB"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m2))
	r1 := &Rule{MonitorID: m1.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, r1))
	r2 := &Rule{MonitorID: m2.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, r2))

	payloads := []struct {
		monitor *Monitor
		rule    *Rule
		event   string
		payload string
	}{
		{m1, r1, "ev-a1", `{"contract_id":"CAAA"}`},
		{m1, r1, "ev-a2", `{"contract_id":"CAAA"}`},
		{m1, r1, "ev-a3", `{"contract_id":"CZZZ"}`},
		{m2, r2, "ev-b1", `{"contract_id":"CBBB"}`},
		{m2, r2, "ev-b2", `{"contract_id":"CBBB"}`},
		{m2, r2, "ev-b3", `{"contract_id":"CBBB"}`},
		{m2, r2, "ev-b4", `{"contract_id":"CBBB"}`},
	}
	var created []Alert
	for _, p := range payloads {
		a := &Alert{
			MonitorID: p.monitor.ID, RuleID: p.rule.ID, EventID: p.event,
			Payload: json.RawMessage(p.payload),
		}
		ok, err := st.CreateAlert(ctx, a)
		require.NoError(t, err)
		require.Equal(t, AlertCreated, ok)
		created = append(created, *a)
	}

	byRule, err := st.ListAlerts(ctx, AlertFilter{RuleID: r1.ID, Limit: 50})
	require.NoError(t, err)
	require.Len(t, byRule, 3)
	for _, a := range byRule {
		assert.Equal(t, r1.ID, a.RuleID)
	}

	byContract, err := st.ListAlerts(ctx, AlertFilter{ContractID: "CAAA", Limit: 50})
	require.NoError(t, err)
	require.Len(t, byContract, 2)

	composed, err := st.ListAlerts(ctx, AlertFilter{MonitorID: m1.ID, RuleID: r1.ID, ContractID: "CAAA", Limit: 50})
	require.NoError(t, err)
	require.Len(t, composed, 2)

	// Paging in both directions must cover every id once: no duplicates, no gaps.
	pageIDs := func(sort string, limit int) []int64 {
		t.Helper()
		var ids []int64
		seen := map[int64]bool{}
		var after int64
		for i := 0; i < 10; i++ {
			page, err := st.ListAlerts(ctx, AlertFilter{Sort: sort, Limit: limit, AfterID: after})
			require.NoError(t, err)
			if len(page) == 0 {
				break
			}
			for _, a := range page {
				if seen[a.ID] {
					t.Fatalf("duplicate id %d under sort %s", a.ID, sort)
				}
				seen[a.ID] = true
				ids = append(ids, a.ID)
			}
			if len(page) < limit {
				break
			}
			after = page[len(page)-1].ID
		}
		return ids
	}

	desc := pageIDs("created_at_desc", 3)
	asc := pageIDs("created_at_asc", 3)
	require.Len(t, desc, len(created))
	require.Len(t, asc, len(created))
	assert.Equal(t, created[len(created)-1].ID, desc[0], "desc starts at newest")
	assert.Equal(t, created[0].ID, asc[0], "asc starts at oldest")
	assert.Equal(t, desc[0], asc[len(asc)-1])
	assert.Equal(t, desc[len(desc)-1], asc[0])
}

func testDeliveryAttempts(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	r := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, r))
	c := &Channel{Name: "c", Type: "webhook", Config: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateChannel(ctx, c))
	a := &Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "ev-1"}
	_, err := st.CreateAlert(ctx, a)
	require.NoError(t, err)

	d1 := &DeliveryAttempt{AlertID: a.ID, ChannelID: c.ID, Status: "failed", ResponseSnippet: "boom"}
	d2 := &DeliveryAttempt{AlertID: a.ID, ChannelID: c.ID, Status: "success"}
	require.NoError(t, st.RecordDeliveryAttempt(ctx, d1))
	require.NoError(t, st.RecordDeliveryAttempt(ctx, d2))

	list, err := st.ListDeliveryAttempts(ctx, a.ID, "")
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, DeliveryStatusFailed, list[0].Status)
	assert.Equal(t, "boom", list[0].ResponseSnippet)
	assert.Equal(t, DeliveryStatusSuccess, list[1].Status)

	failed, err := st.ListDeliveryAttempts(ctx, a.ID, DeliveryStatusFailed)
	require.NoError(t, err)
	require.Len(t, failed, 1)
	assert.Equal(t, d1.ID, failed[0].ID)

	ok, err := st.ListDeliveryAttempts(ctx, a.ID, DeliveryStatusSuccess)
	require.NoError(t, err)
	require.Len(t, ok, 1)
	assert.Equal(t, d2.ID, ok[0].ID)

	got, err := st.GetAlert(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, a.ID, got.ID)
	assert.Equal(t, a.EventID, got.EventID)
	_, err = st.GetAlert(ctx, a.ID+999)
	assert.ErrorIs(t, err, ErrNotFound)
}

func testDeleteExpiredAlertsCascade(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	r := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, r))
	c := &Channel{Name: "c", Type: "webhook", Config: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateChannel(ctx, c))

	oldAlert := &Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "old"}
	recentAlert := &Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "recent"}
	_, err := st.CreateAlert(ctx, oldAlert)
	require.NoError(t, err)
	_, err = st.CreateAlert(ctx, recentAlert)
	require.NoError(t, err)

	oldCutoff := time.Now().Add(-48 * time.Hour)
	require.NoError(t, st.setAlertCreatedAt(ctx, oldAlert.ID, oldCutoff.Add(-time.Hour)))

	oldAttempt := &DeliveryAttempt{AlertID: oldAlert.ID, ChannelID: c.ID, Status: "success"}
	recentAttempt := &DeliveryAttempt{AlertID: recentAlert.ID, ChannelID: c.ID, Status: "success"}
	require.NoError(t, st.RecordDeliveryAttempt(ctx, oldAttempt))
	require.NoError(t, st.RecordDeliveryAttempt(ctx, recentAttempt))

	deleted, err := st.DeleteExpiredAlerts(ctx, time.Now().Add(-24*time.Hour), 1000)
	require.NoError(t, err)
	assert.Equal(t, int64(1), deleted)

	alerts, err := st.ListAlerts(ctx, AlertFilter{})
	require.NoError(t, err)
	require.Len(t, alerts, 1)
	assert.Equal(t, recentAlert.ID, alerts[0].ID)

	oldAttempts, err := st.ListDeliveryAttempts(ctx, oldAlert.ID, "")
	require.NoError(t, err)
	assert.Empty(t, oldAttempts, "delivery_attempts must cascade with the alert")

	kept, err := st.ListDeliveryAttempts(ctx, recentAlert.ID, "")
	require.NoError(t, err)
	require.Len(t, kept, 1)
}

func testDeleteExpiredAlertsBatches(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	r := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, r))

	for i := 0; i < 3; i++ {
		a := &Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "old-" + string(rune('a'+i))}
		_, err := st.CreateAlert(ctx, a)
		require.NoError(t, err)
		require.NoError(t, st.setAlertCreatedAt(ctx, a.ID, time.Now().Add(-48*time.Hour)))
	}

	deleted, err := st.DeleteExpiredAlerts(ctx, time.Now().Add(-24*time.Hour), 2)
	require.NoError(t, err)
	assert.Equal(t, int64(2), deleted)

	deleted, err = st.DeleteExpiredAlerts(ctx, time.Now().Add(-24*time.Hour), 2)
	require.NoError(t, err)
	assert.Equal(t, int64(1), deleted)
}

func testIngestState(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	s, err := st.GetIngestState(ctx)
	require.NoError(t, err)
	assert.Zero(t, s.LastLedger)

	s.LastLedger = 123456
	s.LastCursor = "0000001-0000000"
	require.NoError(t, st.SetIngestState(ctx, s))

	got, err := st.GetIngestState(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint32(123456), got.LastLedger)
	assert.Equal(t, "0000001-0000000", got.LastCursor)
}

func testGetStats(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	r := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, r))
	_, err := st.CreateAlert(ctx, &Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "e"})
	require.NoError(t, err)

	stats, err := st.GetStats(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), stats.Monitors)
	assert.Equal(t, int64(1), stats.Rules)
	assert.Equal(t, int64(1), stats.Alerts)
	assert.Equal(t, int64(1), stats.AlertsLast24)
}

func testAlertCountsByDay(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	empty, err := st.AlertCountsByDay(ctx, AlertSeriesDays)
	require.NoError(t, err)
	require.Len(t, empty, AlertSeriesDays)
	for _, d := range empty {
		assert.Equal(t, int64(0), d.Count, d.Day)
		_, parseErr := time.Parse("2006-01-02", d.Day)
		require.NoError(t, parseErr)
	}
	today := time.Now().UTC()
	assert.Equal(t, today.Format("2006-01-02"), empty[len(empty)-1].Day)
	assert.Equal(t, today.AddDate(0, 0, -(AlertSeriesDays-1)).Format("2006-01-02"), empty[0].Day)

	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	r := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, r))

	noon := func(offsetDays int) time.Time {
		d := today.AddDate(0, 0, offsetDays)
		return time.Date(d.Year(), d.Month(), d.Day(), 12, 0, 0, 0, time.UTC)
	}
	seed := func(eventID string, at time.Time) {
		t.Helper()
		a := &Alert{MonitorID: m.ID, RuleID: r.ID, EventID: eventID}
		_, err := st.CreateAlert(ctx, a)
		require.NoError(t, err)
		require.NoError(t, st.setAlertCreatedAt(ctx, a.ID, at))
	}
	seed("today-a", noon(0))
	seed("today-b", noon(0))
	seed("gap-minus-2", noon(-2))
	seed("too-old", noon(-31))

	series, err := st.AlertCountsByDay(ctx, AlertSeriesDays)
	require.NoError(t, err)
	require.Len(t, series, AlertSeriesDays)
	byDay := map[string]int64{}
	for _, d := range series {
		byDay[d.Day] = d.Count
	}
	assert.Equal(t, int64(2), byDay[today.Format("2006-01-02")])
	assert.Equal(t, int64(0), byDay[today.AddDate(0, 0, -1).Format("2006-01-02")], "gap day must be an explicit zero")
	assert.Equal(t, int64(1), byDay[today.AddDate(0, 0, -2).Format("2006-01-02")])
	_, tooOld := byDay[today.AddDate(0, 0, -31).Format("2006-01-02")]
	assert.False(t, tooOld, "alerts older than the window must not appear")
}

func testDuplicateMonitor(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	src := &Monitor{Name: "alpha", ContractIDs: []string{"CAAA", "CBBB"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, src))
	r1 := &Rule{MonitorID: src.ID, Type: "event_emitted", Params: json.RawMessage(`{"event_name":"transfer"}`), Enabled: true}
	r2 := &Rule{MonitorID: src.ID, Type: "value_threshold", Params: json.RawMessage(`{"min":"1"}`), Enabled: false}
	require.NoError(t, st.CreateRule(ctx, r1))
	require.NoError(t, st.CreateRule(ctx, r2))
	c1 := &Channel{Name: "ops", Type: "webhook", Config: json.RawMessage(`{"url":"u"}`), Enabled: true}
	c2 := &Channel{Name: "pager", Type: "slack", Config: json.RawMessage(`{"webhook_url":"u"}`), Enabled: true}
	require.NoError(t, st.CreateChannel(ctx, c1))
	require.NoError(t, st.CreateChannel(ctx, c2))
	require.NoError(t, st.SetMonitorChannels(ctx, src.ID, []int64{c1.ID, c2.ID}))
	_, err := st.CreateAlert(ctx, &Alert{MonitorID: src.ID, RuleID: r1.ID, EventID: "ev-src"})
	require.NoError(t, err)

	copy, err := st.DuplicateMonitor(ctx, src.ID)
	require.NoError(t, err)
	assert.NotEqual(t, src.ID, copy.ID)
	assert.Equal(t, "alpha (copy)", copy.Name)
	assert.False(t, copy.Enabled, "copy must be created disabled so it cannot alert before review")
	assert.Equal(t, []string{"CAAA", "CBBB"}, copy.ContractIDs)
	assert.Equal(t, []int64{c1.ID, c2.ID}, copy.ChannelIDs)

	got, err := st.GetMonitor(ctx, copy.ID)
	require.NoError(t, err)
	assert.False(t, got.Enabled)
	assert.Equal(t, []int64{c1.ID, c2.ID}, got.ChannelIDs)

	rules, err := st.ListRules(ctx, copy.ID, false)
	require.NoError(t, err)
	require.Len(t, rules, 2)
	assert.Equal(t, "event_emitted", rules[0].Type)
	assert.JSONEq(t, `{"event_name":"transfer"}`, string(rules[0].Params))
	assert.True(t, rules[0].Enabled)
	assert.Equal(t, "value_threshold", rules[1].Type)
	assert.JSONEq(t, `{"min":"1"}`, string(rules[1].Params))
	assert.False(t, rules[1].Enabled)

	srcAlerts, err := st.ListAlerts(ctx, AlertFilter{MonitorID: src.ID})
	require.NoError(t, err)
	require.Len(t, srcAlerts, 1)
	copyAlerts, err := st.ListAlerts(ctx, AlertFilter{MonitorID: copy.ID})
	require.NoError(t, err)
	assert.Empty(t, copyAlerts, "alerts must not be copied")

	second, err := st.DuplicateMonitor(ctx, src.ID)
	require.NoError(t, err)
	assert.Equal(t, "alpha (copy 2)", second.Name)
	assert.False(t, second.Enabled)

	_, err = st.DuplicateMonitor(ctx, 999999)
	assert.ErrorIs(t, err, ErrNotFound)
}

// testLedgerHashesAndRetraction pins the reorg-detection state across both
// backends: hashes upsert and prune by ledger, and retraction marks exactly
// the alerts at or after the divergence without deleting them.
func testLedgerHashesAndRetraction(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	// Hashes upsert and read back in range order.
	require.NoError(t, st.RecordLedgerHashes(ctx, []LedgerHash{
		{Ledger: 200, Hash: "b"},
		{Ledger: 100, Hash: "a"},
		{Ledger: 150, Hash: "c"},
	}))
	hashes, err := st.LedgerHashes(ctx, 100, 200)
	require.NoError(t, err)
	require.Len(t, hashes, 3)
	assert.Equal(t, uint32(100), hashes[0].Ledger)
	assert.Equal(t, "a", hashes[0].Hash)
	assert.Equal(t, uint32(200), hashes[2].Ledger)

	inRange, err := st.LedgerHashes(ctx, 120, 160)
	require.NoError(t, err)
	require.Len(t, inRange, 1)
	assert.Equal(t, uint32(150), inRange[0].Ledger)

	// A changed hash for an existing ledger is recorded as the new value.
	require.NoError(t, st.RecordLedgerHashes(ctx, []LedgerHash{{Ledger: 150, Hash: "c2"}}))
	hashes, err = st.LedgerHashes(ctx, 150, 150)
	require.NoError(t, err)
	require.Len(t, hashes, 1)
	assert.Equal(t, "c2", hashes[0].Hash)

	require.NoError(t, st.PruneLedgerHashes(ctx, 150))
	hashes, err = st.LedgerHashes(ctx, 1, 1000)
	require.NoError(t, err)
	require.Len(t, hashes, 2, "ledgers below the prune boundary are gone")
	assert.Equal(t, uint32(150), hashes[0].Ledger)

	// Retraction marks alerts at or after the divergence, once.
	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	r := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, r))

	old := &Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "old", Ledger: 100}
	orphan := &Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "orphan", Ledger: 200}
	_, err = st.CreateAlert(ctx, old)
	require.NoError(t, err)
	_, err = st.CreateAlert(ctx, orphan)
	require.NoError(t, err)

	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	n, err := st.RetractAlertsFromLedger(ctx, 150, when)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "only the alert at or after the divergence is retracted")

	got, err := st.GetAlert(ctx, old.ID)
	require.NoError(t, err)
	assert.Nil(t, got.RetractedAt)
	assert.Equal(t, uint32(100), got.Ledger)

	got, err = st.GetAlert(ctx, orphan.ID)
	require.NoError(t, err)
	require.NotNil(t, got.RetractedAt)
	assert.True(t, got.RetractedAt.Equal(when), "got %v", got.RetractedAt)

	// Re-running is idempotent: already-retracted rows are not counted again.
	n, err = st.RetractAlertsFromLedger(ctx, 150, when.Add(time.Hour))
	require.NoError(t, err)
	assert.Zero(t, n)

	list, err := st.ListAlerts(ctx, AlertFilter{MonitorID: m.ID})
	require.NoError(t, err)
	require.Len(t, list, 2)
}

// The four channel-config tests below run against both backends so the
// encryption envelope and the lazy re-encryption path cannot diverge.

func testChannelConfigNoKey(t *testing.T, newStore conformanceFactory) {
	st := newStore(t) // no cipher configured
	ctx := context.Background()

	c := &Channel{Name: "plain", Type: "webhook", Config: json.RawMessage(`{"token":"s3cr3t"}`), Enabled: true}
	require.NoError(t, st.CreateChannel(ctx, c))

	raw, err := st.rawChannelConfig(ctx, c.ID)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "s3cr3t", "no key must preserve the old plaintext behaviour")
	assert.NotContains(t, string(raw), `"sorobeacon_config"`)

	got, err := st.GetChannel(ctx, c.ID)
	require.NoError(t, err)
	assert.JSONEq(t, `{"token":"s3cr3t"}`, string(got.Config))
}

func testChannelConfigEncrypted(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	st.setCipher(testCipher(t, testConfigKey))
	ctx := context.Background()

	const secretURL = "https://hooks.example/T000/B000/s3cr3t-path"
	const secretToken = "s3cr3t-token"
	cfg := json.RawMessage(`{"url":"` + secretURL + `","token":"` + secretToken + `"}`)
	c := &Channel{Name: "ops", Type: "webhook", Config: cfg, Enabled: true}
	require.NoError(t, st.CreateChannel(ctx, c))

	// The point of the feature: the raw column must contain no plaintext
	// secret, and must be recognisably an envelope.
	raw, err := st.rawChannelConfig(ctx, c.ID)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), secretURL)
	assert.NotContains(t, string(raw), secretToken)
	assert.Contains(t, string(raw), `"sorobeacon_config"`)

	// Every read path decrypts transparently.
	got, err := st.GetChannel(ctx, c.ID)
	require.NoError(t, err)
	assert.JSONEq(t, string(cfg), string(got.Config))

	list, err := st.ListChannels(ctx, false)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.JSONEq(t, string(cfg), string(list[0].Config))

	page, err := st.ListChannelsPage(ctx, ListFilter{})
	require.NoError(t, err)
	require.Len(t, page, 1)
	assert.JSONEq(t, string(cfg), string(page[0].Config))

	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	require.NoError(t, st.SetMonitorChannels(ctx, m.ID, []int64{c.ID}))
	attached, err := st.ListChannelsForMonitor(ctx, m.ID)
	require.NoError(t, err)
	require.Len(t, attached, 1)
	assert.JSONEq(t, string(cfg), string(attached[0].Config))

	// Update re-encrypts: the rotated secret is not in the raw row either.
	c.Config = json.RawMessage(`{"url":"` + secretURL + `","token":"rotated"}`)
	require.NoError(t, st.UpdateChannel(ctx, c))
	raw, err = st.rawChannelConfig(ctx, c.ID)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "rotated")
	got, err = st.GetChannel(ctx, c.ID)
	require.NoError(t, err)
	assert.JSONEq(t, `{"url":"`+secretURL+`","token":"rotated"}`, string(got.Config))
}

func testChannelConfigLegacy(t *testing.T, newStore conformanceFactory) {
	st := newStore(t) // plaintext row written before a key existed
	ctx := context.Background()

	const secret = "legacy-s3cr3t-token"
	legacyConfig := json.RawMessage(`{"token":"` + secret + `"}`)
	c := &Channel{Name: "legacy", Type: "webhook", Config: legacyConfig, Enabled: true}
	require.NoError(t, st.CreateChannel(ctx, c))
	raw, err := st.rawChannelConfig(ctx, c.ID)
	require.NoError(t, err)
	assert.Contains(t, string(raw), secret)

	// A key is turned on for an existing deployment. The legacy row must
	// still read — an upgrade must never brick a running instance.
	st.setCipher(testCipher(t, testConfigKey))

	got, err := st.GetChannel(ctx, c.ID)
	require.NoError(t, err)
	assert.JSONEq(t, string(legacyConfig), string(got.Config))

	list, err := st.ListChannels(ctx, false)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.JSONEq(t, string(legacyConfig), string(list[0].Config))

	// It is re-encrypted lazily on the next write.
	c.Name = "legacy-renamed"
	require.NoError(t, st.UpdateChannel(ctx, c))
	raw, err = st.rawChannelConfig(ctx, c.ID)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), secret)
	assert.Contains(t, string(raw), `"sorobeacon_config"`)
}

func testChannelConfigDecryptFailure(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	st.setCipher(testCipher(t, testConfigKey))
	ctx := context.Background()

	c := &Channel{Name: "pager", Type: "webhook", Config: json.RawMessage(`{"token":"s3cr3t"}`), Enabled: true}
	require.NoError(t, st.CreateChannel(ctx, c))
	raw, err := st.rawChannelConfig(ctx, c.ID)
	require.NoError(t, err)
	rawBefore := string(raw)

	// Rotating the key without re-encrypting makes the stored row
	// undecryptable. That must be a clear error naming the channel — never
	// a panic, and never an echo of the ciphertext or key material.
	st.setCipher(testCipher(t, "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"))

	_, err = st.GetChannel(ctx, c.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pager")
	assert.Contains(t, err.Error(), "decrypt config")
	assert.NotContains(t, err.Error(), "s3cr3t")
	assert.NotContains(t, err.Error(), rawBefore)

	// Listing must report the same failure rather than returning the
	// envelope as if it were plaintext config.
	_, err = st.ListChannels(ctx, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pager")
}
