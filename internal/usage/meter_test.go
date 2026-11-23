package usage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

type failingUsageStore struct {
	store.Store
	failNext bool
}

func (f *failingUsageStore) RecordWorkspaceUsageBatch(ctx context.Context, deltas []store.WorkspaceUsageDelta) error {
	if f.failNext {
		f.failNext = false
		return errors.New("simulated transient db error")
	}
	return f.Store.RecordWorkspaceUsageBatch(ctx, deltas)
}

func TestMeter_SurvivesRestartAndAggregatesAcrossFlushes(t *testing.T) {
	ctx := context.Background()
	dbURL := "sqlite://" + filepath.Join(t.TempDir(), "usage-restart.db")
	require.NoError(t, store.Migrate(dbURL))

	st1, err := store.NewSQLite(ctx, dbURL)
	require.NoError(t, err)

	fixedTime := time.Date(2026, 6, 15, 14, 22, 0, 0, time.UTC)
	met1 := metrics.New()
	meter1 := NewMeter(st1, met1, nil).WithNow(func() time.Time { return fixedTime })

	// Process 1 records usage in memory (no DB write yet).
	meter1.RecordAlertRaised("acme", 3)
	meter1.RecordRuleEvaluations("acme", 15)
	meter1.RecordNotification("acme", "sms", "ok", 2)
	meter1.RecordNotification("acme", "sms", "error", 1)
	meter1.RecordNotification("acme", "voice", "ok", 1)
	meter1.RecordAPIRequest("acme", 42)

	// Flush before shutdown and close process 1 store.
	require.NoError(t, meter1.Flush(ctx))
	st1.Close()

	// Process 2 opens the same SQLite database after a restart and records more usage.
	st2, err := store.NewSQLite(ctx, dbURL)
	require.NoError(t, err)
	defer st2.Close()

	wrapper := &failingUsageStore{Store: st2, failNext: true}
	met2 := metrics.New()
	meter2 := NewMeter(wrapper, met2, nil).WithNow(func() time.Time { return fixedTime.Add(10 * time.Minute) })

	meter2.RecordAlertRaised("acme", 2)
	meter2.RecordRuleEvaluations("acme", 5)
	meter2.RecordNotification("acme", "sms", "ok", 3)
	meter2.RecordAPIRequest("acme", 8)

	// First flush fails transiently; usage must stay buffered in memory.
	require.Error(t, meter2.Flush(ctx))
	// Second flush succeeds and adds onto Process 1's durable rollup.
	require.NoError(t, meter2.Flush(ctx))

	summaries, err := st2.ListWorkspaceUsage(workspace.With(ctx, "acme"), store.WorkspaceUsageFilter{})
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	s := summaries[0]
	assert.Equal(t, "acme", s.WorkspaceID)
	assert.Equal(t, int64(5), s.AlertsRaised)
	assert.Equal(t, int64(20), s.RuleEvaluations)
	assert.Equal(t, int64(50), s.APIRequests)
	assert.Equal(t, []store.WorkspaceNotificationUsage{
		{ChannelType: "sms", Outcome: "error", Count: 1},
		{ChannelType: "sms", Outcome: "ok", Count: 5},
		{ChannelType: "voice", Outcome: "ok", Count: 1},
	}, s.Notifications)
	require.Len(t, s.Periods, 1)
	assert.Equal(t, time.Date(2026, 6, 15, 14, 0, 0, 0, time.UTC), s.Periods[0].PeriodStart)
}
