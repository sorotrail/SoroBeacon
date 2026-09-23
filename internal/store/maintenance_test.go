package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMaintenanceWindowCRUD(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	start := time.Date(2026, 9, 23, 22, 0, 0, 0, time.UTC)

	w := &MaintenanceWindow{Reason: "planned upgrade", Scope: MaintenanceScopeGlobal, StartAt: start, EndAt: start.Add(4 * time.Hour)}
	require.NoError(t, st.CreateMaintenanceWindow(ctx, w))
	assert.NotZero(t, w.ID)
	assert.False(t, w.CreatedAt.IsZero())

	got, err := st.GetMaintenanceWindow(ctx, w.ID)
	require.NoError(t, err)
	assert.Equal(t, "planned upgrade", got.Reason)
	assert.Equal(t, MaintenanceScopeGlobal, got.Scope)
	assert.Nil(t, got.MonitorID)
	assert.Nil(t, got.ContractID)
	assert.True(t, got.StartAt.Equal(start))

	w.Reason = "renamed"
	require.NoError(t, st.UpdateMaintenanceWindow(ctx, w))
	got, err = st.GetMaintenanceWindow(ctx, w.ID)
	require.NoError(t, err)
	assert.Equal(t, "renamed", got.Reason)

	list, err := st.ListMaintenanceWindows(ctx, MaintenanceWindowFilter{})
	require.NoError(t, err)
	require.Len(t, list, 1)

	require.NoError(t, st.DeleteMaintenanceWindow(ctx, w.ID))
	_, err = st.GetMaintenanceWindow(ctx, w.ID)
	assert.ErrorIs(t, err, ErrNotFound)
	assert.ErrorIs(t, st.DeleteMaintenanceWindow(ctx, w.ID), ErrNotFound)
}

func TestMaintenanceWindowOpenEndedRejected(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	start := time.Date(2026, 9, 23, 22, 0, 0, 0, time.UTC)

	// end == start violates the CHECK: a window must be bounded.
	err := st.CreateMaintenanceWindow(ctx, &MaintenanceWindow{
		Reason: "forever", Scope: MaintenanceScopeGlobal, StartAt: start, EndAt: start,
	})
	assert.Error(t, err)
}

func TestActiveMaintenanceWindowResolution(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	m1 := &Monitor{Name: "m1", ContractIDs: []string{"CAAA"}, Enabled: true}
	m2 := &Monitor{Name: "m2", ContractIDs: []string{"CAAA"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m1))
	require.NoError(t, st.CreateMonitor(ctx, m2))

	at := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	win := func(reason, scope string, monitorID *int64, contractID *string) {
		require.NoError(t, st.CreateMaintenanceWindow(ctx, &MaintenanceWindow{
			Reason: reason, Scope: scope, MonitorID: monitorID, ContractID: contractID,
			StartAt: at.Add(-time.Hour), EndAt: at.Add(time.Hour),
		}))
	}

	// Before any window exists, nothing is suppressed.
	got, err := st.ActiveMaintenanceWindow(ctx, m1.ID, "CBBB", at)
	require.NoError(t, err)
	assert.Nil(t, got)

	win("global", MaintenanceScopeGlobal, nil, nil)
	m1ID := m1.ID
	win("m1-only", MaintenanceScopeMonitor, &m1ID, nil)
	contract := "CBBB"
	win("contract-only", MaintenanceScopeContract, nil, &contract)

	cases := []struct {
		name       string
		monitorID  int64
		contractID string
		want       string
	}{
		{"contract is the most specific", m1.ID, "CBBB", "contract-only"},
		{"monitor covers its other contracts", m1.ID, "COTHER", "m1-only"},
		{"global covers other monitors", m2.ID, "COTHER", "global"},
		{"global covers no contract match", m2.ID, "", "global"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := st.ActiveMaintenanceWindow(ctx, tc.monitorID, tc.contractID, at)
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tc.want, got.Reason)
		})
	}

	// Well outside every window.
	got, err = st.ActiveMaintenanceWindow(ctx, m1.ID, "CBBB", at.Add(48*time.Hour))
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestActiveMaintenanceWindowBoundaries(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	start := time.Date(2026, 9, 23, 22, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	require.NoError(t, st.CreateMaintenanceWindow(ctx, &MaintenanceWindow{
		Reason: "boundary", Scope: MaintenanceScopeGlobal, StartAt: start, EndAt: end,
	}))

	active := func(at time.Time) *MaintenanceWindow {
		t.Helper()
		got, err := st.ActiveMaintenanceWindow(ctx, 1, "CAAA", at)
		require.NoError(t, err)
		return got
	}

	assert.Nil(t, active(start.Add(-time.Second)), "before start is not active")
	assert.NotNil(t, active(start), "start is inclusive")
	assert.NotNil(t, active(end.Add(-time.Second)), "inside is active")
	assert.Nil(t, active(end), "end is exclusive")
}

func TestSetAlertSuppressed(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "m", ContractIDs: []string{"CAAA"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	r := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{"event_name":"transfer"}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, r))
	a := &Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "e1", Payload: json.RawMessage(`{}`)}
	created, err := st.CreateAlert(ctx, a)
	require.NoError(t, err)
	require.True(t, created)

	require.NoError(t, st.SetAlertSuppressed(ctx, a.ID, "planned upgrade"))

	got, err := st.GetAlert(ctx, a.ID)
	require.NoError(t, err)
	assert.True(t, got.Suppressed)
	assert.Equal(t, "planned upgrade", got.SuppressionReason)

	list, err := st.ListAlerts(ctx, AlertFilter{})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.True(t, list[0].Suppressed)

	assert.ErrorIs(t, st.SetAlertSuppressed(ctx, 99999, "x"), ErrNotFound)
}
