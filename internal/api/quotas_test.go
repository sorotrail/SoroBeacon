package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/api/graphql"
	"github.com/sorotrail/sorobeacon/internal/apierr"
	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/poller"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

type quotaProblemBody struct {
	Error             string  `json:"error"`
	Code              string  `json:"code"`
	Quota             string  `json:"quota"`
	Resource          string  `json:"resource"`
	WorkspaceID       string  `json:"workspace_id"`
	Limit             int64   `json:"limit"`
	Current           int64   `json:"current"`
	Cost              float64 `json:"cost"`
	Budget            float64 `json:"budget"`
	Remaining         float64 `json:"remaining"`
	RetryAfterSeconds int     `json:"retry_after_seconds"`
}

func TestWorkspaceQuotas_ResourceLimitsAndSSE(t *testing.T) {
	ctx := context.Background()
	dbURL := "sqlite://" + filepath.Join(t.TempDir(), "quotas.db")
	require.NoError(t, store.Migrate(dbURL))
	st, err := store.NewSQLite(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(st.Close)

	wsAcme := workspace.ID("acme")
	wsBeta := workspace.ID("beta")
	require.NoError(t, st.EnsureWorkspace(ctx, wsAcme))
	require.NoError(t, st.EnsureWorkspace(ctx, wsBeta))

	authn := auth.NewBound([]auth.Binding{
		{Workspace: workspace.Default, Token: "operator-admin-token"},
		{Workspace: wsAcme, Token: "acme-admin-token"},
		{Workspace: wsBeta, Token: "beta-admin-token"},
	}, time.Hour)

	m := metrics.New()
	srv := New(st, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger()).
		WithAuth(authn).
		WithMetrics(m)
	router := srv.Routes()

	doJSON := func(token, method, path, body string) *httptest.ResponseRecorder {
		var r *bytes.Reader
		if body != "" {
			r = bytes.NewReader([]byte(body))
		} else {
			r = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, r)
		req.Header.Set("Authorization", "Bearer "+token)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	// Configure quotas for acme: 1 monitor, 1 rule, 1 channel, 1 subscription.
	putRec := doJSON("acme-admin-token", http.MethodPut, "/quotas", `{
		"max_monitors": 1,
		"max_rules": 1,
		"max_channels": 1,
		"max_subscriptions": 1,
		"query_cost_budget": 100,
		"query_cost_refill_per_sec": 10
	}`)
	require.Equal(t, http.StatusOK, putRec.Code, putRec.Body.String())

	var summary WorkspaceQuotaUsageSummary
	require.NoError(t, json.Unmarshal(putRec.Body.Bytes(), &summary))
	assert.Equal(t, "acme", summary.WorkspaceID)
	assert.Equal(t, int64(1), summary.MaxMonitors)
	assert.Equal(t, int64(1), summary.MaxRules)
	assert.Equal(t, int64(1), summary.MaxChannels)
	assert.Equal(t, int64(1), summary.MaxSubscriptions)

	// 1. Monitors quota enforcement.
	contractID := "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	mon1Rec := doJSON("acme-admin-token", http.MethodPost, "/monitors",
		fmt.Sprintf(`{"name":"m1","contract_ids":[%q],"enabled":true}`, contractID))
	require.Equal(t, http.StatusCreated, mon1Rec.Code, mon1Rec.Body.String())
	var mon1 store.Monitor
	require.NoError(t, json.Unmarshal(mon1Rec.Body.Bytes(), &mon1))

	mon2Rec := doJSON("acme-admin-token", http.MethodPost, "/monitors",
		fmt.Sprintf(`{"name":"m2","contract_ids":[%q],"enabled":true}`, contractID))
	require.Equal(t, http.StatusTooManyRequests, mon2Rec.Code)
	assert.NotEmpty(t, mon2Rec.Header().Get("Retry-After"))
	var monErr quotaProblemBody
	require.NoError(t, json.Unmarshal(mon2Rec.Body.Bytes(), &monErr))
	assert.Equal(t, apierr.CodeQuotaExceeded, monErr.Code)
	assert.Equal(t, store.QuotaResourceMonitors, monErr.Quota)
	assert.Equal(t, int64(1), monErr.Limit)
	assert.Equal(t, int64(1), monErr.Current)

	// 2. Rules quota enforcement.
	rule1Rec := doJSON("acme-admin-token", http.MethodPost, fmt.Sprintf("/monitors/%d/rules", mon1.ID),
		`{"type":"event_emitted","params":{"event_name":"transfer"},"enabled":true}`)
	require.Equal(t, http.StatusCreated, rule1Rec.Code, rule1Rec.Body.String())

	rule2Rec := doJSON("acme-admin-token", http.MethodPost, fmt.Sprintf("/monitors/%d/rules", mon1.ID),
		`{"type":"event_emitted","params":{"event_name":"mint"},"enabled":true}`)
	require.Equal(t, http.StatusTooManyRequests, rule2Rec.Code)
	var ruleErr quotaProblemBody
	require.NoError(t, json.Unmarshal(rule2Rec.Body.Bytes(), &ruleErr))
	assert.Equal(t, apierr.CodeQuotaExceeded, ruleErr.Code)
	assert.Equal(t, store.QuotaResourceRules, ruleErr.Quota)
	assert.Equal(t, int64(1), ruleErr.Limit)
	assert.Equal(t, int64(1), ruleErr.Current)

	// 3. Channels quota enforcement.
	ch1Rec := doJSON("acme-admin-token", http.MethodPost, "/channels",
		`{"name":"c1","type":"webhook","enabled":true,"config":{"url":"https://example.com/hook","secret":"s1"}}`)
	require.Equal(t, http.StatusCreated, ch1Rec.Code, ch1Rec.Body.String())

	ch2Rec := doJSON("acme-admin-token", http.MethodPost, "/channels",
		`{"name":"c2","type":"webhook","enabled":true,"config":{"url":"https://example.com/hook2","secret":"s2"}}`)
	require.Equal(t, http.StatusTooManyRequests, ch2Rec.Code)
	var chErr quotaProblemBody
	require.NoError(t, json.Unmarshal(ch2Rec.Body.Bytes(), &chErr))
	assert.Equal(t, apierr.CodeQuotaExceeded, chErr.Code)
	assert.Equal(t, store.QuotaResourceChannels, chErr.Quota)
	assert.Equal(t, int64(1), chErr.Limit)
	assert.Equal(t, int64(1), chErr.Current)

	// 4. SSE subscription quota enforcement.
	ts := httptest.NewServer(router)
	t.Cleanup(ts.Close)

	sseCtx, sseCancel := context.WithCancel(ctx)
	defer sseCancel()
	sseReq1, err := http.NewRequestWithContext(sseCtx, http.MethodGet, ts.URL+"/alerts/stream", nil)
	require.NoError(t, err)
	sseReq1.Header.Set("Authorization", "Bearer acme-admin-token")
	sseResp1, err := ts.Client().Do(sseReq1)
	require.NoError(t, err)
	defer sseResp1.Body.Close()
	require.Equal(t, http.StatusOK, sseResp1.StatusCode)

	// Second concurrent SSE subscription for acme must be rejected with 429 QUOTA_EXCEEDED.
	sseRec2 := doJSON("acme-admin-token", http.MethodGet, "/alerts/stream", "")
	require.Equal(t, http.StatusTooManyRequests, sseRec2.Code)
	var sseErr quotaProblemBody
	require.NoError(t, json.Unmarshal(sseRec2.Body.Bytes(), &sseErr))
	assert.Equal(t, apierr.CodeQuotaExceeded, sseErr.Code)
	assert.Equal(t, store.QuotaResourceSubscriptions, sseErr.Quota)
	assert.Equal(t, int64(1), sseErr.Limit)
	assert.Equal(t, int64(1), sseErr.Current)

	// Workspace beta can still open its own SSE stream while acme is at limit.
	betaSSECtx, betaSSECancel := context.WithCancel(ctx)
	defer betaSSECancel()
	betaSSEReq, err := http.NewRequestWithContext(betaSSECtx, http.MethodGet, ts.URL+"/alerts/stream", nil)
	require.NoError(t, err)
	betaSSEReq.Header.Set("Authorization", "Bearer beta-admin-token")
	betaSSEResp, err := ts.Client().Do(betaSSEReq)
	require.NoError(t, err)
	defer betaSSEResp.Body.Close()
	require.Equal(t, http.StatusOK, betaSSEResp.StatusCode)

	// Closing acme's first SSE stream releases the slot.
	sseCancel()
	require.Eventually(t, func() bool {
		return srv.activeSubscriptionCount("acme") == 0
	}, 2*time.Second, 10*time.Millisecond)

	// Deleting monitor 1 frees the monitor and rule quota slots.
	delRec := doJSON("acme-admin-token", http.MethodDelete, fmt.Sprintf("/monitors/%d", mon1.ID), "")
	require.Equal(t, http.StatusNoContent, delRec.Code)

	mon3Rec := doJSON("acme-admin-token", http.MethodPost, "/monitors",
		fmt.Sprintf(`{"name":"m3","contract_ids":[%q],"enabled":true}`, contractID))
	require.Equal(t, http.StatusCreated, mon3Rec.Code)
}

func TestWorkspaceQuotas_UnifiedQueryBudget_RESTAndGraphQL(t *testing.T) {
	ctx := context.Background()
	dbURL := "sqlite://" + filepath.Join(t.TempDir(), "budget.db")
	require.NoError(t, store.Migrate(dbURL))
	st, err := store.NewSQLite(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(st.Close)

	wsAcme := workspace.ID("acme")
	wsBeta := workspace.ID("beta")
	require.NoError(t, st.EnsureWorkspace(ctx, wsAcme))
	require.NoError(t, st.EnsureWorkspace(ctx, wsBeta))

	// Seed a monitor + rule in beta to verify ingestion/delivery continues when acme is throttled.
	betaCtx := workspace.With(ctx, wsBeta)
	sharedContract := "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	betaMon := &store.Monitor{
		Name:        "beta-mon",
		ContractIDs: []string{sharedContract},
		Enabled:     true,
		Priority:    store.PriorityNormal,
	}
	require.NoError(t, st.CreateMonitor(betaCtx, betaMon))
	require.NoError(t, st.CreateRule(betaCtx, &store.Rule{
		MonitorID: betaMon.ID,
		Type:      "event_emitted",
		Params:    json.RawMessage(`{"event_name":"transfer"}`),
		Enabled:   true,
		Severity:  store.SeverityWarning,
	}))

	var nowUnix atomic.Int64
	t0 := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	nowUnix.Store(t0.UnixNano())
	clockFn := func() time.Time {
		return time.Unix(0, nowUnix.Load()).UTC()
	}

	authn := auth.NewBound([]auth.Binding{
		{Workspace: workspace.Default, Token: "operator-admin-token"},
		{Workspace: wsAcme, Token: "acme-admin-token"},
		{Workspace: wsBeta, Token: "beta-admin-token"},
	}, time.Hour)

	m := metrics.New()
	reg := rules.NewRegistry()
	factory := notify.DefaultFactory()
	srv := New(st, reg, factory, &fakeRPC{}, discardLogger()).
		WithAuth(authn).
		WithMetrics(m).
		WithQuotaClock(clockFn)
	restRouter := srv.Routes()

	gqlResolver := graphql.NewResolver(st, discardLogger()).WithMetrics(m).WithClock(clockFn)
	gqlHandler := AuthMiddleware(authn, discardLogger())(graphql.NewHandler(gqlResolver, graphql.DefaultServerConfig()))

	doREST := func(token, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		restRouter.ServeHTTP(rec, req)
		return rec
	}

	doGraphQL := func(token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		gqlHandler.ServeHTTP(rec, req)
		return rec
	}

	// Set acme query budget to 8 cost units with refill rate of 1 unit/sec.
	putRec := doREST("acme-admin-token", http.MethodPut, "/quotas", `{
		"query_cost_budget": 8,
		"query_cost_refill_per_sec": 1
	}`)
	require.Equal(t, http.StatusOK, putRec.Code, putRec.Body.String())

	// 1. REST GET /stats costs 5 units (remaining: 3).
	restStats := doREST("acme-admin-token", http.MethodGet, "/stats", "")
	require.Equal(t, http.StatusOK, restStats.Code)
	assert.Equal(t, "5", restStats.Header().Get("X-Query-Cost-Charged"))
	assert.Equal(t, "3", restStats.Header().Get("X-Query-Budget-Remaining"))

	// 2. GraphQL POST /graphql `{ stats { monitors } }` costs 2 units (root field + scalar field) (remaining: 1).
	gqlStats := doGraphQL("acme-admin-token", `{"query":"{ stats { monitors } }"}`)
	require.Equal(t, http.StatusOK, gqlStats.Code, gqlStats.Body.String())
	assert.Equal(t, "1", gqlStats.Header().Get("X-Query-Budget-Remaining"))

	// Inspect GET /quotas (exempt from consuming query budget).
	qRec := doREST("acme-admin-token", http.MethodGet, "/quotas", "")
	require.Equal(t, http.StatusOK, qRec.Code)
	var qResp QuotasResponse
	require.NoError(t, json.Unmarshal(qRec.Body.Bytes(), &qResp))
	assert.InDelta(t, 1.0, qResp.QueryBudgetRemaining, 0.01)

	// 3. Next REST GET /stats (cost 5 > remaining 1) must be throttled with 429 and Retry-After: 4.
	restThrottled := doREST("acme-admin-token", http.MethodGet, "/stats", "")
	require.Equal(t, http.StatusTooManyRequests, restThrottled.Code)
	assert.Equal(t, "4", restThrottled.Header().Get("Retry-After"))
	var restProb quotaProblemBody
	require.NoError(t, json.Unmarshal(restThrottled.Body.Bytes(), &restProb))
	assert.Equal(t, apierr.CodeQuotaExceeded, restProb.Code)
	assert.Equal(t, store.QuotaResourceQueryCostBudget, restProb.Quota)
	assert.Equal(t, 4, restProb.RetryAfterSeconds)

	// 4. GraphQL query `{ alerts(first: 10) { totalCount } }` (cost >= 2 > remaining 1) shares the same budget and is also throttled with 429.
	gqlThrottled := doGraphQL("acme-admin-token", `{"query":"{ alerts(first: 10) { totalCount } }"}`)
	require.Equal(t, http.StatusTooManyRequests, gqlThrottled.Code)
	assert.NotEmpty(t, gqlThrottled.Header().Get("Retry-After"))
	var gqlProb quotaProblemBody
	require.NoError(t, json.Unmarshal(gqlThrottled.Body.Bytes(), &gqlProb))
	assert.Equal(t, apierr.CodeQuotaExceeded, gqlProb.Code)
	assert.Equal(t, store.QuotaResourceQueryCostBudget, gqlProb.Quota)

	// 5. Health/readiness and /quotas endpoints are never throttled.
	assert.Equal(t, http.StatusOK, doREST("", http.MethodGet, "/livez", "").Code)
	assert.Equal(t, http.StatusOK, doREST("", http.MethodGet, "/health", "").Code)
	assert.Equal(t, http.StatusOK, doREST("acme-admin-token", http.MethodGet, "/quotas", "").Code)

	// 6. Workspace beta is completely unaffected across REST, GraphQL, and ingestion.
	assert.Equal(t, http.StatusOK, doREST("beta-admin-token", http.MethodGet, "/stats", "").Code)
	assert.Equal(t, http.StatusOK, doGraphQL("beta-admin-token", `{"query":"{ alerts(first: 10) { totalCount } }"}`).Code)

	disp := notify.NewDispatcher(st, factory, discardLogger())
	ing := poller.NewIngestor(st, reg, disp, discardLogger())
	sysCtx := workspace.WithSystem(ctx)
	monitors, err := st.ListMonitors(sysCtx, true)
	require.NoError(t, err)
	res := ing.Handle(sysCtx, &stellar.DecodedEvent{
		ID:         "0002000-1",
		ContractID: sharedContract,
		Ledger:     2000,
		TxHash:     "tx-beta-ok",
		Topics:     []any{"transfer"},
		Value:      map[string]any{"amount": 50},
	}, monitors, poller.HandleOptions{Deliver: false})
	assert.Equal(t, 1, res.Alerts, "beta alert ingestion must remain unaffected while acme is throttled")

	// 7. Advance clock by 10 seconds so acme's token bucket refills to 8 units and verify both REST and GraphQL succeed again.
	nowUnix.Store(t0.Add(10 * time.Second).UnixNano())
	assert.Equal(t, http.StatusOK, doREST("acme-admin-token", http.MethodGet, "/stats", "").Code)
	assert.Equal(t, http.StatusOK, doGraphQL("acme-admin-token", `{"query":"{ stats { monitors } }"}`).Code)

	// 8. Verify Prometheus metrics use hashed workspace labels and never expose raw "acme".
	mRec := httptest.NewRecorder()
	m.Handler().ServeHTTP(mRec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, mRec.Code)
	mBody := mRec.Body.String()
	hashedAcme := metrics.HashWorkspaceLabel("acme")
	assert.Contains(t, mBody, fmt.Sprintf(`sorobeacon_workspace_query_budget_throttled_total{surface="rest",workspace=%q}`, hashedAcme))
	assert.Contains(t, mBody, fmt.Sprintf(`sorobeacon_workspace_query_budget_throttled_total{surface="graphql",workspace=%q}`, hashedAcme))
	assert.NotContains(t, mBody, `workspace="acme"`)
}

func TestWorkspaceQuotas_ConcurrentBoundaryHTTP(t *testing.T) {
	ctx := context.Background()
	dbURL := "sqlite://" + filepath.Join(t.TempDir(), "concurrent_quotas.db")
	require.NoError(t, store.Migrate(dbURL))
	st, err := store.NewSQLite(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(st.Close)

	wsAcme := workspace.ID("acme")
	require.NoError(t, st.EnsureWorkspace(ctx, wsAcme))

	authn := auth.NewBound([]auth.Binding{
		{Workspace: wsAcme, Token: "acme-admin-token"},
	}, time.Hour)

	srv := New(st, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger()).
		WithAuth(authn)
	router := srv.Routes()

	putReq := httptest.NewRequest(http.MethodPut, "/quotas", strings.NewReader(`{"max_monitors":3}`))
	putReq.Header.Set("Authorization", "Bearer acme-admin-token")
	putReq.Header.Set("Content-Type", "application/json")
	putRec := httptest.NewRecorder()
	router.ServeHTTP(putRec, putReq)
	require.Equal(t, http.StatusOK, putRec.Code)

	contractID := "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	const workers = 10
	var createdCount, rejectedCount atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			body := fmt.Sprintf(`{"name":"concurrent-mon-%d","contract_ids":[%q],"enabled":true}`, idx, contractID)
			req := httptest.NewRequest(http.MethodPost, "/monitors", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer acme-admin-token")
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			switch rec.Code {
			case http.StatusCreated:
				createdCount.Add(1)
			case http.StatusTooManyRequests:
				rejectedCount.Add(1)
			default:
				t.Errorf("unexpected status code %d: %s", rec.Code, rec.Body.String())
			}
		}(i)
	}

	close(start)
	wg.Wait()

	assert.Equal(t, int32(3), createdCount.Load(), "exactly 3 monitors must be created at the quota boundary")
	assert.Equal(t, int32(7), rejectedCount.Load(), "remaining 7 concurrent requests must be rejected with 429")
}
