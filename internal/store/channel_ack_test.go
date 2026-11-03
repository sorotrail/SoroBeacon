package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testChannelAckReconciliation(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "mon-reconcile", ContractIDs: []string{"C1"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))

	rWarn := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true, Severity: SeverityWarning}
	require.NoError(t, st.CreateRule(ctx, rWarn))

	rCrit := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true, Severity: SeverityCritical}
	require.NoError(t, st.CreateRule(ctx, rCrit))

	rInfo := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true, Severity: SeverityInfo}
	require.NoError(t, st.CreateRule(ctx, rInfo))

	// 1. Channel with healthy acknowledgement
	chHealthy := &Channel{Name: "ch-healthy", Type: "webhook", Config: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateChannel(ctx, chHealthy))

	// 2. Channel that accepts deliveries but is never acknowledged (ack silent)
	chUnacked := &Channel{Name: "ch-unacked", Type: "slack", Config: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateChannel(ctx, chUnacked))

	// 3. Channel with only excluded deliveries (info, suppressed, inhibited, grace window)
	chExcluded := &Channel{Name: "ch-excluded", Type: "email", Config: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateChannel(ctx, chExcluded))

	now := time.Now().UTC()
	pastTime := now.Add(-6 * time.Hour)

	// Populate chHealthy: 3 warning alerts, backdated, delivered, all acknowledged
	for i := 1; i <= 3; i++ {
		a := &Alert{
			MonitorID: m.ID,
			RuleID:    rWarn.ID,
			EventID:   "evt-healthy-" + string(rune('0'+i)),
			Severity:  SeverityWarning,
		}
		_, err := st.CreateAlert(ctx, a)
		require.NoError(t, err)
		require.NoError(t, st.setAlertCreatedAt(ctx, a.ID, pastTime))
		require.NoError(t, st.RecordDeliveryAttempt(ctx, &DeliveryAttempt{
			AlertID:   a.ID,
			ChannelID: chHealthy.ID,
			Status:    DeliveryStatusSuccess,
		}))
		require.NoError(t, st.AcknowledgeAlert(ctx, a.ID, "operator-alice"))
	}

	// Populate chUnacked: 3 warning alerts, backdated, delivered, NONE acknowledged
	for i := 1; i <= 3; i++ {
		a := &Alert{
			MonitorID: m.ID,
			RuleID:    rWarn.ID,
			EventID:   "evt-unacked-" + string(rune('0'+i)),
			Severity:  SeverityWarning,
		}
		_, err := st.CreateAlert(ctx, a)
		require.NoError(t, err)
		require.NoError(t, st.setAlertCreatedAt(ctx, a.ID, pastTime))
		require.NoError(t, st.RecordDeliveryAttempt(ctx, &DeliveryAttempt{
			AlertID:   a.ID,
			ChannelID: chUnacked.ID,
			Status:    DeliveryStatusSuccess,
		}))
	}

	// Populate chExcluded:
	// a) info alert (should be excluded)
	aInfo := &Alert{
		MonitorID: m.ID,
		RuleID:    rInfo.ID,
		EventID:   "evt-excluded-info",
		Severity:  SeverityInfo,
	}
	_, err := st.CreateAlert(ctx, aInfo)
	require.NoError(t, err)
	require.NoError(t, st.setAlertCreatedAt(ctx, aInfo.ID, pastTime))
	require.NoError(t, st.RecordDeliveryAttempt(ctx, &DeliveryAttempt{
		AlertID:   aInfo.ID,
		ChannelID: chExcluded.ID,
		Status:    DeliveryStatusSuccess,
	}))

	// b) suppressed alert (maintenance window) (should be excluded)
	aSuppressed := &Alert{
		MonitorID: m.ID,
		RuleID:    rWarn.ID,
		EventID:   "evt-excluded-suppressed",
	}
	_, err = st.CreateAlert(ctx, aSuppressed)
	require.NoError(t, err)
	require.NoError(t, st.SetAlertSuppressed(ctx, aSuppressed.ID, "planned maintenance"))
	require.NoError(t, st.setAlertCreatedAt(ctx, aSuppressed.ID, pastTime))
	require.NoError(t, st.RecordDeliveryAttempt(ctx, &DeliveryAttempt{
		AlertID:   aSuppressed.ID,
		ChannelID: chExcluded.ID,
		Status:    DeliveryStatusSuccess,
	}))

	// c) inhibited alert (should be excluded)
	aInhibited := &Alert{
		MonitorID: m.ID,
		RuleID:    rWarn.ID,
		EventID:   "evt-excluded-inhibited",
	}
	_, err = st.CreateAlert(ctx, aInhibited)
	require.NoError(t, err)
	require.NoError(t, st.MarkAlertInhibited(ctx, aInhibited.ID, rCrit.ID))
	require.NoError(t, st.setAlertCreatedAt(ctx, aInhibited.ID, pastTime))
	require.NoError(t, st.RecordDeliveryAttempt(ctx, &DeliveryAttempt{
		AlertID:   aInhibited.ID,
		ChannelID: chExcluded.ID,
		Status:    DeliveryStatusSuccess,
	}))

	// d) critical alert created 5 minutes ago (< 1h critical grace window) (should not be counted as unack yet)
	aRecentCrit := &Alert{
		MonitorID: m.ID,
		RuleID:    rCrit.ID,
		EventID:   "evt-excluded-recent-crit",
		Severity:  SeverityCritical,
	}
	_, err = st.CreateAlert(ctx, aRecentCrit)
	require.NoError(t, err)
	require.NoError(t, st.setAlertCreatedAt(ctx, aRecentCrit.ID, now.Add(-5*time.Minute)))
	require.NoError(t, st.RecordDeliveryAttempt(ctx, &DeliveryAttempt{
		AlertID:   aRecentCrit.ID,
		ChannelID: chExcluded.ID,
		Status:    DeliveryStatusSuccess,
	}))

	// Run reconciliation
	cfg := DefaultAckReconciliationConfig()
	results, err := st.ReconcileChannelAcknowledgements(ctx, cfg)
	require.NoError(t, err)
	require.NotEmpty(t, results)

	byID := make(map[int64]ChannelAckReconciliation)
	for _, r := range results {
		byID[r.ChannelID] = r
	}

	// 1. Assert healthy channel
	healthyRec, ok := byID[chHealthy.ID]
	require.True(t, ok, "chHealthy must be in reconciliation results")
	assert.Equal(t, int64(3), healthyRec.DeliveredTotal)
	assert.Equal(t, int64(3), healthyRec.AcknowledgedTotal)
	assert.Equal(t, int64(0), healthyRec.UnacknowledgedTotal)
	require.NotNil(t, healthyRec.UnackRate)
	assert.Equal(t, 0.0, *healthyRec.UnackRate)
	assert.False(t, healthyRec.Flagged, "healthy channel must not be flagged")

	// 2. Assert unacknowledged channel (flagged!)
	unackRec, ok := byID[chUnacked.ID]
	require.True(t, ok, "chUnacked must be in reconciliation results")
	assert.Equal(t, int64(3), unackRec.DeliveredTotal)
	assert.Equal(t, int64(0), unackRec.AcknowledgedTotal)
	assert.Equal(t, int64(3), unackRec.UnacknowledgedTotal)
	require.NotNil(t, unackRec.UnackRate)
	assert.Equal(t, 1.0, *unackRec.UnackRate)
	assert.True(t, unackRec.Flagged, "channel with 100% unacknowledged deliveries must be flagged")

	// 3. Assert excluded channel (0 qualifying deliveries)
	excludedRec, ok := byID[chExcluded.ID]
	require.True(t, ok, "chExcluded must be in reconciliation results")
	assert.Equal(t, int64(0), excludedRec.DeliveredTotal)
	assert.Equal(t, int64(0), excludedRec.AcknowledgedTotal)
	assert.Equal(t, int64(0), excludedRec.UnacknowledgedTotal)
	assert.Nil(t, excludedRec.UnackRate)
	assert.False(t, excludedRec.Flagged)

	// 4. Test GetChannelAckReconciliation and ListChannelAckReconciliations
	singleUnack, err := st.GetChannelAckReconciliation(ctx, chUnacked.ID)
	require.NoError(t, err)
	assert.True(t, singleUnack.Flagged)
	assert.Equal(t, int64(3), singleUnack.DeliveredTotal)

	listRecs, err := st.ListChannelAckReconciliations(ctx)
	require.NoError(t, err)
	assert.True(t, listRecs[chUnacked.ID].Flagged)
	assert.False(t, listRecs[chHealthy.ID].Flagged)

	// 5. Test aggregation surviving a retention sweep
	// DeleteExpiredAlerts deletes alerts older than 2 hours ago
	deletedCount, err := st.DeleteExpiredAlerts(ctx, now.Add(-2*time.Hour), 1000)
	require.NoError(t, err)
	assert.Greater(t, deletedCount, int64(0), "older alerts must be pruned by retention sweep")

	// Reconciled summary in channel_ack_reconciliations must still be intact!
	survivedUnack, err := st.GetChannelAckReconciliation(ctx, chUnacked.ID)
	require.NoError(t, err)
	assert.True(t, survivedUnack.Flagged, "reconciliation state must survive retention sweep")
	assert.Equal(t, int64(3), survivedUnack.DeliveredTotal)
	assert.Equal(t, int64(3), survivedUnack.UnacknowledgedTotal)

	survivedHealthy, err := st.GetChannelAckReconciliation(ctx, chHealthy.ID)
	require.NoError(t, err)
	assert.False(t, survivedHealthy.Flagged)
	assert.Equal(t, int64(3), survivedHealthy.DeliveredTotal)
}
