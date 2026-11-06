package api

import (
	"bufio"
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

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

func TestWorkspaceExport_AuthorizationIsolationSecretsAndAlerts(t *testing.T) {
	ctx := context.Background()
	dbURL := "sqlite://" + filepath.Join(t.TempDir(), "workspace_export.db")
	require.NoError(t, store.Migrate(dbURL))
	st, err := store.NewSQLite(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(st.Close)

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 11)
	}
	cipher, err := store.NewAESGCMCipher(key)
	require.NoError(t, err)
	st.WithConfigCipher(cipher)

	wsAlpha := workspace.ID("alpha")
	wsBeta := workspace.ID("beta")
	require.NoError(t, st.EnsureWorkspace(ctx, wsAlpha))
	require.NoError(t, st.EnsureWorkspace(ctx, wsBeta))

	tokenMgr := auth.NewManager(st, discardLogger())

	seedTenant := func(ws workspace.ID, prefix string) string {
		wctx := workspace.With(ctx, ws)

		mon := &store.Monitor{
			ContractIDs: []string{"CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"},
			Name:        prefix + "-monitor",
			Enabled:     true,
			Tags:        []string{prefix + "-tag"},
		}
		require.NoError(t, st.CreateMonitor(wctx, mon))

		ch := &store.Channel{
			Name:    prefix + "-webhook",
			Type:    "webhook",
			Config:  json.RawMessage(`{"url":"https://hooks.example.com/` + prefix + `-secret-hook","bot_token":"` + prefix + `-secret-bot","headers":{"Authorization":"Bearer ` + prefix + `-secret-hdr","X-Tenant":"` + prefix + `"}}`),
			Enabled: true,
		}
		require.NoError(t, st.CreateChannel(wctx, ch))
		require.NoError(t, st.SetMonitorChannels(wctx, mon.ID, []int64{ch.ID}))

		require.NoError(t, st.CreateChannelSigningKey(wctx, &store.ChannelSigningKey{
			ChannelID: ch.ID,
			KeyID:     prefix + "-key-v1",
			Secret:    prefix + "-signing-secret-material",
			IsCurrent: true,
			Status:    store.SigningKeyStatusActive,
		}))

		rule := &store.Rule{
			MonitorID: mon.ID,
			Type:      "event_emitted",
			Params:    json.RawMessage(`{"event_name":"` + prefix + `_transfer"}`),
			Enabled:   true,
			Severity:  store.SeverityCritical,
		}
		require.NoError(t, st.CreateRule(wctx, rule))

		al := &store.Alert{
			MonitorID: mon.ID,
			RuleID:    rule.ID,
			EventID:   prefix + "-evt-001",
			Payload:   json.RawMessage(`{"marker":"` + prefix + `-alert-payload"}`),
			Severity:  store.SeverityCritical,
			Ledger:    100,
		}
		out, err := st.CreateAlert(wctx, al)
		require.NoError(t, err)
		require.Equal(t, store.AlertCreated, out)

		require.NoError(t, st.RecordDeliveryAttempt(wctx, &store.DeliveryAttempt{
			AlertID:         al.ID,
			ChannelID:       ch.ID,
			Status:          store.DeliveryStatusFailed,
			ResponseSnippet: prefix + "-delivery-err",
		}))

		require.NoError(t, st.CreateDeadLetter(wctx, &store.DeadLetter{
			AlertID:      al.ID,
			ChannelID:    ch.ID,
			LastError:    prefix + "-dead-letter",
			AttemptCount: 3,
			LastStatus:   502,
		}))

		rawTok, _, err := tokenMgr.Mint(wctx, prefix+"-scoped-token", []auth.Scope{auth.ScopeMonitorsRead}, time.Time{})
		require.NoError(t, err)
		return rawTok
	}

	alphaScopedToken := seedTenant(wsAlpha, "alpha")
	betaScopedToken := seedTenant(wsBeta, "beta")

	authn := auth.NewBound([]auth.Binding{
		{Workspace: workspace.Default, Token: "instance-operator-token:admin"},
		{Workspace: workspace.Default, Token: "default-viewer-token:viewer"},
		{Workspace: wsAlpha, Token: "alpha-static-token:admin"},
		{Workspace: wsAlpha, Token: "alpha-viewer-token:viewer"},
		{Workspace: wsBeta, Token: "beta-static-token:admin"},
	}, time.Hour).WithTokens(tokenMgr)

	srv := New(st, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger()).
		WithAuth(authn).
		WithTokens(tokenMgr)
	router := srv.Routes()

	doGet := func(token, path string, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	// 1. Authorization Case A: Workspace-bound token (both static and scoped) exporting its own workspace -> 200 OK.
	for _, tc := range []struct {
		name  string
		token string
		path  string
	}{
		{"alpha static token via /workspaces/alpha/export", "alpha-static-token", "/workspaces/alpha/export"},
		{"alpha static token via /workspace/export", "alpha-static-token", "/workspace/export"},
		{"alpha static token via /workspace/export?workspace=alpha", "alpha-static-token", "/workspace/export?workspace=alpha"},
		{"alpha viewer token via /workspaces/alpha/export", "alpha-viewer-token", "/workspaces/alpha/export"},
		{"alpha scoped token via /workspaces/alpha/export", alphaScopedToken, "/workspaces/alpha/export"},
		{"alpha scoped token via /workspace/export", alphaScopedToken, "/workspace/export"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doGet(tc.token, tc.path, nil)
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
			assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")
			assert.Contains(t, rec.Header().Get("Content-Disposition"), "workspace_alpha_export.json")

			raw := rec.Body.String()
			assert.NotContains(t, raw, "beta-", "alpha export must never leak any beta records")

			var bundle store.WorkspaceExportBundle
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &bundle))
			assert.Equal(t, store.WorkspaceExportFormat, bundle.Format)
			assert.Equal(t, store.WorkspaceExportVersion, bundle.Manifest.Version)
			assert.Equal(t, "alpha", bundle.Manifest.WorkspaceID)
			assert.True(t, bundle.Manifest.SecretsRedacted)
			assert.False(t, bundle.Manifest.IncludeSecrets)
			assert.False(t, bundle.Manifest.AlertsIncluded)
			require.Len(t, bundle.Monitors, 1)
			assert.Equal(t, "alpha-monitor", bundle.Monitors[0].Name)
		})
	}

	// 2. Authorization Case B: Workspace-bound token attempting to export another workspace -> 403 Forbidden.
	for _, tc := range []struct {
		name  string
		token string
		path  string
	}{
		{"alpha static token requesting /workspaces/beta/export", "alpha-static-token", "/workspaces/beta/export"},
		{"alpha static token requesting /workspace/export?workspace=beta", "alpha-static-token", "/workspace/export?workspace=beta"},
		{"alpha scoped token requesting /workspaces/beta/export", alphaScopedToken, "/workspaces/beta/export"},
		{"beta static token requesting /workspaces/alpha/export", "beta-static-token", "/workspaces/alpha/export"},
		{"beta scoped token requesting /workspaces/alpha/export", betaScopedToken, "/workspaces/alpha/export"},
		{"default viewer token requesting /workspaces/alpha/export", "default-viewer-token", "/workspaces/alpha/export"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doGet(tc.token, tc.path, nil)
			require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "alpha-monitor")
			assert.NotContains(t, rec.Body.String(), "beta-monitor")
		})
	}

	// 3. Authorization Case C: Instance operator token exporting any workspace -> 200 OK.
	for _, target := range []string{"alpha", "beta"} {
		rec := doGet("instance-operator-token", "/workspaces/"+target+"/export", nil)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

		var bundle store.WorkspaceExportBundle
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &bundle))
		assert.Equal(t, target, bundle.Manifest.WorkspaceID)
		require.Len(t, bundle.Monitors, 1)
		assert.Equal(t, target+"-monitor", bundle.Monitors[0].Name)

		other := "beta"
		if target == "beta" {
			other = "alpha"
		}
		assert.NotContains(t, rec.Body.String(), other+"-", "export of %s must not contain %s data", target, other)
	}

	// 4. Secret handling: default redaction vs explicit include_secrets=true.
	t.Run("secrets redacted by default and included only when requested", func(t *testing.T) {
		recDefault := doGet("alpha-static-token", "/workspaces/alpha/export", nil)
		require.Equal(t, http.StatusOK, recDefault.Code)
		rawDefault := recDefault.Body.String()
		for _, secretMaterial := range []string{
			"alpha-secret-hook",
			"alpha-secret-bot",
			"alpha-secret-hdr",
			"alpha-signing-secret-material",
		} {
			assert.NotContains(t, rawDefault, secretMaterial, "default export leaked secret %q", secretMaterial)
		}
		var defaultBundle store.WorkspaceExportBundle
		require.NoError(t, json.Unmarshal(recDefault.Body.Bytes(), &defaultBundle))
		assert.True(t, defaultBundle.Manifest.SecretsRedacted)
		assert.False(t, defaultBundle.Manifest.IncludeSecrets)
		require.Len(t, defaultBundle.ChannelSigningKeys, 1)
		assert.Equal(t, store.RedactedSecretPlaceholder, defaultBundle.ChannelSigningKeys[0].Secret)
		require.Len(t, defaultBundle.APITokens, 1)
		assert.Equal(t, store.RedactedSecretPlaceholder, defaultBundle.APITokens[0].TokenHash)

		// Non-admin tokens (viewer role or monitors:read scoped token) are denied include_secrets=true with 403 Forbidden.
		for _, nonAdminToken := range []string{"alpha-viewer-token", alphaScopedToken} {
			recDenied := doGet(nonAdminToken, "/workspaces/alpha/export?include_secrets=true", nil)
			require.Equal(t, http.StatusForbidden, recDenied.Code, "non-admin token %q must get 403 on include_secrets=true, got %d: %s", nonAdminToken, recDenied.Code, recDenied.Body.String())
			assert.NotContains(t, recDenied.Body.String(), "alpha-secret-hook")
			assert.NotContains(t, recDenied.Body.String(), "alpha-signing-secret-material")
		}

		recSecrets := doGet("alpha-static-token", "/workspaces/alpha/export?include_secrets=true", nil)
		require.Equal(t, http.StatusOK, recSecrets.Code)
		rawSecrets := recSecrets.Body.String()
		assert.NotContains(t, rawSecrets, "beta-")
		for _, secretMaterial := range []string{
			"alpha-secret-hook",
			"alpha-secret-bot",
			"alpha-secret-hdr",
			"alpha-signing-secret-material",
		} {
			assert.Contains(t, rawSecrets, secretMaterial, "include_secrets=true export missing %q", secretMaterial)
		}
		var secretBundle store.WorkspaceExportBundle
		require.NoError(t, json.Unmarshal(recSecrets.Body.Bytes(), &secretBundle))
		assert.False(t, secretBundle.Manifest.SecretsRedacted)
		assert.True(t, secretBundle.Manifest.IncludeSecrets)
		assert.Equal(t, "alpha-signing-secret-material", secretBundle.ChannelSigningKeys[0].Secret)
		assert.NotEqual(t, store.RedactedSecretPlaceholder, secretBundle.APITokens[0].TokenHash)
		assert.NotEmpty(t, secretBundle.APITokens[0].TokenHash)
	})

	// 5. Alert history handling: omitted by default vs streamed when include_alerts=true.
	t.Run("alerts omitted by default and included with include_alerts=true", func(t *testing.T) {
		recNoAlerts := doGet("alpha-static-token", "/workspaces/alpha/export", nil)
		require.Equal(t, http.StatusOK, recNoAlerts.Code)
		assert.NotContains(t, recNoAlerts.Body.String(), "alpha-alert-payload")
		var noAlertsBundle store.WorkspaceExportBundle
		require.NoError(t, json.Unmarshal(recNoAlerts.Body.Bytes(), &noAlertsBundle))
		assert.False(t, noAlertsBundle.Manifest.AlertsIncluded)
		assert.Empty(t, noAlertsBundle.Alerts)
		assert.Empty(t, noAlertsBundle.AlertDedup)
		assert.Empty(t, noAlertsBundle.DeliveryAttempts)
		assert.Empty(t, noAlertsBundle.DeadLetters)

		recWithAlerts := doGet("alpha-static-token", "/workspaces/alpha/export?include_alerts=true&page_size=1", nil)
		require.Equal(t, http.StatusOK, recWithAlerts.Code)
		assert.Contains(t, recWithAlerts.Body.String(), "alpha-alert-payload")
		assert.NotContains(t, recWithAlerts.Body.String(), "beta-alert-payload")
		var withAlertsBundle store.WorkspaceExportBundle
		require.NoError(t, json.Unmarshal(recWithAlerts.Body.Bytes(), &withAlertsBundle))
		assert.True(t, withAlertsBundle.Manifest.AlertsIncluded)
		require.Len(t, withAlertsBundle.Alerts, 1)
		require.Len(t, withAlertsBundle.AlertDedup, 1)
		require.Len(t, withAlertsBundle.DeliveryAttempts, 1)
		require.Len(t, withAlertsBundle.DeadLetters, 1)
	})

	// 6. Streaming NDJSON export via query parameter and Accept header.
	t.Run("NDJSON streaming export", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			path    string
			headers map[string]string
		}{
			{"query param format=ndjson", "/workspaces/alpha/export?format=ndjson&include_alerts=true", nil},
			{"Accept application/x-ndjson", "/workspaces/alpha/export?include_alerts=true", map[string]string{"Accept": "application/x-ndjson"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := doGet("alpha-static-token", tc.path, tc.headers)
				require.Equal(t, http.StatusOK, rec.Code)
				assert.Contains(t, rec.Header().Get("Content-Type"), "application/x-ndjson")
				assert.Contains(t, rec.Header().Get("Content-Disposition"), "workspace_alpha_export.ndjson")
				assert.NotContains(t, rec.Body.String(), "beta-")

				scanner := bufio.NewScanner(bytes.NewReader(rec.Body.Bytes()))
				var lines []map[string]any
				for scanner.Scan() {
					var obj map[string]any
					require.NoError(t, json.Unmarshal(scanner.Bytes(), &obj))
					lines = append(lines, obj)
				}
				require.NoError(t, scanner.Err())
				require.GreaterOrEqual(t, len(lines), 8)
				assert.Equal(t, string(store.WorkspaceExportKindManifest), lines[0]["kind"])
				assert.Equal(t, string(store.WorkspaceExportKindSummary), lines[len(lines)-1]["kind"])
				for _, line := range lines[1 : len(lines)-1] {
					assert.Equal(t, string(store.WorkspaceExportKindRow), line["kind"])
					assert.NotEmpty(t, line["table"])
				}
			})
		}
	})

	// 7. Determinism: two exports of the same workspace state produce identical table payloads.
	t.Run("deterministic ordering across repeated exports", func(t *testing.T) {
		rec1 := doGet("alpha-static-token", "/workspaces/alpha/export?include_alerts=true", nil)
		rec2 := doGet("alpha-static-token", "/workspaces/alpha/export?include_alerts=true", nil)
		require.Equal(t, http.StatusOK, rec1.Code)
		require.Equal(t, http.StatusOK, rec2.Code)

		var b1, b2 store.WorkspaceExportBundle
		require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &b1))
		require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &b2))

		b1.ExportedAt = time.Time{}
		b2.ExportedAt = time.Time{}
		b1.Manifest.ExportedAt = time.Time{}
		b2.Manifest.ExportedAt = time.Time{}
		b1.APITokens = nil
		b2.APITokens = nil

		j1, err := json.Marshal(b1)
		require.NoError(t, err)
		j2, err := json.Marshal(b2)
		require.NoError(t, err)
		assert.Equal(t, string(j1), string(j2))
		assert.Equal(t, b1.Summary.TableCounts, b2.Summary.TableCounts)
	})

	// 8. Validation errors return 400 Bad Request.
	for _, badPath := range []string{
		"/workspaces/INVALID!!/export",
		"/workspace/export?workspace=INVALID!!",
		"/workspaces/alpha/export?workspace=beta",
		"/workspaces/alpha/export?include_secrets=maybe",
		"/workspaces/alpha/export?include_alerts=invalid",
		"/workspaces/alpha/export?page_size=0",
		"/workspaces/alpha/export?page_size=5001",
		"/workspaces/alpha/export?page_size=abc",
		"/workspaces/alpha/export?format=xml",
	} {
		rec := doGet("instance-operator-token", badPath, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "path %s should return 400, got %d: %s", badPath, rec.Code, rec.Body.String())
	}
}

func TestWorkspaceExport_QueryCostEstimation(t *testing.T) {
	reqConfig := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/alpha/export", nil)
	assert.Equal(t, 20.0, estimateRESTQueryCost(reqConfig))

	reqSelf := httptest.NewRequest(http.MethodGet, "/api/v1/workspace/export", nil)
	assert.Equal(t, 20.0, estimateRESTQueryCost(reqSelf))

	reqWithAlerts := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/alpha/export?include_alerts=true", nil)
	assert.Equal(t, 40.0, estimateRESTQueryCost(reqWithAlerts))

	reconciled := reconcileRESTQueryCost(reqConfig, 20.0, http.StatusOK, 4096)
	assert.Equal(t, 12.0, reconciled)
}
