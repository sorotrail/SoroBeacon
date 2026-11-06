package store

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

func TestAlertStatisticsPrecomputedRollupsAndStateTransitions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st := newTestSQLite(t).(*SQLite)

	m := &Monitor{
		Name:        "Core Vault",
		ContractIDs: []string{"CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"},
		Enabled:     true,
	}
	require.NoError(t, st.CreateMonitor(ctx, m))

	r1 := &Rule{MonitorID: m.ID, Type: "event_match", Params: json.RawMessage(`{"topic":"transfer"}`), Severity: SeverityCritical, Enabled: true}
	require.NoError(t, st.CreateRule(ctx, r1))

	r2 := &Rule{MonitorID: m.ID, Type: "event_match", Params: json.RawMessage(`{"topic":"mint"}`), Severity: SeverityWarning, Enabled: true}
	require.NoError(t, st.CreateRule(ctx, r2))

	// 1. Insert 4 alerts across rules and severities.
	a1 := &Alert{MonitorID: m.ID, RuleID: r1.ID, EventID: "ev-1", Severity: SeverityCritical, Ledger: 100, Payload: json.RawMessage(`{}`)}
	a2 := &Alert{MonitorID: m.ID, RuleID: r1.ID, EventID: "ev-2", Severity: SeverityCritical, Ledger: 101, Payload: json.RawMessage(`{}`)}
	a3 := &Alert{MonitorID: m.ID, RuleID: r2.ID, EventID: "ev-3", Severity: SeverityWarning, Ledger: 102, Payload: json.RawMessage(`{}`)}
	a4 := &Alert{MonitorID: m.ID, RuleID: r2.ID, EventID: "ev-4", Severity: SeverityWarning, Ledger: 250, Payload: json.RawMessage(`{}`)}

	for _, a := range []*Alert{a1, a2, a3, a4} {
		outcome, err := st.CreateAlert(ctx, a)
		require.NoError(t, err)
		require.Equal(t, AlertCreated, outcome)
	}

	sum, err := st.GetAlertStatistics(ctx)
	require.NoError(t, err)
	assert.True(t, sum.Exact)
	assert.Equal(t, int64(4), sum.Total)
	assert.Equal(t, int64(4), sum.ByState["active"])
	assert.Equal(t, int64(2), sum.BySeverity["critical"])
	assert.Equal(t, int64(2), sum.BySeverity["warning"])
	assert.Equal(t, int64(4), sum.ByMonitor[m.ID])
	assert.Equal(t, int64(2), sum.ByRule[r1.ID])
	assert.Equal(t, int64(2), sum.ByRule[r2.ID])

	// 2. State transitions: acknowledge a1, suppress a2, inhibit a3, retract a4 (reorg at ledger 200).
	require.NoError(t, st.AcknowledgeAlert(ctx, a1.ID, "alice"))
	require.NoError(t, st.SetAlertSuppressed(ctx, a2.ID, "maintenance window"))
	require.NoError(t, st.MarkAlertInhibited(ctx, a3.ID, r1.ID))
	retracted, err := st.RetractAlertsFromLedger(ctx, m.Network, 200, time.Now().UTC())
	require.NoError(t, err)
	assert.Equal(t, int64(1), retracted)

	sum, err = st.GetAlertStatistics(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(4), sum.Total)
	assert.Equal(t, int64(0), sum.ByState["active"], "every alert transitioned out of active")
	assert.Equal(t, int64(1), sum.ByState["acknowledged"])
	assert.Equal(t, int64(1), sum.ByState["suppressed"])
	assert.Equal(t, int64(1), sum.ByState["inhibited"])
	assert.Equal(t, int64(1), sum.ByState["retracted"])

	// 3. VerifyAlertStatistics confirms zero drift against a full recomputation.
	matched, err := st.VerifyAlertStatistics(ctx)
	require.NoError(t, err)
	assert.True(t, matched, "incremental trigger maintenance must match full recomputation")

	// 4. Simulate drift by corrupting the rollup row; VerifyAlertStatistics
	// detects the mismatch, repairs the rollup, and returns matched == false.
	_, err = st.db.ExecContext(ctx, `UPDATE alert_stat_rollups SET active_count = 999`)
	require.NoError(t, err)
	matched, err = st.VerifyAlertStatistics(ctx)
	require.NoError(t, err)
	assert.False(t, matched, "corrupted rollup must be detected")

	matchedAfterRepair, err := st.VerifyAlertStatistics(ctx)
	require.NoError(t, err)
	assert.True(t, matchedAfterRepair, "VerifyAlertStatistics must repair the drifted rollup")

	// 5. Workspace scoping: another tenant sees 0 alerts and cannot see default's rollup.
	otherCtx := workspace.With(ctx, "tenant-b")
	otherSum, err := st.GetAlertStatistics(otherCtx)
	require.NoError(t, err)
	assert.Zero(t, otherSum.Total)
}

func BenchmarkGetAlertStatisticsRollupVsLive(b *testing.B) {
	ctx := context.Background()
	url := "sqlite://" + filepath.Join(b.TempDir(), "sorobeacon.db")
	require.NoError(b, Migrate(url))
	st, err := NewSQLite(ctx, url)
	require.NoError(b, err)
	b.Cleanup(st.Close)

	m := &Monitor{
		Name:        "Bench Monitor",
		ContractIDs: []string{"CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"},
		Enabled:     true,
	}
	require.NoError(b, st.CreateMonitor(ctx, m))
	r := &Rule{MonitorID: m.ID, Type: "event_match", Params: json.RawMessage(`{}`), Severity: SeverityWarning, Enabled: true}
	require.NoError(b, st.CreateRule(ctx, r))

	for i := 0; i < 500; i++ {
		_, err := st.CreateAlert(ctx, &Alert{
			MonitorID: m.ID,
			RuleID:    r.ID,
			EventID:   fmt.Sprintf("bench-ev-%d", i),
			Severity:  SeverityWarning,
			Ledger:    uint32(1000 + i),
			Payload:   json.RawMessage(`{}`),
		})
		require.NoError(b, err)
	}

	b.Run("Rollup", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, err := st.GetAlertStatistics(ctx)
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("LiveScan", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, err := st.liveAlertStatistics(ctx)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}
