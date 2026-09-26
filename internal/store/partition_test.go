package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newPartitionStore returns a migrated Postgres store, skipping without a
// database like the rest of the integration suite.
func newPartitionStore(t *testing.T) *Postgres {
	t.Helper()
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres partition tests")
	}
	p, ok := newTestPostgres(t).(*Postgres)
	require.True(t, ok)
	return p
}

// dropPartition removes a partition so a test starts from a known state,
// regardless of what an earlier run created. The partition is detached first
// because the per-partition FK constraint on delivery_attempts otherwise
// blocks the DROP.
func dropPartition(t *testing.T, p *Postgres, name string) {
	t.Helper()
	ctx := context.Background()
	names, err := p.alertPartitions(ctx)
	require.NoError(t, err)
	for _, n := range names {
		if n != name {
			continue
		}
		_, err := p.pool.Exec(ctx, `ALTER TABLE alerts DETACH PARTITION `+name)
		require.NoError(t, err)
	}
	_, err = p.pool.Exec(ctx, `DROP TABLE IF EXISTS `+name)
	require.NoError(t, err)
}

// TestEnsureAlertPartitionsCreatesMonths pins the naming and idempotency.
func TestEnsureAlertPartitionsCreatesMonths(t *testing.T) {
	p := newPartitionStore(t)
	ctx := context.Background()
	const month = "alerts_2099_03"
	dropPartition(t, p, month)
	t.Cleanup(func() { dropPartition(t, p, month) })

	from := time.Date(2099, 3, 15, 0, 0, 0, 0, time.UTC)
	require.NoError(t, p.EnsureAlertPartitions(ctx, from, 1))
	names, err := p.alertPartitions(ctx)
	require.NoError(t, err)
	assert.Contains(t, names, month)

	// Running it again is a no-op, not an error.
	require.NoError(t, p.EnsureAlertPartitions(ctx, from, 1))
}

// TestAlertLandsInItsMonthPartition is the core property: a row inserted for a
// month with a partition goes there, and one for a month without a partition
// is caught by the default partition rather than rejected.
func TestAlertLandsInItsMonthPartition(t *testing.T) {
	p := newPartitionStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, p.CreateMonitor(ctx, m))
	r := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, p.CreateRule(ctx, r))

	// A month well past the ensured window, with no partition yet.
	const future = "alerts_2099_07"
	dropPartition(t, p, future)
	t.Cleanup(func() { dropPartition(t, p, future) })

	a := &Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "ev-default"}
	_, err := p.CreateAlert(ctx, a)
	require.NoError(t, err)
	require.NoError(t, p.setAlertCreatedAt(ctx, a.ID, time.Date(2099, 7, 10, 12, 0, 0, 0, time.UTC)))

	// No partition for 2099-07, so the row sits in the default partition.
	var inDefault int
	require.NoError(t, p.pool.QueryRow(ctx,
		`SELECT count(*) FROM `+alertDefaultPartition+` WHERE id = $1`, a.ID).Scan(&inDefault))
	assert.Equal(t, 1, inDefault, "a missing partition must not lose the row")

	// Creating the partition moves it out of the default.
	require.NoError(t, p.EnsureAlertPartitions(ctx, time.Date(2099, 7, 1, 0, 0, 0, 0, time.UTC), 1))

	var inPartition int
	require.NoError(t, p.pool.QueryRow(ctx, `SELECT count(*) FROM `+future+` WHERE id = $1`, a.ID).Scan(&inPartition))
	assert.Equal(t, 1, inPartition, "the row must be moved into the new partition")

	require.NoError(t, p.pool.QueryRow(ctx,
		`SELECT count(*) FROM `+alertDefaultPartition+` WHERE id = $1`, a.ID).Scan(&inDefault))
	assert.Zero(t, inDefault, "the default partition must be empty for that range afterwards")

	// The row is still readable through the parent table.
	got, err := p.GetAlert(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, a.EventID, got.EventID)
}

// TestDropExpiredPartitionsCleansChildren proves retention's contract: the
// whole partition goes, and the delivery attempts and dedup key that referenced
// its alerts go with it (a DROP cannot fire ON DELETE CASCADE).
func TestDropExpiredPartitionsCleansChildren(t *testing.T) {
	p := newPartitionStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, p.CreateMonitor(ctx, m))
	r := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, p.CreateRule(ctx, r))
	c := &Channel{Name: "c", Type: "webhook", Config: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, p.CreateChannel(ctx, c))

	const old = "alerts_2000_01"
	dropPartition(t, p, old)
	t.Cleanup(func() { dropPartition(t, p, old) })
	require.NoError(t, p.EnsureAlertPartitions(ctx, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), 1))

	a := &Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "ev-old"}
	_, err := p.CreateAlert(ctx, a)
	require.NoError(t, err)
	require.NoError(t, p.setAlertCreatedAt(ctx, a.ID, time.Date(2000, 1, 15, 12, 0, 0, 0, time.UTC)))

	attempt := &DeliveryAttempt{AlertID: a.ID, ChannelID: c.ID, Status: DeliveryStatusSuccess}
	require.NoError(t, p.RecordDeliveryAttempt(ctx, attempt))

	dropped, err := p.DropExpiredPartitions(ctx, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, dropped, int64(1))

	names, err := p.alertPartitions(ctx)
	require.NoError(t, err)
	assert.NotContains(t, names, old, "the expired partition must be gone")

	_, err = p.GetAlert(ctx, a.ID)
	assert.ErrorIs(t, err, ErrNotFound)

	attempts, err := p.ListDeliveryAttempts(ctx, a.ID, "")
	require.NoError(t, err)
	assert.Empty(t, attempts, "delivery attempts must not outlive a dropped partition")

	var dedupLeft int
	require.NoError(t, p.pool.QueryRow(ctx,
		`SELECT count(*) FROM alert_dedup WHERE alert_id = $1`, a.ID).Scan(&dedupLeft))
	assert.Zero(t, dedupLeft, "a stranded dedup key would stop the event ever alerting again")

	// A recent partition is untouched by the same cutoff.
	recent, err := p.alertPartitions(ctx)
	require.NoError(t, err)
	assert.NotEmpty(t, recent)
}

// TestDeleteExpiredAlertsClearsDedupKey covers the ragged edge: the batched
// row delete must clear the dedup key too, and its composite FK must cascade
// the delivery attempts.
func TestDeleteExpiredAlertsClearsDedupKey(t *testing.T) {
	p := newPartitionStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, p.CreateMonitor(ctx, m))
	r := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, p.CreateRule(ctx, r))
	c := &Channel{Name: "c", Type: "webhook", Config: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, p.CreateChannel(ctx, c))

	a := &Alert{MonitorID: m.ID, RuleID: r.ID, EventID: fmt.Sprintf("ev-%d", time.Now().UnixNano())}
	_, err := p.CreateAlert(ctx, a)
	require.NoError(t, err)
	require.NoError(t, p.setAlertCreatedAt(ctx, a.ID, time.Now().Add(-72*time.Hour)))
	require.NoError(t, p.RecordDeliveryAttempt(ctx, &DeliveryAttempt{AlertID: a.ID, ChannelID: c.ID, Status: DeliveryStatusSuccess}))

	deleted, err := p.DeleteExpiredAlerts(ctx, time.Now().Add(-24*time.Hour), 100)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, deleted, int64(1))

	attempts, err := p.ListDeliveryAttempts(ctx, a.ID, "")
	require.NoError(t, err)
	assert.Empty(t, attempts, "the composite FK must cascade")

	var dedupLeft int
	require.NoError(t, p.pool.QueryRow(ctx,
		`SELECT count(*) FROM alert_dedup WHERE alert_id = $1`, a.ID).Scan(&dedupLeft))
	assert.Zero(t, dedupLeft)
}
