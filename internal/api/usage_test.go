package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/poller"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/usage"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

func TestUsage_AttributionAndSharedContractRule(t *testing.T) {
	ctx := context.Background()
	dbURL := "sqlite://" + filepath.Join(t.TempDir(), "usage.db")
	require.NoError(t, store.Migrate(dbURL))
	st, err := store.NewSQLite(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(st.Close)

	wsAcme := workspace.ID("acme")
	wsBeta := workspace.ID("beta")
	require.NoError(t, st.EnsureWorkspace(ctx, wsAcme))
	require.NoError(t, st.EnsureWorkspace(ctx, wsBeta))

	acmeCtx := workspace.With(ctx, wsAcme)
	betaCtx := workspace.With(ctx, wsBeta)

	okWebhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(okWebhook.Close)

	failWebhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(failWebhook.Close)

	sharedContract := "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"

	// Workspace acme: 1 monitor watching sharedContract, 2 rules, 2 channels (1 ok webhook, 1 failing slack).
	acmeMon := &store.Monitor{
		Name:        "acme-shared-monitor",
		ContractIDs: []string{sharedContract},
		Enabled:     true,
		Priority:    store.PriorityNormal,
	}
	require.NoError(t, st.CreateMonitor(acmeCtx, acmeMon))

	acmeRuleMatch := &store.Rule{
		MonitorID: acmeMon.ID,
		Type:      "event_emitted",
		Params:    json.RawMessage(`{"event_name":"transfer"}`),
		Enabled:   true,
		Severity:  store.SeverityWarning,
	}
	require.NoError(t, st.CreateRule(acmeCtx, acmeRuleMatch))

	acmeRuleNoMatch := &store.Rule{
		MonitorID: acmeMon.ID,
		Type:      "event_emitted",
		Params:    json.RawMessage(`{"event_name":"mint"}`),
		Enabled:   true,
		Severity:  store.SeverityInfo,
	}
	require.NoError(t, st.CreateRule(acmeCtx, acmeRuleNoMatch))

	acmeChOK := &store.Channel{
		Name:    "acme-webhook-ok",
		Type:    "webhook",
		Enabled: true,
		Config:  json.RawMessage(`{"url":"` + okWebhook.URL + `","secret":"acme-hook-secret"}`),
	}
	require.NoError(t, st.CreateChannel(acmeCtx, acmeChOK))

	acmeChFail := &store.Channel{
		Name:    "acme-slack-fail",
		Type:    "slack",
		Enabled: true,
		Config:  json.RawMessage(`{"webhook_url":"` + failWebhook.URL + `"}`),
	}
	require.NoError(t, st.CreateChannel(acmeCtx, acmeChFail))
	require.NoError(t, st.SetMonitorChannels(acmeCtx, acmeMon.ID, []int64{acmeChOK.ID, acmeChFail.ID}))

	// Workspace beta: 1 monitor watching the SAME sharedContract, 1 rule, 1 channel (ok webhook).
	betaMon := &store.Monitor{
		Name:        "beta-shared-monitor",
		ContractIDs: []string{sharedContract},
		Enabled:     true,
		Priority:    store.PriorityNormal,
	}
	require.NoError(t, st.CreateMonitor(betaCtx, betaMon))

	betaRuleMatch := &store.Rule{
		MonitorID: betaMon.ID,
		Type:      "event_emitted",
		Params:    json.RawMessage(`{"event_name":"transfer"}`),
		Enabled:   true,
		Severity:  store.SeverityCritical,
	}
	require.NoError(t, st.CreateRule(betaCtx, betaRuleMatch))

	betaChOK := &store.Channel{
		Name:    "beta-webhook-ok",
		Type:    "webhook",
		Enabled: true,
		Config:  json.RawMessage(`{"url":"` + okWebhook.URL + `","secret":"beta-hook-secret"}`),
	}
	require.NoError(t, st.CreateChannel(betaCtx, betaChOK))
	require.NoError(t, st.SetMonitorChannels(betaCtx, betaMon.ID, []int64{betaChOK.ID}))

	m := metrics.New()
	um := usage.NewMeter(st, m, discardLogger())
	reg := rules.NewRegistry()
	factory := notify.DefaultFactory()
	disp := notify.NewDispatcher(st, factory, discardLogger()).WithMetrics(m).WithUsageMeter(um)
	ing := poller.NewIngestor(st, reg, disp, discardLogger()).WithMetrics(m).WithUsageMeter(um)

	// Load monitors across all workspaces as the poller does under system context.
	sysCtx := workspace.WithSystem(ctx)
	monitors, err := st.ListMonitors(sysCtx, true)
	require.NoError(t, err)
	require.Len(t, monitors, 2)

	// Ingest one event from the shared contract.
	ev := &stellar.DecodedEvent{
		ID:         "0001000-1",
		ContractID: sharedContract,
		Ledger:     1000,
		TxHash:     "tx-shared-1",
		Topics:     []any{"transfer"},
		Value:      map[string]any{"amount": 100},
	}
	res := ing.Handle(sysCtx, ev, monitors, poller.HandleOptions{Deliver: true})
	require.Equal(t, 2, res.Matched)
	require.Equal(t, 2, res.Alerts)
	require.Equal(t, 2, res.Dispatched)

	authn := auth.NewBound([]auth.Binding{
		{Workspace: workspace.Default, Token: "operator-admin-token"},
		{Workspace: wsAcme, Token: "acme-admin-token"},
		{Workspace: wsBeta, Token: "beta-admin-token"},
	}, time.Hour)
	tokenMgr := auth.NewManager(st, discardLogger())
	authn.WithTokens(tokenMgr)

	srv := New(st, reg, factory, &fakeRPC{}, discardLogger()).
		WithAuth(authn).
		WithTokens(tokenMgr).
		WithMetrics(m).
		WithUsageMeter(um)
	router := srv.Routes()

	doReq := func(token, path string) (*httptest.ResponseRecorder, UsageResponse) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var out UsageResponse
		if rec.Code == http.StatusOK {
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		}
		return rec, out
	}

	// Make 2 API requests as acme (/monitors then /usage).
	recMonitors, _ := doReq("acme-admin-token", "/monitors")
	require.Equal(t, http.StatusOK, recMonitors.Code)

	recAcme, acmeUsage := doReq("acme-admin-token", "/usage")
	require.Equal(t, http.StatusOK, recAcme.Code)
	assert.Equal(t, "acme", acmeUsage.WorkspaceID)
	assert.Equal(t, int64(1), acmeUsage.AlertsRaised, "acme raised 1 alert on shared contract")
	assert.Equal(t, int64(2), acmeUsage.RuleEvaluations, "acme evaluated 2 rules on shared contract")
	assert.Equal(t, int64(2), acmeUsage.APIRequests, "acme made 2 API requests (/monitors and /usage)")
	assert.Equal(t, []store.WorkspaceNotificationUsage{
		{ChannelType: "slack", Outcome: "error", Count: 1},
		{ChannelType: "webhook", Outcome: "ok", Count: 1},
	}, acmeUsage.Notifications)
	assert.Equal(t, store.WorkspaceStorageCounts{
		Alerts:           1,
		DeliveryAttempts: 2,
		Monitors:         1,
		Rules:            2,
		Channels:         2,
		DeadLetters:      1,
	}, acmeUsage.StoredRows)

	// Make 1 API request as beta (/usage).
	recBeta, betaUsage := doReq("beta-admin-token", "/usage")
	require.Equal(t, http.StatusOK, recBeta.Code)
	assert.Equal(t, "beta", betaUsage.WorkspaceID)
	assert.Equal(t, int64(1), betaUsage.AlertsRaised, "beta raised 1 alert on shared contract")
	assert.Equal(t, int64(1), betaUsage.RuleEvaluations, "beta evaluated 1 rule on shared contract")
	assert.Equal(t, int64(1), betaUsage.APIRequests, "beta made 1 API request (/usage)")
	assert.Equal(t, []store.WorkspaceNotificationUsage{
		{ChannelType: "webhook", Outcome: "ok", Count: 1},
	}, betaUsage.Notifications)
	assert.Equal(t, store.WorkspaceStorageCounts{
		Alerts:           1,
		DeliveryAttempts: 1,
		Monitors:         1,
		Rules:            1,
		Channels:         1,
		DeadLetters:      0,
	}, betaUsage.StoredRows)

	// Cross-workspace query by workspace-bound token must fail with 403 Forbidden.
	recCross, _ := doReq("acme-admin-token", "/usage?workspace=beta")
	assert.Equal(t, http.StatusForbidden, recCross.Code)

	// Scoped database token with stats:read in acme can read acme's usage, but not beta's.
	rawScopedStats, _, err := tokenMgr.Mint(acmeCtx, "acme-stats-reader", []auth.Scope{auth.ScopeStatsRead}, time.Time{})
	require.NoError(t, err)
	recScopedOK, scopedUsage := doReq(rawScopedStats, "/usage")
	require.Equal(t, http.StatusOK, recScopedOK.Code)
	assert.Equal(t, "acme", scopedUsage.WorkspaceID)
	recScopedCross, _ := doReq(rawScopedStats, "/usage?workspace=beta")
	assert.Equal(t, http.StatusForbidden, recScopedCross.Code)

	// Scoped token lacking stats:read is denied.
	rawScopedMonitorsOnly, _, err := tokenMgr.Mint(acmeCtx, "acme-monitors-reader", []auth.Scope{auth.ScopeMonitorsRead}, time.Time{})
	require.NoError(t, err)
	recScopedDenied, _ := doReq(rawScopedMonitorsOnly, "/usage")
	assert.Equal(t, http.StatusForbidden, recScopedDenied.Code)

	// Operator token in default workspace can query all workspaces or filter by workspace.
	recOpAll, opAll := doReq("operator-admin-token", "/usage")
	require.Equal(t, http.StatusOK, recOpAll.Code)
	wsMap := make(map[string]store.WorkspaceUsageSummary, len(opAll.Workspaces))
	for _, w := range opAll.Workspaces {
		wsMap[w.WorkspaceID] = w
	}
	require.Contains(t, wsMap, "acme")
	require.Contains(t, wsMap, "beta")
	assert.Equal(t, int64(2), wsMap["acme"].RuleEvaluations)
	assert.Equal(t, int64(1), wsMap["beta"].RuleEvaluations)

	recOpBeta, opBeta := doReq("operator-admin-token", "/usage?workspace=beta")
	require.Equal(t, http.StatusOK, recOpBeta.Code)
	assert.Equal(t, "beta", opBeta.WorkspaceID)
	require.Len(t, opBeta.Workspaces, 1)
}

func TestUsage_SurvivesRestartAndExposesOpaqueMetrics(t *testing.T) {
	ctx := context.Background()
	dbFile := filepath.Join(t.TempDir(), "restart_usage.db")
	dbURL := "sqlite://" + dbFile
	require.NoError(t, store.Migrate(dbURL))

	t0 := time.Date(2026, 4, 15, 10, 15, 0, 0, time.UTC)

	// Process 1: record usage and flush on shutdown.
	st1, err := store.NewSQLite(ctx, dbURL)
	require.NoError(t, err)
	require.NoError(t, st1.EnsureWorkspace(ctx, workspace.ID("acme")))

	m1 := metrics.New()
	um1 := usage.NewMeter(st1, m1, discardLogger()).WithNow(func() time.Time { return t0 })
	um1.RecordAlertRaised("acme", 3)
	um1.RecordRuleEvaluations("acme", 12)
	um1.RecordNotification("acme", "webhook", "ok", 3)
	um1.RecordAPIRequest("acme", 7)
	require.NoError(t, um1.Flush(ctx))
	st1.Close()

	// Process 2: reopen store on the same DB file, record more usage in the same and next hour, and query GET /usage.
	st2, err := store.NewSQLite(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(st2.Close)

	m2 := metrics.New()
	um2 := usage.NewMeter(st2, m2, discardLogger()).WithNow(func() time.Time { return t0.Add(10 * time.Minute) })
	um2.RecordAlertRaised("acme", 2)
	um2.RecordRuleEvaluations("acme", 8)
	um2.RecordNotification("acme", "webhook", "ok", 2)
	um2.RecordNotification("acme", "webhook", "throttled", 1)

	srv := New(st2, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger()).
		WithMetrics(m2).
		WithUsageMeter(um2)
	router := srv.Routes()

	req := httptest.NewRequest(http.MethodGet, "/usage?workspace=acme&from=2026-04-15&to=2026-04-15", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp UsageResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "acme", resp.WorkspaceID)
	assert.Equal(t, int64(5), resp.AlertsRaised, "alerts_raised must aggregate across restart (3 + 2)")
	assert.Equal(t, int64(20), resp.RuleEvaluations, "rule_evaluations must aggregate across restart (12 + 8)")
	assert.Equal(t, int64(7), resp.APIRequests, "api_requests from process 1 must persist across restart")
	assert.Equal(t, []store.WorkspaceNotificationUsage{
		{ChannelType: "webhook", Outcome: "ok", Count: 5},
		{ChannelType: "webhook", Outcome: "throttled", Count: 1},
	}, resp.Notifications)

	// Verify /metrics uses opaque hashed workspace label and never exposes raw "acme".
	mReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	mRec := httptest.NewRecorder()
	m2.Handler().ServeHTTP(mRec, mReq)
	require.Equal(t, http.StatusOK, mRec.Code)
	body := mRec.Body.String()
	expectedLabel := metrics.HashWorkspaceLabel("acme")
	assert.Contains(t, body, `sorobeacon_workspace_alerts_raised_total{workspace="`+expectedLabel+`"} 2`)
	assert.Contains(t, body, `sorobeacon_workspace_rule_evaluations_total{workspace="`+expectedLabel+`"} 8`)
	assert.NotContains(t, body, `workspace="acme"`)
}
