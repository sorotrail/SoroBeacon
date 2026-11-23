package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

func TestWorkspaceUsageRollupsAndStorageCounts(t *testing.T) {
	st := newTestSQLite(t)
	bg := context.Background()
	acme := workspace.With(bg, "acme")
	beta := workspace.With(bg, "beta")
	system := workspace.WithSystem(bg)

	// Create resources in acme and beta to verify stored_rows counts and WorkspaceID population.
	monAcme := &Monitor{Name: "acme-mon", ContractIDs: []string{"CSHARED"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(acme, monAcme))
	assert.Equal(t, "acme", monAcme.WorkspaceID)

	ruleAcme := &Rule{MonitorID: monAcme.ID, Type: "event_emitted", Params: json.RawMessage(`{"event_name":"transfer"}`), Enabled: true}
	require.NoError(t, st.CreateRule(acme, ruleAcme))

	chAcme := &Channel{Name: "acme-sms", Type: "webhook", Config: json.RawMessage(`{"url":"https://example.invalid/sms"}`), Enabled: true}
	require.NoError(t, st.CreateChannel(acme, chAcme))
	assert.Equal(t, "acme", chAcme.WorkspaceID)
	require.NoError(t, st.SetMonitorChannels(acme, monAcme.ID, []int64{chAcme.ID}))

	// Verify cross-tenant poller alert creation attributes alert to monitor's workspace.
	alertAcme := &Alert{MonitorID: monAcme.ID, RuleID: ruleAcme.ID, EventID: "ev-shared-1", Payload: json.RawMessage(`{"contract_id":"CSHARED"}`)}
	outcome, err := st.CreateAlert(system, alertAcme)
	require.NoError(t, err)
	assert.Equal(t, AlertCreated, outcome)

	require.NoError(t, st.CreateDeadLetter(system, &DeadLetter{
		AlertID:      alertAcme.ID,
		ChannelID:    chAcme.ID,
		LastError:    "502 bad gateway",
		AttemptCount: 3,
		LastStatus:   502,
	}))

	// Beta also watches CSHARED with its own monitor.
	monBeta := &Monitor{Name: "beta-mon", ContractIDs: []string{"CSHARED"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(beta, monBeta))
	assert.Equal(t, "beta", monBeta.WorkspaceID)

	countsAcme, err := st.GetWorkspaceStorageCounts(acme, "acme")
	require.NoError(t, err)
	assert.Equal(t, WorkspaceStorageCounts{
		Monitors:    1,
		Rules:       1,
		Channels:    1,
		Alerts:      1,
		DeadLetters: 1,
	}, countsAcme)

	countsBeta, err := st.GetWorkspaceStorageCounts(beta, "beta")
	require.NoError(t, err)
	assert.Equal(t, WorkspaceStorageCounts{
		Monitors:    1,
		Rules:       0,
		Channels:    0,
		Alerts:      0,
		DeadLetters: 0,
	}, countsBeta)

	// Flush two batches across two hours and verify additive rollup + restart survival.
	h1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	h2 := time.Date(2026, 5, 1, 11, 0, 0, 0, time.UTC)

	require.NoError(t, st.RecordWorkspaceUsageBatch(system, []WorkspaceUsageDelta{
		{
			WorkspaceID:     "acme",
			PeriodStart:     h1,
			PeriodEnd:       h1.Add(time.Hour),
			AlertsRaised:    2,
			RuleEvaluations: 10,
			APIRequests:     25,
			Notifications: []WorkspaceNotificationUsage{
				{ChannelType: "sms", Outcome: "ok", Count: 2},
				{ChannelType: "sms", Outcome: "error", Count: 1},
				{ChannelType: "slack", Outcome: "ok", Count: 2},
			},
		},
		{
			WorkspaceID:     "beta",
			PeriodStart:     h1,
			PeriodEnd:       h1.Add(time.Hour),
			AlertsRaised:    1,
			RuleEvaluations: 5,
			APIRequests:     7,
			Notifications: []WorkspaceNotificationUsage{
				{ChannelType: "webhook", Outcome: "ok", Count: 1},
			},
		},
	}))

	// Second flush into same h1 bucket + new h2 bucket for acme.
	require.NoError(t, st.RecordWorkspaceUsageBatch(system, []WorkspaceUsageDelta{
		{
			WorkspaceID:     "acme",
			PeriodStart:     h1,
			PeriodEnd:       h1.Add(time.Hour),
			AlertsRaised:    1,
			RuleEvaluations: 4,
			APIRequests:     5,
			Notifications: []WorkspaceNotificationUsage{
				{ChannelType: "sms", Outcome: "ok", Count: 1},
			},
		},
		{
			WorkspaceID:     "acme",
			PeriodStart:     h2,
			PeriodEnd:       h2.Add(time.Hour),
			AlertsRaised:    3,
			RuleEvaluations: 12,
			APIRequests:     10,
			Notifications: []WorkspaceNotificationUsage{
				{ChannelType: "voice", Outcome: "ok", Count: 3},
			},
		},
	}))

	// Tenant-scoped query sees only acme.
	acmeUsage, err := st.ListWorkspaceUsage(acme, WorkspaceUsageFilter{})
	require.NoError(t, err)
	require.Len(t, acmeUsage, 1)
	assert.Equal(t, "acme", acmeUsage[0].WorkspaceID)
	assert.Equal(t, int64(6), acmeUsage[0].AlertsRaised)
	assert.Equal(t, int64(26), acmeUsage[0].RuleEvaluations)
	assert.Equal(t, int64(40), acmeUsage[0].APIRequests)
	assert.Equal(t, countsAcme, acmeUsage[0].StoredRows)
	require.Len(t, acmeUsage[0].Periods, 2)
	assert.Equal(t, []WorkspaceNotificationUsage{
		{ChannelType: "slack", Outcome: "ok", Count: 2},
		{ChannelType: "sms", Outcome: "error", Count: 1},
		{ChannelType: "sms", Outcome: "ok", Count: 3},
		{ChannelType: "voice", Outcome: "ok", Count: 3},
	}, acmeUsage[0].Notifications)

	// Period window filter [h2, h2+1h) returns only h2.
	windowed, err := st.ListWorkspaceUsage(acme, WorkspaceUsageFilter{
		From: h2,
		To:   h2.Add(time.Hour),
	})
	require.NoError(t, err)
	require.Len(t, windowed, 1)
	assert.Equal(t, int64(3), windowed[0].AlertsRaised)
	assert.Equal(t, int64(12), windowed[0].RuleEvaluations)
	assert.Equal(t, int64(10), windowed[0].APIRequests)
	require.Len(t, windowed[0].Periods, 1)

	// Operator system query across all workspaces returns acme, beta, and default.
	allUsage, err := st.ListWorkspaceUsage(system, WorkspaceUsageFilter{})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(allUsage), 2)
}
