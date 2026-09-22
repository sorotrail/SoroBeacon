package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testStore connects to TEST_DATABASE_URL, runs migrations and cleans all
// tables. Tests are skipped when the variable is unset so `go test ./...`
// works without a database (CI runs them against the compose Postgres:
// make test-db).
func testStore(t *testing.T) *Postgres {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping store integration tests")
	}
	require.NoError(t, Migrate(url))

	st, err := NewPostgres(context.Background(), url, PoolSettings{})
	require.NoError(t, err)
	t.Cleanup(st.Close)

	_, err = st.pool.Exec(context.Background(),
		`TRUNCATE monitors, rules, channels, monitor_channels, alerts, delivery_attempts RESTART IDENTITY CASCADE;
		 UPDATE ingest_state SET last_ledger = 0, last_cursor = '' WHERE id = 1`)
	require.NoError(t, err)
	return st
}

func TestMonitorCRUD(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "m1", ContractIDs: []string{"CAAA", "CBBB"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	assert.NotZero(t, m.ID)
	assert.False(t, m.CreatedAt.IsZero())

	got, err := st.GetMonitor(ctx, m.ID)
	require.NoError(t, err)
	assert.Equal(t, "m1", got.Name)
	assert.Equal(t, []string{"CAAA", "CBBB"}, got.ContractIDs)

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

func TestSetMonitorsEnabled_AtomicUnknownIDs(t *testing.T) {
	st := testStore(t)
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

func TestMonitorsAndChannelsKeysetPagination(t *testing.T) {
	st := testStore(t)
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

func TestListMonitorsPageSearchFilterSort(t *testing.T) {
	st := testStore(t)
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

func TestRuleCRUDAndCascade(t *testing.T) {
	st := testStore(t)
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

func TestChannelsAndAttachments(t *testing.T) {
	st := testStore(t)
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

func TestAlertDedupAndListing(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	r := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, r))

	a := &Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "ev-1", Payload: json.RawMessage(`{"k":"v"}`)}
	created, err := st.CreateAlert(ctx, a)
	require.NoError(t, err)
	assert.True(t, created)
	assert.NotZero(t, a.ID)

	dup := &Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "ev-1"}
	created, err = st.CreateAlert(ctx, dup)
	require.NoError(t, err)
	assert.False(t, created, "same (rule_id, event_id) must dedup")

	b := &Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "ev-2"}
	created, err = st.CreateAlert(ctx, b)
	require.NoError(t, err)
	assert.True(t, created)

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

func TestListAlertsSearchFilterSort(t *testing.T) {
	st := testStore(t)
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
		require.True(t, ok)
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

func TestDeliveryAttempts(t *testing.T) {
	st := testStore(t)
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

	list, err := st.ListDeliveryAttempts(ctx, a.ID)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "failed", list[0].Status)
	assert.Equal(t, "boom", list[0].ResponseSnippet)
	assert.Equal(t, "success", list[1].Status)

	got, err := st.GetAlert(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, a.ID, got.ID)
	assert.Equal(t, a.EventID, got.EventID)
	_, err = st.GetAlert(ctx, a.ID+999)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestDeleteExpiredAlertsKeepsRecentAndCascadesAttempts(t *testing.T) {
	st := testStore(t)
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
	_, err = st.pool.Exec(ctx, `UPDATE alerts SET created_at = $1 WHERE id = $2`, oldCutoff.Add(-time.Hour), oldAlert.ID)
	require.NoError(t, err)

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

	oldAttempts, err := st.ListDeliveryAttempts(ctx, oldAlert.ID)
	require.NoError(t, err)
	assert.Empty(t, oldAttempts, "delivery_attempts must cascade with the alert")

	kept, err := st.ListDeliveryAttempts(ctx, recentAlert.ID)
	require.NoError(t, err)
	require.Len(t, kept, 1)
}

func TestDeleteExpiredAlertsBatches(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	r := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, r))

	for i := 0; i < 3; i++ {
		a := &Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "old-" + string(rune('a'+i))}
		_, err := st.CreateAlert(ctx, a)
		require.NoError(t, err)
		_, err = st.pool.Exec(ctx, `UPDATE alerts SET created_at = now() - interval '48 hours' WHERE id = $1`, a.ID)
		require.NoError(t, err)
	}

	deleted, err := st.DeleteExpiredAlerts(ctx, time.Now().Add(-24*time.Hour), 2)
	require.NoError(t, err)
	assert.Equal(t, int64(2), deleted)

	deleted, err = st.DeleteExpiredAlerts(ctx, time.Now().Add(-24*time.Hour), 2)
	require.NoError(t, err)
	assert.Equal(t, int64(1), deleted)
}

func TestIngestStateRoundTrip(t *testing.T) {
	st := testStore(t)
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

func TestGetStats(t *testing.T) {
	st := testStore(t)
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

func TestCopyMonitorName(t *testing.T) {
	assert.Equal(t, "m (copy)", CopyMonitorName("m", nil))
	assert.Equal(t, "m (copy)", CopyMonitorName("m", []string{"m"}))
	assert.Equal(t, "m (copy 2)", CopyMonitorName("m", []string{"m", "m (copy)"}))
	assert.Equal(t, "m (copy 3)", CopyMonitorName("m", []string{"m", "m (copy)", "m (copy 2)"}))
}

func TestDuplicateMonitor_CopiesRulesChannelsDisabledUniqueName(t *testing.T) {
	st := testStore(t)
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
