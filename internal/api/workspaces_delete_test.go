package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
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

func openWorkspaceDeleteTestServer(t *testing.T, log *slog.Logger) (*Server, store.Store, *notify.Dispatcher, *notify.Factory) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "api_ws_delete.db")
	dbURL := "sqlite://" + dbPath
	require.NoError(t, store.Migrate(dbURL))
	st, err := store.NewSQLite(context.Background(), dbURL)
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })

	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	reg := rules.NewRegistry()
	factory := notify.DefaultFactory()
	disp := notify.NewDispatcher(st, factory, log).WithDigestQueue(st)
	srv := New(st, reg, factory, &fakeRPC{}, log).WithDispatcher(disp)
	return srv, st, disp, factory
}

type blockingNotifier struct {
	started   chan struct{}
	cancelled chan struct{}
	calls     atomic.Int64
}

func (b *blockingNotifier) Send(ctx context.Context, _ notify.Alert) error {
	if b.calls.Add(1) == 1 {
		close(b.started)
	}
	<-ctx.Done()
	select {
	case <-b.cancelled:
	default:
		close(b.cancelled)
	}
	return ctx.Err()
}

func TestAPIWorkspaceDeleteStopsDeliveryAndEvaluationPromptly(t *testing.T) {
	srv, st, disp, factory := openWorkspaceDeleteTestServer(t, nil)

	blocker := &blockingNotifier{
		started:   make(chan struct{}),
		cancelled: make(chan struct{}),
	}
	factory.Register("blocking", func(_ json.RawMessage) (notify.Notifier, error) {
		return blocker, nil
	})

	wsDoomed := workspace.ID("doomed")
	wsOther := workspace.ID("survivor")
	sysCtx := workspace.WithSystem(context.Background())
	doomedCtx := workspace.With(context.Background(), wsDoomed)
	otherCtx := workspace.With(context.Background(), wsOther)

	require.NoError(t, st.EnsureWorkspace(sysCtx, wsDoomed))
	require.NoError(t, st.EnsureWorkspace(sysCtx, wsOther))

	chDoomed := &store.Channel{
		Name:    "doomed-hook",
		Type:    "blocking",
		Config:  json.RawMessage(`{"url":"https://hooks.example.com/doomed-secret"}`),
		Enabled: true,
	}
	require.NoError(t, st.CreateChannel(doomedCtx, chDoomed))

	monDoomed := &store.Monitor{
		Name:        "doomed-mon",
		ContractIDs: []string{"CDOOMED"},
		Enabled:     true,
		ChannelIDs:  []int64{chDoomed.ID},
	}
	require.NoError(t, st.CreateMonitor(doomedCtx, monDoomed))
	require.NoError(t, st.SetMonitorChannels(doomedCtx, monDoomed.ID, []int64{chDoomed.ID}))

	ruleDoomed := &store.Rule{
		MonitorID: monDoomed.ID,
		Type:      "event_match",
		Params:    json.RawMessage(`{"event_name":"transfer"}`),
		Enabled:   true,
	}
	require.NoError(t, st.CreateRule(doomedCtx, ruleDoomed))

	alertDoomed := &store.Alert{
		MonitorID: monDoomed.ID,
		RuleID:    ruleDoomed.ID,
		EventID:   "evt-inflight-1",
		Payload:   json.RawMessage(`{"contract_id":"CDOOMED","event_name":"transfer"}`),
		Ledger:    500,
	}
	created, err := st.CreateAlert(doomedCtx, alertDoomed)
	require.NoError(t, err)
	require.Equal(t, store.AlertCreated, created)

	chOther := &store.Channel{
		Name:    "survivor-hook",
		Type:    "webhook",
		Config:  json.RawMessage(`{"url":"https://hooks.example.com/survivor"}`),
		Enabled: true,
	}
	require.NoError(t, st.CreateChannel(otherCtx, chOther))

	// Launch an in-flight delivery for the doomed workspace that blocks until cancelled.
	doneDispatch := make(chan struct{})
	go func() {
		defer close(doneDispatch)
		disp.Dispatch(doomedCtx, notify.Alert{
			ID:          alertDoomed.ID,
			MonitorID:   monDoomed.ID,
			MonitorName: monDoomed.Name,
			RuleID:      ruleDoomed.ID,
			RuleType:    ruleDoomed.Type,
			EventID:     alertDoomed.EventID,
			ContractID:  "CDOOMED",
			EventName:   "transfer",
			Ledger:      500,
			CreatedAt:   time.Now().UTC(),
		})
	}()

	select {
	case <-blocker.started:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for in-flight delivery to start")
	}

	router := srv.Routes()
	req := httptest.NewRequest(http.MethodDelete, "/workspaces/doomed?confirm=doomed", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var res store.WorkspaceDeletionResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))
	assert.True(t, res.Completed)
	assert.GreaterOrEqual(t, res.InFlightCancelled, 1, "in-flight delivery must be cancelled during Phase 1 stop")

	select {
	case <-blocker.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight delivery context was not cancelled promptly")
	}
	select {
	case <-doneDispatch:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight Dispatch goroutine did not exit promptly after cancellation")
	}
	assert.Equal(t, int64(1), blocker.calls.Load(), "cancelled delivery must not retry after workspace stop")

	// Other workspace remains intact.
	otherChannels, err := st.ListChannels(otherCtx, false)
	require.NoError(t, err)
	require.Len(t, otherChannels, 1)
}

func TestAPIWorkspaceDeleteDryRunConfirmationAuthorizationAndAudit(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	srv, st, _, _ := openWorkspaceDeleteTestServer(t, logger)

	authn := auth.NewBound([]auth.Binding{
		{Workspace: workspace.Default, Token: "operator-token"},
		{Workspace: workspace.ID("alpha"), Token: "alpha-admin"},
		{Workspace: workspace.ID("beta"), Token: "beta-admin"},
	}, time.Hour)
	srv.WithAuth(authn)

	sysCtx := workspace.WithSystem(context.Background())
	alphaCtx := workspace.With(context.Background(), workspace.ID("alpha"))
	betaCtx := workspace.With(context.Background(), workspace.ID("beta"))
	require.NoError(t, st.EnsureWorkspace(sysCtx, workspace.ID("alpha")))
	require.NoError(t, st.EnsureWorkspace(sysCtx, workspace.ID("beta")))

	secretWebhook := "https://hooks.slack.com/services/T000/B000/SUPER-SECRET-WEBHOOK-TOKEN"
	require.NoError(t, st.CreateChannel(alphaCtx, &store.Channel{
		Name:    "alpha-slack",
		Type:    "slack",
		Config:  json.RawMessage(`{"webhook_url":"` + secretWebhook + `"}`),
		Enabled: true,
	}))
	require.NoError(t, st.CreateMonitor(alphaCtx, &store.Monitor{
		Name:        "alpha-mon",
		ContractIDs: []string{"CALPHA"},
		Enabled:     true,
	}))
	require.NoError(t, st.CreateMonitor(betaCtx, &store.Monitor{
		Name:        "beta-mon",
		ContractIDs: []string{"CBETA"},
		Enabled:     true,
	}))

	router := srv.Routes()

	// 1. Cross-workspace authorization: alpha admin cannot dry-run or delete beta (403 Forbidden).
	req := httptest.NewRequest(http.MethodDelete, "/workspaces/beta?confirm=beta", nil)
	req.Header.Set("Authorization", "Bearer alpha-admin")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)

	// 2. Missing confirmation is rejected with 400 Bad Request and deletes nothing.
	req = httptest.NewRequest(http.MethodDelete, "/workspaces/alpha", nil)
	req.Header.Set("Authorization", "Bearer alpha-admin")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "confirm")

	// 2b. Deleting the built-in default workspace is rejected with 400 Bad Request.
	req = httptest.NewRequest(http.MethodDelete, "/workspaces/default?confirm=default", nil)
	req.Header.Set("Authorization", "Bearer operator-token")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "cannot delete the built-in default workspace")

	// 3. Dry-run returns exact counts and modifies nothing.
	req = httptest.NewRequest(http.MethodDelete, "/workspaces/alpha?dry_run=true", nil)
	req.Header.Set("Authorization", "Bearer alpha-admin")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var dryRes store.WorkspaceDeletionResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &dryRes))
	assert.True(t, dryRes.DryRun)
	assert.False(t, dryRes.Completed)
	assert.Equal(t, int64(1), dryRes.Counts.Monitors)
	assert.Equal(t, int64(1), dryRes.Counts.Channels)
	assert.Equal(t, int64(1), dryRes.Counts.Workspaces)

	// Verify alpha's monitor and channel still exist after dry-run.
	mons, err := st.ListMonitors(alphaCtx, false)
	require.NoError(t, err)
	require.Len(t, mons, 1)

	// 4. Confirmed deletion by workspace's own admin succeeds.
	req = httptest.NewRequest(http.MethodPost, "/workspaces/alpha/delete", strings.NewReader(`{"confirm":"alpha"}`))
	req.Header.Set("Authorization", "Bearer alpha-admin")
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var delRes store.WorkspaceDeletionResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &delRes))
	assert.True(t, delRes.Completed)
	assert.False(t, delRes.DryRun)
	assert.Equal(t, int64(1), delRes.Counts.Monitors)
	assert.Equal(t, int64(1), delRes.Counts.Channels)

	// 5. Status inspection via GET /workspaces/alpha/delete returns the completed tombstone.
	req = httptest.NewRequest(http.MethodGet, "/workspaces/alpha/delete", nil)
	req.Header.Set("Authorization", "Bearer operator-token")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// 6. Operator can delete any workspace (beta), returning 202 Accepted on a
	// partial batch pass when alerts remain and 200 OK once completed.
	betaMons, err := st.ListMonitors(betaCtx, false)
	require.NoError(t, err)
	require.Len(t, betaMons, 1)
	betaRule := &store.Rule{
		MonitorID: betaMons[0].ID,
		Type:      "event_match",
		Params:    json.RawMessage(`{"event_name":"transfer"}`),
		Enabled:   true,
	}
	require.NoError(t, st.CreateRule(betaCtx, betaRule))
	for _, evID := range []string{"beta-ev-1", "beta-ev-2", "beta-ev-3"} {
		out, err := st.CreateAlert(betaCtx, &store.Alert{
			MonitorID: betaMons[0].ID,
			RuleID:    betaRule.ID,
			EventID:   evID,
			Payload:   json.RawMessage(`{"contract_id":"CBETA"}`),
			Ledger:    100,
		})
		require.NoError(t, err)
		require.Equal(t, store.AlertCreated, out)
	}

	req = httptest.NewRequest(http.MethodDelete, "/workspaces/beta?confirm=beta&batch_size=1&max_alert_batches=1", nil)
	req.Header.Set("Authorization", "Bearer operator-token")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code, "partial alert batch deletion must return 202 Accepted: %s", rec.Body.String())
	var partialRes store.WorkspaceDeletionResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &partialRes))
	assert.False(t, partialRes.Completed)
	assert.Equal(t, store.WorkspaceDeletionStatusDeletingAlerts, partialRes.Status)

	req = httptest.NewRequest(http.MethodDelete, "/workspaces/beta?confirm=beta", nil)
	req.Header.Set("Authorization", "Bearer operator-token")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// 7. Audit entry is recorded and never contains channel secrets.
	audits, err := st.ListAuditEntries(sysCtx, store.AuditFilter{TargetType: "workspace"})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(audits), 2)
	for _, entry := range audits {
		assert.Equal(t, store.AuditActionDelete, entry.Action)
		assert.Equal(t, "workspace", entry.TargetType)
		assert.NotContains(t, string(entry.Diff), secretWebhook)
		assert.NotContains(t, string(entry.Diff), "SUPER-SECRET-WEBHOOK-TOKEN")
	}
	assert.NotContains(t, logBuf.String(), secretWebhook, "structured logs must never contain channel config secrets")
	assert.NotContains(t, logBuf.String(), "SUPER-SECRET-WEBHOOK-TOKEN")
	assert.Contains(t, logBuf.String(), `"workspace_id":"alpha"`)
}
