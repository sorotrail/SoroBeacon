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

	st, err := NewPostgres(context.Background(), url)
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
