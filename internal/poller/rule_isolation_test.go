package poller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
)

type controllableRuleEvaluator struct {
	delay     time.Duration
	panicMsg  string
	err       error
	dbErr     error
	callCount atomic.Int64
}

func (e *controllableRuleEvaluator) Validate(json.RawMessage) error { return nil }

func (e *controllableRuleEvaluator) Evaluate(ctx context.Context, _ *stellar.DecodedEvent, _ json.RawMessage) (bool, error) {
	e.callCount.Add(1)
	if e.dbErr != nil {
		done := rules.EnterDB(ctx)
		done(e.dbErr)
		return false, rules.WrapStorageError(e.dbErr)
	}
	if e.panicMsg != "" {
		panic(e.panicMsg)
	}
	if e.delay > 0 {
		select {
		case <-time.After(e.delay):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	if e.err != nil {
		return false, e.err
	}
	return true, nil
}

func TestPollerIsolatesSlowAndPanickingRulesAndAdvancesContractCursor(t *testing.T) {
	ctx := context.Background()
	url := "sqlite://" + t.TempDir() + "/isolation.db"
	require.NoError(t, store.Migrate(url))
	st, err := store.NewSQLite(ctx, url)
	require.NoError(t, err)
	defer st.Close()

	m1 := &store.Monitor{Name: "m-broken", WorkspaceID: "ws-a", ContractIDs: []string{contractA}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m1))
	m2 := &store.Monitor{Name: "m-healthy", WorkspaceID: "ws-b", ContractIDs: []string{contractA}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m2))
	require.NoError(t, st.SetIngestState(ctx, "", store.IngestState{LastLedger: 100}))

	reg := rules.NewRegistry()
	slowRuleEval := &controllableRuleEvaluator{delay: 200 * time.Millisecond}
	panicRuleEval := &controllableRuleEvaluator{panicMsg: "nil pointer in custom evaluator"}
	reg.Register("slow_eval", slowRuleEval)
	reg.Register("panic_eval", panicRuleEval)

	rSlow := &store.Rule{MonitorID: m1.ID, Type: "slow_eval", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, rSlow))
	rPanic := &store.Rule{MonitorID: m1.ID, Type: "panic_eval", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, rPanic))
	rHealthy := &store.Rule{MonitorID: m2.ID, Type: rules.TypeEventEmitted, Params: json.RawMessage(`{"event_name":"transfer"}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, rHealthy))

	rpc := &fakeRPC{
		latest: 120,
		responses: []*stellar.GetEventsResult{
			{
				Events: []stellar.Event{
					transferEvent("ev-1", 101, "10"),
					transferEvent("ev-2", 102, "20"),
				},
				LatestLedger: 105,
			},
			{
				Events: []stellar.Event{
					transferEvent("ev-3", 106, "30"),
				},
				LatestLedger: 110,
			},
			{
				Events: []stellar.Event{
					transferEvent("ev-4", 111, "40"),
				},
				LatestLedger: 115,
			},
		},
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
	m := metrics.New()
	d := &fakeDispatcher{}

	p := New(NewRPCSource(rpc, stellar.DefaultDecoder{}), st, reg, d, time.Second, logger).
		WithMetrics(m).
		WithRuleTimeout(20 * time.Millisecond).
		WithRuleQuarantineThreshold(2)

	// Cycle 1: 2 events -> rSlow times out twice and rPanic panics twice.
	// Both broken rules reach quarantine threshold=2, while rHealthy fires 2 alerts
	// and the contract cursor advances to 105 without stalling!
	require.NoError(t, p.Poll(ctx))

	alerts, err := st.ListAlerts(ctx, store.AlertFilter{MonitorID: m2.ID})
	require.NoError(t, err)
	require.Len(t, alerts, 2, "healthy monitor must alert on both events despite sibling slow/panicking rules")

	cursors, err := st.ListContractCursors(ctx, "")
	require.NoError(t, err)
	require.Len(t, cursors, 1)
	assert.Equal(t, uint32(105), cursors[0].LastLedger, "isolated rule failures must not hold the contract cursor back")

	assert.Len(t, p.QuarantinedRules(), 2, "both failing rules must be quarantined after 2 consecutive failures")
	assert.Contains(t, logBuf.String(), "rule quarantined after repeated evaluation failures")

	// Audit log must record the quarantine transitions.
	audits, err := st.ListAuditEntries(ctx, store.AuditFilter{TargetType: "rule", Actor: "system:rule_quarantine"})
	require.NoError(t, err)
	assert.Len(t, audits, 2, "quarantine transitions must be recorded in audit_log")

	// Cycle 2: quarantined rules are skipped immediately without invoking their evaluators.
	slowCallsBefore := slowRuleEval.callCount.Load()
	panicCallsBefore := panicRuleEval.callCount.Load()
	require.NoError(t, p.Poll(ctx))
	assert.Equal(t, slowCallsBefore, slowRuleEval.callCount.Load(), "quarantined slow rule must be skipped")
	assert.Equal(t, panicCallsBefore, panicRuleEval.callCount.Load(), "quarantined panic rule must be skipped")

	alerts, err = st.ListAlerts(ctx, store.AlertFilter{MonitorID: m2.ID})
	require.NoError(t, err)
	require.Len(t, alerts, 3, "healthy rule continues alerting on cycle 2")

	// Fix rSlow and explicitly re-enable it; fix rPanic by updating its rule version in the store.
	slowRuleEval.delay = 0
	panicRuleEval.panicMsg = ""
	require.True(t, p.ReenableRule(ctx, rSlow.ID))
	rPanic.Params = json.RawMessage(`{"fixed":true}`)
	require.NoError(t, st.UpdateRule(ctx, rPanic))

	// Cycle 3: both recovered rules evaluate and fire alerts alongside rHealthy!
	require.NoError(t, p.Poll(ctx))
	assert.Empty(t, p.QuarantinedRules(), "all rules should be healthy after recovery")

	m1Alerts, err := st.ListAlerts(ctx, store.AlertFilter{MonitorID: m1.ID})
	require.NoError(t, err)
	assert.Len(t, m1Alerts, 2, "both re-enabled rules on m1 must fire on ev-4")
}

func TestPollerStorageFailureInRuleHoldsCursorWithoutQuarantiningRule(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	st.state.LastLedger = 100
	st.monitors = []store.Monitor{{ID: 1, Name: "m1", ContractIDs: []string{contractA}, Enabled: true}}

	reg := rules.NewRegistry()
	storageRule := &controllableRuleEvaluator{dbErr: errors.New("temporary db timeout")}
	reg.Register("db_rule", storageRule)
	st.rules[1] = []store.Rule{{ID: 10, MonitorID: 1, Type: "db_rule", Params: json.RawMessage(`{}`), Enabled: true}}

	rpc := &fakeRPC{
		latest: 110,
		responses: []*stellar.GetEventsResult{
			{
				Events:       []stellar.Event{transferEvent("ev-1", 105, "10")},
				LatestLedger: 110,
			},
			{
				Events:       []stellar.Event{transferEvent("ev-1", 105, "10")},
				LatestLedger: 110,
			},
		},
	}

	p := New(NewRPCSource(rpc, stellar.DefaultDecoder{}), st, reg, &fakeDispatcher{}, time.Second, slog.New(slog.DiscardHandler)).
		WithRuleQuarantineThreshold(1)

	// Cycle 1: storage error holds contract cursor at 104 and does NOT quarantine rule 10.
	require.NoError(t, p.Poll(ctx))
	assert.Empty(t, p.QuarantinedRules(), "storage error must never quarantine a rule")
	cursors, err := st.ListContractCursors(ctx, "")
	require.NoError(t, err)
	require.Len(t, cursors, 1)
	assert.Equal(t, uint32(104), cursors[0].LastLedger, "storage failure must hold contract cursor before failed event")

	// Cycle 2: storage recovers, event replays from ledger 105 and creates the alert.
	storageRule.dbErr = nil
	require.NoError(t, p.Poll(ctx))
	require.Len(t, st.alerts, 1)
	assert.Equal(t, "ev-1", st.alerts[0].EventID)
}

func TestPollerWorkspaceFairnessAndPerWorkspaceBudget(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	st.state.LastLedger = 100

	// Workspace A has two monitors with slow rules (each taking 15ms);
	// Workspace B has one monitor listed after Workspace A's monitors.
	st.monitors = []store.Monitor{
		{ID: 1, Name: "wsA-m1", WorkspaceID: "ws-heavy", ContractIDs: []string{contractA}, Enabled: true},
		{ID: 2, Name: "wsA-m2", WorkspaceID: "ws-heavy", ContractIDs: []string{contractA}, Enabled: true},
		{ID: 3, Name: "wsB-m1", WorkspaceID: "ws-light", ContractIDs: []string{contractA}, Enabled: true},
	}

	reg := rules.NewRegistry()
	heavyEval := &controllableRuleEvaluator{delay: 15 * time.Millisecond}
	reg.Register("heavy_eval", heavyEval)

	st.rules[1] = []store.Rule{
		{ID: 101, MonitorID: 1, Type: "heavy_eval", Params: json.RawMessage(`{}`), Enabled: true},
		{ID: 102, MonitorID: 1, Type: "heavy_eval", Params: json.RawMessage(`{}`), Enabled: true},
	}
	st.rules[2] = []store.Rule{
		{ID: 201, MonitorID: 2, Type: "heavy_eval", Params: json.RawMessage(`{}`), Enabled: true},
	}
	st.rules[3] = []store.Rule{
		{ID: 301, MonitorID: 3, Type: rules.TypeEventEmitted, Params: json.RawMessage(`{"event_name":"transfer"}`), Enabled: true},
	}

	rpc := &fakeRPC{
		latest: 105,
		responses: []*stellar.GetEventsResult{{
			Events:       []stellar.Event{transferEvent("ev-fair", 101, "10")},
			LatestLedger: 105,
		}},
	}

	p := New(NewRPCSource(rpc, stellar.DefaultDecoder{}), st, reg, &fakeDispatcher{}, time.Second, slog.New(slog.DiscardHandler)).
		WithRuleTimeout(100 * time.Millisecond).
		WithWorkspaceEvalBudget(25 * time.Millisecond)

	require.NoError(t, p.Poll(ctx))

	// Workspace B's monitor must be interleaved before wsA-m2 and must fire its alert,
	// while ws-heavy's 3rd rule (rule 201 on wsA-m2) is skipped because ws-heavy
	// exceeded its 25ms per-event budget after rules 101 and 102 (2 x 15ms = 30ms).
	var alertedRules []int64
	for _, a := range st.alerts {
		alertedRules = append(alertedRules, a.RuleID)
	}
	assert.Contains(t, alertedRules, int64(301), "ws-light must alert without being starved by ws-heavy")
	assert.NotContains(t, alertedRules, int64(201), "ws-heavy's third rule must be skipped once its per-event budget is exhausted")
}
