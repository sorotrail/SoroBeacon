package notify

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

func TestMonitorBudget_CapAndSummaryNotification(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{mockChannel(1)}}
	n := &mockNotifier{}
	d := newTestDispatcher(t, st, n)

	// Configure cap of 3 alerts per 10m window, 50% hysteresis
	d.WithMonitorCap(MonitorCapConfig{
		MaxAlerts:         3,
		Window:            10 * time.Minute,
		HysteresisPercent: 50,
	})

	ctx := context.Background()

	// 1st alert: allowed
	a1 := Alert{ID: 101, MonitorID: 1, MonitorName: "dex-monitor", Severity: "warning"}
	d.Dispatch(ctx, a1)
	assert.Equal(t, 1, n.calls, "first alert must be delivered")
	assert.Empty(t, n.lastSent.Digest)

	// 2nd alert: allowed
	a2 := Alert{ID: 102, MonitorID: 1, MonitorName: "dex-monitor", Severity: "warning"}
	d.Dispatch(ctx, a2)
	assert.Equal(t, 2, n.calls, "second alert must be delivered")
	assert.Empty(t, n.lastSent.Digest)

	// 3rd alert: reaches the cap -> produces ONE summary notification!
	a3 := Alert{ID: 103, MonitorID: 1, MonitorName: "dex-monitor", Severity: "warning"}
	d.Dispatch(ctx, a3)
	assert.Equal(t, 3, n.calls, "third alert must be delivered as summary")
	assert.Contains(t, n.lastSent.Digest, "reached notification volume cap")
	assert.Contains(t, n.lastSent.Digest, "Subsequent notifications are throttled")

	// 4th alert: capped -> suppressed, NOT delivered, marked in store!
	a4 := Alert{ID: 104, MonitorID: 1, MonitorName: "dex-monitor", Severity: "warning"}
	d.Dispatch(ctx, a4)
	assert.Equal(t, 3, n.calls, "fourth alert must be suppressed and not delivered")
	assert.Len(t, st.suppressed, 1, "suppressed alert must be marked in store")
	assert.Contains(t, st.suppressed[0], "volume cap reached")

	// 5th alert: still capped -> suppressed
	a5 := Alert{ID: 105, MonitorID: 1, MonitorName: "dex-monitor", Severity: "warning"}
	d.Dispatch(ctx, a5)
	assert.Equal(t, 3, n.calls, "fifth alert must be suppressed")
	assert.Len(t, st.suppressed, 2)
}

func TestMonitorBudget_RecoveryWithHysteresis(t *testing.T) {
	tracker := NewMonitorBudgetTracker(MonitorCapConfig{
		MaxAlerts:         4,
		Window:            10 * time.Minute,
		HysteresisPercent: 50, // Recovery threshold is <= 2 alerts
	})

	ctx := context.Background()
	now := time.Now()

	a := Alert{MonitorID: 10, MonitorName: "test-monitor"}

	// 1, 2, 3: Allow
	assert.Equal(t, CapOutcomeAllow, tracker.RecordAlert(ctx, a, now).Outcome)
	assert.Equal(t, CapOutcomeAllow, tracker.RecordAlert(ctx, a, now.Add(1*time.Minute)).Outcome)
	assert.Equal(t, CapOutcomeAllow, tracker.RecordAlert(ctx, a, now.Add(2*time.Minute)).Outcome)

	// 4: Reaches cap -> Summary
	res4 := tracker.RecordAlert(ctx, a, now.Add(3*time.Minute))
	assert.Equal(t, CapOutcomeSummary, res4.Outcome)
	assert.True(t, tracker.IsThrottled("", 10))

	// 5: Immediate next alert within window -> Suppressed
	res5 := tracker.RecordAlert(ctx, a, now.Add(4*time.Minute))
	assert.Equal(t, CapOutcomeSuppressed, res5.Outcome)

	// Advance time past the older alerts (now + 13m).
	// Timestamps remaining within 10m window: only 4m (1 alert).
	// Because 1 <= 50% of 4 (threshold is 2), hysteresis allows recovery!
	res6 := tracker.RecordAlert(ctx, a, now.Add(13*time.Minute))
	assert.Equal(t, CapOutcomeAllow, res6.Outcome, "after volume falls to hysteresis threshold, alerts must recover")
	assert.False(t, tracker.IsThrottled("", 10))
}

func TestMonitorBudget_SeverityAllowance(t *testing.T) {
	tracker := NewMonitorBudgetTracker(MonitorCapConfig{
		MaxAlerts:         2,
		Window:            10 * time.Minute,
		HysteresisPercent: 50,
		CriticalAllowance: 10,
	})

	ctx := context.Background()
	now := time.Now()

	warningAlert := Alert{MonitorID: 5, MonitorName: "finance", Severity: "warning"}
	criticalAlert := Alert{MonitorID: 5, MonitorName: "finance", Severity: "critical"}

	// Fire 2 warnings -> reaches cap
	tracker.RecordAlert(ctx, warningAlert, now)
	sum := tracker.RecordAlert(ctx, warningAlert, now.Add(1*time.Second))
	assert.Equal(t, CapOutcomeSummary, sum.Outcome)

	// 3rd warning is throttled
	throttledWarning := tracker.RecordAlert(ctx, warningAlert, now.Add(2*time.Second))
	assert.Equal(t, CapOutcomeSuppressed, throttledWarning.Outcome)

	// Critical alert arrives during the flood -> MUST BE ALLOWED!
	critRes := tracker.RecordAlert(ctx, criticalAlert, now.Add(3*time.Second))
	assert.Equal(t, CapOutcomeAllow, critRes.Outcome, "critical alert must not be silenced by informational cap")
}

func TestMonitorBudget_PerWorkspaceIsolation(t *testing.T) {
	tracker := NewMonitorBudgetTracker(MonitorCapConfig{
		MaxAlerts: 2,
		Window:    10 * time.Minute,
	})

	wsA := workspace.With(context.Background(), workspace.ID("alpha"))
	wsB := workspace.With(context.Background(), workspace.ID("beta"))

	now := time.Now()
	alert := Alert{MonitorID: 100, MonitorName: "shared-monitor-id"}

	// In Workspace Alpha: use up budget
	tracker.RecordAlert(wsA, alert, now)
	sumAlpha := tracker.RecordAlert(wsA, alert, now.Add(1*time.Second))
	assert.Equal(t, CapOutcomeSummary, sumAlpha.Outcome)
	suppAlpha := tracker.RecordAlert(wsA, alert, now.Add(2*time.Second))
	assert.Equal(t, CapOutcomeSuppressed, suppAlpha.Outcome)

	// In Workspace Beta: same monitor ID must be completely unaffected!
	allowBeta1 := tracker.RecordAlert(wsB, alert, now.Add(3*time.Second))
	assert.Equal(t, CapOutcomeAllow, allowBeta1.Outcome, "Workspace Beta must have its own independent capacity")
}

type testEscalationBudgetStore struct {
	*fakeDispatchStore
	policy    *store.EscalationPolicy
	scheduled []int64
}

func (s *testEscalationBudgetStore) GetEscalationPolicyForMonitor(context.Context, int64) (*store.EscalationPolicy, error) {
	if s.policy == nil {
		return nil, store.ErrNotFound
	}
	return s.policy, nil
}

func (s *testEscalationBudgetStore) ScheduleEscalation(_ context.Context, alertID, _ int64, _ json.RawMessage, _ int, _ time.Time) error {
	s.scheduled = append(s.scheduled, alertID)
	return nil
}

func TestMonitorBudget_EscalationInteraction(t *testing.T) {
	mockStore := &fakeDispatchStore{
		channels: []store.Channel{mockChannel(1)},
	}
	stateStore := &testEscalationBudgetStore{
		fakeDispatchStore: mockStore,
		policy: &store.EscalationPolicy{
			ID:        1,
			MonitorID: 1,
			Steps: []store.EscalationStep{
				{Position: 0, ChannelIDs: []int64{1}, DelaySeconds: 0},
				{Position: 1, ChannelIDs: []int64{1}, DelaySeconds: 300},
			},
		},
	}

	n := &mockNotifier{}
	f := scriptedFactory(n)
	d := NewDispatcher(stateStore, f, slog.New(slog.DiscardHandler))
	d.BaseBackoff = time.Millisecond

	d.WithMonitorCap(MonitorCapConfig{
		MaxAlerts: 2,
		Window:    10 * time.Minute,
	})

	ctx := context.Background()

	// First alert: starts escalation step 1
	a1 := Alert{ID: 201, MonitorID: 1, MonitorName: "vault"}
	d.Dispatch(ctx, a1)
	assert.Equal(t, 1, n.calls)
	assert.Equal(t, 1, len(stateStore.scheduled), "first alert schedules escalation")

	// Second alert: reaches cap -> summary notification delivered to escalation
	a2 := Alert{ID: 202, MonitorID: 1, MonitorName: "vault"}
	d.Dispatch(ctx, a2)
	assert.Equal(t, 2, n.calls)
	assert.Contains(t, n.lastSent.Digest, "reached notification volume cap")

	// Third alert: suppressed -> does NOT deliver or spam escalation
	a3 := Alert{ID: 203, MonitorID: 1, MonitorName: "vault"}
	d.Dispatch(ctx, a3)
	assert.Equal(t, 2, n.calls, "suppressed alert must not deliver to escalation")
	assert.Len(t, mockStore.suppressed, 1, "alert marked suppressed in store")
}

func TestMonitorBudget_PerMonitorOverride(t *testing.T) {
	tracker := NewMonitorBudgetTracker(MonitorCapConfig{
		MaxAlerts: 5,
		Window:    10 * time.Minute,
	})

	// Override monitor 42 to cap at 2 alerts
	tracker.SetOverride(workspace.Default.String(), 42, MonitorCapConfig{
		MaxAlerts: 2,
		Window:    10 * time.Minute,
	})

	ctx := context.Background()
	now := time.Now()

	aDefault := Alert{MonitorID: 99}
	aOverridden := Alert{MonitorID: 42}

	// Monitor 99 uses default (5)
	for i := 0; i < 4; i++ {
		assert.Equal(t, CapOutcomeAllow, tracker.RecordAlert(ctx, aDefault, now.Add(time.Duration(i)*time.Second)).Outcome)
	}

	// Monitor 42 hits cap at 2!
	assert.Equal(t, CapOutcomeAllow, tracker.RecordAlert(ctx, aOverridden, now).Outcome)
	assert.Equal(t, CapOutcomeSummary, tracker.RecordAlert(ctx, aOverridden, now.Add(1*time.Second)).Outcome)
	assert.Equal(t, CapOutcomeSuppressed, tracker.RecordAlert(ctx, aOverridden, now.Add(2*time.Second)).Outcome)
}
