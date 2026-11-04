package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/apierr"
	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

func TestWorkspaceRetention_EndpointsConfirmationAuditAndHorizon(t *testing.T) {
	ctx := context.Background()
	dbURL := "sqlite://" + filepath.Join(t.TempDir(), "retention.db")
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
		WithMetrics(m).
		WithAlertRetention(30 * 24 * time.Hour)
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

	// 1. GET /retention before override returns instance default (30d).
	getRec := doJSON("acme-admin-token", http.MethodGet, "/retention", "")
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	assert.Equal(t, store.RetentionPolicySourceInstance, getRec.Header().Get("X-Retention-Policy-Source"))
	assert.NotEmpty(t, getRec.Header().Get("X-Pruned-Before"))

	var getResp RetentionResponse
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &getResp))
	assert.Equal(t, "acme", getResp.WorkspaceID)
	assert.False(t, getResp.Configured)
	assert.Equal(t, store.RetentionPolicySourceInstance, getResp.PolicySource)
	assert.Equal(t, int64(30*86400), getResp.EffectiveRetentionSeconds)
	assert.NotNil(t, getResp.PrunedBefore)

	// 2. PUT /retention shortening from 30d default to 7d without confirm_shorten returns 409 CONFLICT.
	conflictRec := doJSON("acme-admin-token", http.MethodPut, "/retention", `{"alert_retention":"7d"}`)
	require.Equal(t, http.StatusConflict, conflictRec.Code, conflictRec.Body.String())
	var conflictBody map[string]any
	require.NoError(t, json.Unmarshal(conflictRec.Body.Bytes(), &conflictBody))
	assert.Equal(t, apierr.CodeConflict, conflictBody["code"])
	assert.Equal(t, true, conflictBody["confirmation_required"])

	// 3. PUT /retention shortening with confirm_shorten=true succeeds and sets 7d override.
	putRec := doJSON("acme-admin-token", http.MethodPut, "/retention", `{"alert_retention":"7d","confirm_shorten":true}`)
	require.Equal(t, http.StatusOK, putRec.Code, putRec.Body.String())
	assert.Equal(t, store.RetentionPolicySourceWorkspace, putRec.Header().Get("X-Retention-Policy-Source"))
	var putSummary RetentionSummary
	require.NoError(t, json.Unmarshal(putRec.Body.Bytes(), &putSummary))
	assert.Equal(t, "acme", putSummary.WorkspaceID)
	assert.True(t, putSummary.Configured)
	assert.Equal(t, store.RetentionPolicySourceWorkspace, putSummary.PolicySource)
	assert.Equal(t, int64(7*86400), putSummary.EffectiveRetentionSeconds)

	// 4. PUT /retention extending beta from 30d default to 90d succeeds without confirm_shorten.
	betaPutRec := doJSON("beta-admin-token", http.MethodPut, "/retention", `{"alert_retention_days":90}`)
	require.Equal(t, http.StatusOK, betaPutRec.Code, betaPutRec.Body.String())
	var betaSummary RetentionSummary
	require.NoError(t, json.Unmarshal(betaPutRec.Body.Bytes(), &betaSummary))
	assert.Equal(t, "beta", betaSummary.WorkspaceID)
	assert.Equal(t, int64(90*86400), betaSummary.EffectiveRetentionSeconds)

	// 5. Cross-workspace operator can inspect all workspaces via ?workspace=all, while tenant token gets 403.
	forbiddenRec := doJSON("acme-admin-token", http.MethodGet, "/retention?workspace=beta", "")
	assert.Equal(t, http.StatusForbidden, forbiddenRec.Code)

	opAllRec := doJSON("operator-admin-token", http.MethodGet, "/retention?workspace=all", "")
	require.Equal(t, http.StatusOK, opAllRec.Code, opAllRec.Body.String())
	var opAllResp RetentionResponse
	require.NoError(t, json.Unmarshal(opAllRec.Body.Bytes(), &opAllResp))
	assert.GreaterOrEqual(t, len(opAllResp.Workspaces), 2)

	// 6. Alert listing, stats, daily series, and exports expose pruned_before and X-Pruned-Before / X-Retention-Policy-Source.
	alertsRec := doJSON("acme-admin-token", http.MethodGet, "/alerts", "")
	require.Equal(t, http.StatusOK, alertsRec.Code)
	assert.Equal(t, store.RetentionPolicySourceWorkspace, alertsRec.Header().Get("X-Retention-Policy-Source"))
	assert.NotEmpty(t, alertsRec.Header().Get("X-Pruned-Before"))
	var alertsBody map[string]any
	require.NoError(t, json.Unmarshal(alertsRec.Body.Bytes(), &alertsBody))
	assert.Equal(t, store.RetentionPolicySourceWorkspace, alertsBody["retention_policy_source"])
	assert.NotEmpty(t, alertsBody["pruned_before"])

	dailyRec := doJSON("acme-admin-token", http.MethodGet, "/stats/alerts-daily", "")
	require.Equal(t, http.StatusOK, dailyRec.Code)
	assert.Equal(t, store.RetentionPolicySourceWorkspace, dailyRec.Header().Get("X-Retention-Policy-Source"))
	assert.NotEmpty(t, dailyRec.Header().Get("X-Pruned-Before"))

	csvRec := doJSON("acme-admin-token", http.MethodGet, "/alerts.csv", "")
	require.Equal(t, http.StatusOK, csvRec.Code)
	assert.Equal(t, store.RetentionPolicySourceWorkspace, csvRec.Header().Get("X-Retention-Policy-Source"))
	assert.NotEmpty(t, csvRec.Header().Get("X-Pruned-Before"))

	ndjsonRec := doJSON("acme-admin-token", http.MethodGet, "/alerts/export", "")
	require.Equal(t, http.StatusOK, ndjsonRec.Code)
	assert.Equal(t, store.RetentionPolicySourceWorkspace, ndjsonRec.Header().Get("X-Retention-Policy-Source"))
	assert.NotEmpty(t, ndjsonRec.Header().Get("X-Pruned-Before"))

	// 7. DELETE /retention on beta (90d -> 30d default) requires confirm_shorten, then succeeds when confirmed.
	delConflictRec := doJSON("beta-admin-token", http.MethodDelete, "/retention", "")
	assert.Equal(t, http.StatusConflict, delConflictRec.Code)

	delOKRec := doJSON("beta-admin-token", http.MethodDelete, "/retention?confirm_shorten=true", "")
	require.Equal(t, http.StatusOK, delOKRec.Code, delOKRec.Body.String())
	var resetSummary RetentionSummary
	require.NoError(t, json.Unmarshal(delOKRec.Body.Bytes(), &resetSummary))
	assert.False(t, resetSummary.Configured)
	assert.Equal(t, store.RetentionPolicySourceInstance, resetSummary.PolicySource)
	assert.Equal(t, int64(30*86400), resetSummary.EffectiveRetentionSeconds)

	// 8. GET /audit confirms retention policy changes were recorded in audit_log.
	auditRec := doJSON("acme-admin-token", http.MethodGet, "/audit?target_type="+store.AuditTargetWorkspaceRetention, "")
	require.Equal(t, http.StatusOK, auditRec.Code, auditRec.Body.String())
	var auditResp struct {
		Entries []store.AuditEntry `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(auditRec.Body.Bytes(), &auditResp))
	assert.NotEmpty(t, auditResp.Entries)
}
