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

// TestPostgresDeliveryAttempts pins the delivery_attempts audit trail on the
// real Postgres schema: how RecordDeliveryAttempt fills in the server-generated
// fields, what ListDeliveryAttempts returns and in which order (oldest first,
// per the documented Alerts contract), and that the ON DELETE CASCADE declared
// in 0001_init (re-created as a composite FK by 0011) removes a child row when
// its parent alert is deleted. Like the conformance suite it runs against
// TEST_DATABASE_URL and skips when it is unset, so `go test ./...` works
// without a database.
func TestPostgresDeliveryAttempts(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres delivery attempt tests")
	}

	url := os.Getenv("TEST_DATABASE_URL")
	require.NoError(t, Migrate(url))
	st, err := NewPostgres(context.Background(), url, PoolSettings{})
	require.NoError(t, err)
	t.Cleanup(st.Close)
	require.NoError(t, st.resetConformance(context.Background()))
	ctx := context.Background()

	// One monitor, rule and channel to hang alerts on. The channel config is
	// empty JSON here; a test snippet must never look like a credential even
	// though it is fake, because snippets are rendered on the dashboard.
	m := &Monitor{Name: "delivery-attempts", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	r := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, r))
	c := &Channel{Name: "delivery-attempts-hook", Type: "webhook", Config: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateChannel(ctx, c))
	alert := &Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "ev-dlv-1"}
	_, err = st.CreateAlert(ctx, alert)
	require.NoError(t, err)

	// Success first: the insert succeeds with an empty response snippet (the
	// column is NOT NULL DEFAULT ''), and the store stamps the server-side id
	// and attempted_at rather than trusting the caller's values.
	success := &DeliveryAttempt{AlertID: alert.ID, ChannelID: c.ID, Status: DeliveryStatusSuccess}
	require.NoError(t, st.RecordDeliveryAttempt(ctx, success))
	assert.Positive(t, success.ID, "the insert should stamp the generated id")
	assert.WithinDuration(t, time.Now(), success.AttemptedAt, 2*time.Minute,
		"the insert should stamp attempted_at with the server clock")

	t.Run("success attempt records with an empty error snippet", func(t *testing.T) {
		stored, err := st.ListDeliveryAttempts(ctx, alert.ID, "")
		require.NoError(t, err)
		require.Len(t, stored, 1)
		assert.Equal(t, alert.ID, stored[0].AlertID)
		assert.Equal(t, c.ID, stored[0].ChannelID)
		assert.Equal(t, DeliveryStatusSuccess, stored[0].Status)
		assert.Empty(t, stored[0].ResponseSnippet,
			"a successful attempt leaves response_snippet empty; it is NOT NULL DEFAULT '' in 0001_init.up.sql")
	})

	// A failure carries a response code and a snippet. The snippet is a plain
	// prose message so the fixture cannot resemble a credential even though
	// it is fake: snippets are rendered on the dashboard.
	failure := &DeliveryAttempt{
		AlertID:         alert.ID,
		ChannelID:       c.ID,
		Status:          DeliveryStatusFailed,
		ResponseSnippet: "upstream rejected the request with response code 500",
	}
	require.NoError(t, st.RecordDeliveryAttempt(ctx, failure))
	assert.Positive(t, failure.ID)

	t.Run("failed attempt records its response code and snippet", func(t *testing.T) {
		failed, err := st.ListDeliveryAttempts(ctx, alert.ID, DeliveryStatusFailed)
		require.NoError(t, err)
		require.Len(t, failed, 1)
		assert.Equal(t, failure.ID, failed[0].ID)
		assert.Equal(t, DeliveryStatusFailed, failed[0].Status)
		assert.Equal(t, failure.ResponseSnippet, failed[0].ResponseSnippet)
		assert.Equal(t, c.ID, failed[0].ChannelID)
	})

	t.Run("list is oldest first with per-attempt ids preserved", func(t *testing.T) {
		// A third attempt makes the ordering observable; pinning the full id
		// sequence holds the store to its documented contract ("oldest first"
		// on the Alerts interface) rather than only to one end of the list.
		third := &DeliveryAttempt{AlertID: alert.ID, ChannelID: c.ID, Status: DeliveryStatusFailed, ResponseSnippet: "third attempt failed"}
		require.NoError(t, st.RecordDeliveryAttempt(ctx, third))

		list, err := st.ListDeliveryAttempts(ctx, alert.ID, "")
		require.NoError(t, err)
		require.Len(t, list, 3)
		assert.Equal(t, []int64{success.ID, failure.ID, third.ID},
			[]int64{list[0].ID, list[1].ID, list[2].ID},
			"ListDeliveryAttempts returns attempts oldest first, as documented on the Alerts interface")
	})

	t.Run("count matches the stored attempts for the alert", func(t *testing.T) {
		list, err := st.ListDeliveryAttempts(ctx, alert.ID, "")
		require.NoError(t, err)
		assert.Len(t, list, 3, "the audit trail should hold every attempt recorded for the alert")
	})

	t.Run("listing an alert with no attempts returns an empty slice", func(t *testing.T) {
		quiet := &Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "ev-dlv-quiet"}
		_, err := st.CreateAlert(ctx, quiet)
		require.NoError(t, err)

		list, err := st.ListDeliveryAttempts(ctx, quiet.ID, "")
		require.NoError(t, err)
		assert.Empty(t, list, "an alert with no deliveries has an empty audit trail")
	})

	// The cascade is a schema promise, not an interface one: 0001_init.up.sql
	// declares ON DELETE CASCADE on delivery_attempts.alert_id, and 0011
	// re-creates it as a composite FK on (alert_id, alert_created_at) when the
	// alerts table becomes partitioned. Assert against the migration as
	// written so a schema change that silently drops the cascade fails here.
	t.Run("deleting the parent alert removes its attempts", func(t *testing.T) {
		orphan := &Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "ev-dlv-orphan"}
		_, err := st.CreateAlert(ctx, orphan)
		require.NoError(t, err)
		doomed := &DeliveryAttempt{AlertID: orphan.ID, ChannelID: c.ID, Status: DeliveryStatusFailed, ResponseSnippet: "orphaned with its alert"}
		require.NoError(t, st.RecordDeliveryAttempt(ctx, doomed))
		require.Len(t, mustListDeliveryAttempts(t, st, orphan.ID), 1)

		// DeleteMonitor cascades to alerts via the monitor FK, one of the
		// production paths that removes a parent alert; it must take the
		// alert's attempts with it.
		require.NoError(t, st.DeleteMonitor(ctx, m.ID))

		list := mustListDeliveryAttempts(t, st, orphan.ID)
		assert.Empty(t, list, "deleting the parent alert must cascade to its delivery attempts")
	})
}

// mustListDeliveryAttempts is a small helper for the subtests that assert on
// the audit trail's existence rather than its contents, so an unexpected
// store error reads as a test failure instead of one further assertion down.
func mustListDeliveryAttempts(t *testing.T, st *Postgres, alertID int64) []DeliveryAttempt {
	t.Helper()
	list, err := st.ListDeliveryAttempts(context.Background(), alertID, "")
	require.NoError(t, err)
	return list
}
