package api

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	apigql "github.com/sorotrail/sorobeacon/internal/api/graphql"
	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/broadcast"
	sbgrpc "github.com/sorotrail/sorobeacon/internal/grpc"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

type propGobCodec struct{}

func (propGobCodec) Name() string { return "sorobeacon-test-gob" }
func (propGobCodec) Marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	err := gob.NewEncoder(&b).Encode(v)
	return b.Bytes(), err
}
func (propGobCodec) Unmarshal(data []byte, v any) error {
	return gob.NewDecoder(bytes.NewReader(data)).Decode(v)
}

func init() { encoding.RegisterCodec(propGobCodec{}) }

const (
	propSharedContractID = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	propSharedLedger     = uint32(777888)
	propMissingID        = int64(999999)

	// Fake secret canaries seeded exclusively into workspace beta. Every raw
	// response body (REST, GraphQL, gRPC, SSE) returned to workspace alpha is
	// scanned for these substrings.
	canaryBetaChannelSecret = "canary-secret-beta-webhook-token-9f8e7d6c5b4a"
	canaryBetaChannelURL    = "https://hooks.beta.internal/secret-canary-9f8e7d6c5b4a"
	canaryBetaMonitorName   = "canary-beta-monitor-treasury"
	canaryBetaRuleEvent     = "canary_beta_rule_event"
	canaryBetaTemplateName  = "canary-beta-template-vault"
	canaryBetaAlertEventID  = "canary-beta-alert-ev-001"
	canaryBetaAlertPayload  = "canary-beta-alert-payload-marker"
	canaryBetaDeadLetterErr = "canary-beta-deadletter-error-marker"
	canaryBetaTokenName     = "canary-beta-token-admin"
	canaryBetaSigningSecret = "canary-beta-signing-key-secret"
)

var allBetaCanaries = []string{
	canaryBetaChannelSecret,
	canaryBetaChannelURL,
	canaryBetaMonitorName,
	canaryBetaRuleEvent,
	canaryBetaTemplateName,
	canaryBetaAlertEventID,
	canaryBetaAlertPayload,
	canaryBetaDeadLetterErr,
	canaryBetaTokenName,
	canaryBetaSigningSecret,
}

func assertNoBetaCanaries(t *testing.T, surface string, raw []byte) {
	t.Helper()
	body := string(raw)
	for _, canary := range allBetaCanaries {
		if strings.Contains(body, canary) {
			t.Fatalf("%s leaked beta canary %q in response body: %s", surface, canary, body)
		}
	}
}

type isolationFixture struct {
	st          store.Store
	broadcaster *broadcast.Broadcaster
	tokens      *auth.Manager
	authn       *auth.Authenticator
	apiServer   *Server
	router      chi.Router
	resolver    *apigql.Resolver
	grpcConn    *grpc.ClientConn

	alphaToken string
	betaToken  string

	alphaMonitorID int64
	alphaChannelID int64
	alphaRuleID    int64
	alphaAlertID   int64

	betaMonitorID        int64
	betaDeletedMonitorID int64
	betaRuleID           int64
	betaChannelID        int64
	betaActiveKeyID      string
	betaRetirableKeyID   string
	betaTemplateID       int64
	betaAlertID          int64
	betaDeadLetterID     int64
	betaTokenID          int64
	betaRotatingTokenID  int64
}

func isolationStores(t *testing.T) []struct {
	name string
	open func(t *testing.T) store.Store
} {
	t.Helper()
	backends := []struct {
		name string
		open func(t *testing.T) store.Store
	}{
		{
			name: "sqlite",
			open: func(t *testing.T) store.Store {
				t.Helper()
				dbURL := "sqlite://" + filepath.Join(t.TempDir(), "isolation.db")
				require.NoError(t, store.Migrate(dbURL))
				st, err := store.NewSQLite(context.Background(), dbURL)
				require.NoError(t, err)
				t.Cleanup(st.Close)
				return st
			},
		},
	}
	if pgURL := os.Getenv("TEST_DATABASE_URL"); pgURL != "" {
		backends = append(backends, struct {
			name string
			open func(t *testing.T) store.Store
		}{
			name: "postgres",
			open: func(t *testing.T) store.Store {
				t.Helper()
				require.NoError(t, store.Migrate(pgURL))
				st, err := store.New(context.Background(), pgURL, store.PoolSettings{}, nil)
				require.NoError(t, err)
				t.Cleanup(st.Close)
				return st
			},
		})
	}
	return backends
}

func newIsolationFixture(t *testing.T, st store.Store) *isolationFixture {
	t.Helper()
	ctx := context.Background()
	wsAlpha := workspace.ID("alpha")
	wsBeta := workspace.ID("beta")
	require.NoError(t, st.EnsureWorkspace(ctx, wsAlpha))
	require.NoError(t, st.EnsureWorkspace(ctx, wsBeta))

	alphaCtx := workspace.With(ctx, wsAlpha)
	betaCtx := workspace.With(ctx, wsBeta)

	// Webhook target for POST /channels/{id}/test so channel testing succeeds for alpha.
	webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(webhookSrv.Close)

	alphaToken := "tok-alpha-admin-secret"
	betaToken := "tok-beta-admin-secret"
	authn := auth.NewBound([]auth.Binding{
		{Workspace: wsAlpha, Token: alphaToken},
		{Workspace: wsBeta, Token: betaToken},
	}, time.Hour)
	tokenMgr := auth.NewManager(st, discardLogger())
	authn.WithTokens(tokenMgr)

	bcast := broadcast.New(32)
	apiSrv := New(st, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger()).
		WithAuth(authn).
		WithTokens(tokenMgr).
		WithBroadcaster(bcast).
		WithStreamHeartbeat(25 * time.Millisecond)
	router := apiSrv.Routes()
	resolver := apigql.NewResolver(st, discardLogger())

	// Start in-memory gRPC server over bufconn with fast stream interval.
	lis := bufconn.Listen(1 << 20)
	grpcSrv := sbgrpc.New(st, authn, discardLogger()).WithStreamInterval(25 * time.Millisecond)
	go func() { _ = grpcSrv.ServeListener(ctx, lis) }()
	t.Cleanup(func() {
		grpcSrv.Stop()
		_ = lis.Close()
	})
	grpcConn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(propGobCodec{})),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = grpcConn.Close() })

	// --- Seed overlapping data in alpha and beta ---
	// Both workspaces watch the exact same contract ID and have alerts on the same ledger.
	alphaMon := &store.Monitor{
		Name:        "shared-vault-monitor",
		ContractIDs: []string{propSharedContractID},
		Enabled:     true,
		Priority:    store.PriorityHigh,
	}
	require.NoError(t, st.CreateMonitor(alphaCtx, alphaMon))

	betaMon := &store.Monitor{
		Name:        canaryBetaMonitorName,
		ContractIDs: []string{propSharedContractID},
		Enabled:     true,
		Priority:    store.PriorityHigh,
	}
	require.NoError(t, st.CreateMonitor(betaCtx, betaMon))

	betaDeletedMon := &store.Monitor{
		Name:        canaryBetaMonitorName + "-deleted",
		ContractIDs: []string{propSharedContractID},
		Enabled:     true,
		Priority:    store.PriorityNormal,
	}
	require.NoError(t, st.CreateMonitor(betaCtx, betaDeletedMon))
	require.NoError(t, st.SoftDeleteMonitor(betaCtx, betaDeletedMon.ID))

	alphaRule := &store.Rule{
		MonitorID: alphaMon.ID,
		Type:      "event_emitted",
		Params:    json.RawMessage(`{"event_name":"transfer"}`),
		Enabled:   true,
		Severity:  store.SeverityCritical,
	}
	require.NoError(t, st.CreateRule(alphaCtx, alphaRule))

	betaRule := &store.Rule{
		MonitorID: betaMon.ID,
		Type:      "event_emitted",
		Params:    json.RawMessage(`{"event_name":"` + canaryBetaRuleEvent + `"}`),
		Enabled:   true,
		Severity:  store.SeverityCritical,
	}
	require.NoError(t, st.CreateRule(betaCtx, betaRule))

	alphaCh := &store.Channel{
		Name:        "shared-ops-webhook",
		Type:        "webhook",
		Enabled:     true,
		Config:      json.RawMessage(fmt.Sprintf(`{"url":%q,"secret":"alpha-webhook-secret"}`, webhookSrv.URL)),
		MinSeverity: store.SeverityInfo,
	}
	require.NoError(t, st.CreateChannel(alphaCtx, alphaCh))

	betaCh := &store.Channel{
		Name:        "shared-ops-webhook",
		Type:        "webhook",
		Enabled:     true,
		Config:      json.RawMessage(fmt.Sprintf(`{"url":%q,"secret":%q}`, canaryBetaChannelURL, canaryBetaChannelSecret)),
		MinSeverity: store.SeverityInfo,
	}
	require.NoError(t, st.CreateChannel(betaCtx, betaCh))

	require.NoError(t, st.SetMonitorChannels(alphaCtx, alphaMon.ID, []int64{alphaCh.ID}))
	require.NoError(t, st.SetMonitorChannels(betaCtx, betaMon.ID, []int64{betaCh.ID}))

	// Seed signing keys on beta's channel so retire/revoke can be tested.
	betaKey1 := &store.ChannelSigningKey{
		ChannelID: betaCh.ID,
		KeyID:     "k_beta_1",
		Secret:    canaryBetaSigningSecret + "-1",
		IsCurrent: true,
		Status:    store.SigningKeyStatusActive,
	}
	require.NoError(t, st.CreateChannelSigningKey(betaCtx, betaKey1))
	betaKey2 := &store.ChannelSigningKey{
		ChannelID: betaCh.ID,
		KeyID:     "k_beta_2",
		Secret:    canaryBetaSigningSecret + "-2",
		IsCurrent: true,
		Status:    store.SigningKeyStatusActive,
	}
	require.NoError(t, st.RotateChannelSigningKey(betaCtx, betaCh.ID, betaKey2))

	// Seed escalation policy on beta's monitor.
	_, err = st.SetEscalationPolicy(betaCtx, betaMon.ID, []store.EscalationStep{
		{Position: 0, DelaySeconds: 0, ChannelIDs: []int64{betaCh.ID}},
	})
	require.NoError(t, err)

	// Seed templates in alpha and beta.
	alphaTpl := &store.MonitorTemplate{
		Name:        "shared-template",
		Description: "alpha template",
		Rules:       []store.MonitorTemplateRule{{Type: "event_emitted", Params: json.RawMessage(`{"event_name":"transfer"}`)}},
	}
	require.NoError(t, st.CreateMonitorTemplate(alphaCtx, alphaTpl))

	betaTpl := &store.MonitorTemplate{
		Name:        canaryBetaTemplateName,
		Description: canaryBetaChannelSecret,
		Rules:       []store.MonitorTemplateRule{{Type: "event_emitted", Params: json.RawMessage(`{"event_name":"` + canaryBetaRuleEvent + `"}`)}},
	}
	require.NoError(t, st.CreateMonitorTemplate(betaCtx, betaTpl))

	// Seed alerts on the exact same ledger and contract in alpha and beta.
	alphaAlert := &store.Alert{
		MonitorID: alphaMon.ID,
		RuleID:    alphaRule.ID,
		EventID:   "alpha-alert-ev-001",
		Payload:   json.RawMessage(fmt.Sprintf(`{"contract_id":%q,"event_name":"transfer"}`, propSharedContractID)),
		Ledger:    propSharedLedger,
		Severity:  store.SeverityCritical,
	}
	outcome, err := st.CreateAlert(alphaCtx, alphaAlert)
	require.NoError(t, err)
	require.Equal(t, store.AlertCreated, outcome)

	betaAlert := &store.Alert{
		MonitorID: betaMon.ID,
		RuleID:    betaRule.ID,
		EventID:   canaryBetaAlertEventID,
		Payload:   json.RawMessage(fmt.Sprintf(`{"contract_id":%q,"marker":%q}`, propSharedContractID, canaryBetaAlertPayload)),
		Ledger:    propSharedLedger,
		Severity:  store.SeverityCritical,
	}
	outcome, err = st.CreateAlert(betaCtx, betaAlert)
	require.NoError(t, err)
	require.Equal(t, store.AlertCreated, outcome)
	require.NoError(t, st.MarkAlertInhibited(betaCtx, betaAlert.ID, betaRule.ID))

	// Seed delivery and dead-letter for beta's alert/channel.
	require.NoError(t, st.RecordDeliveryAttempt(betaCtx, &store.DeliveryAttempt{
		AlertID:         betaAlert.ID,
		ChannelID:       betaCh.ID,
		Status:          "failed",
		ResponseSnippet: canaryBetaDeadLetterErr,
	}))
	require.NoError(t, st.CreateDeadLetter(betaCtx, &store.DeadLetter{
		AlertID:      betaAlert.ID,
		ChannelID:    betaCh.ID,
		Cause:        "permanent",
		LastError:    canaryBetaDeadLetterErr,
		AttemptCount: 3,
		LastStatus:   500,
	}))
	betaDLs, err := st.ListDeadLetters(betaCtx, store.DeadLetterFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, betaDLs, 1)

	// Seed scoped API tokens in beta (one normal, one mid-rotation for abandon).
	_, betaTok, err := tokenMgr.Mint(betaCtx, canaryBetaTokenName, []auth.Scope{auth.ScopeMonitorsRead}, time.Time{})
	require.NoError(t, err)
	_, betaRotTok, err := tokenMgr.Mint(betaCtx, canaryBetaTokenName+"-rotating", []auth.Scope{auth.ScopeMonitorsRead}, time.Time{})
	require.NoError(t, err)
	_, _, err = tokenMgr.Rotate(betaCtx, betaRotTok.ID, canaryBetaTokenName+"-rotating", []auth.Scope{auth.ScopeMonitorsRead}, time.Hour, time.Time{})
	require.NoError(t, err)

	return &isolationFixture{
		st:                   st,
		broadcaster:          bcast,
		tokens:               tokenMgr,
		authn:                authn,
		apiServer:            apiSrv,
		router:               router,
		resolver:             resolver,
		grpcConn:             grpcConn,
		alphaToken:           alphaToken,
		betaToken:            betaToken,
		alphaMonitorID:       alphaMon.ID,
		alphaChannelID:       alphaCh.ID,
		alphaRuleID:          alphaRule.ID,
		alphaAlertID:         alphaAlert.ID,
		betaMonitorID:        betaMon.ID,
		betaDeletedMonitorID: betaDeletedMon.ID,
		betaRuleID:           betaRule.ID,
		betaChannelID:        betaCh.ID,
		betaActiveKeyID:      betaKey2.KeyID,
		betaRetirableKeyID:   betaKey1.KeyID,
		betaTemplateID:       betaTpl.ID,
		betaAlertID:          betaAlert.ID,
		betaDeadLetterID:     betaDLs[0].ID,
		betaTokenID:          betaTok.ID,
		betaRotatingTokenID:  betaRotTok.ID,
	}
}

func (fx *isolationFixture) doREST(t *testing.T, token, method, path, body string) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	fx.router.ServeHTTP(rec, req)
	raw := rec.Body.Bytes()
	if token == fx.alphaToken {
		assertNoBetaCanaries(t, method+" "+path, raw)
	}
	return rec.Code, raw
}

// restCrossWorkspaceExceptions is the explicit, documented exception list of
// REST routes that are genuinely instance-level rather than workspace-scoped.
// Adding a new route to the router fails TestWorkspaceIsolationProperty_RESTSurfaceSweep
// unless it has a workspace-isolation test builder or an entry here with a
// non-empty architectural justification.
var restCrossWorkspaceExceptions = map[string]string{
	"GET /health":     "Instance-wide health probe; reports process/DB/RPC liveness without tenant data.",
	"GET /livez":      "Instance-wide Kubernetes liveness probe; returns static ok status.",
	"GET /readyz":     "Instance-wide Kubernetes readiness probe; reports DB/RPC readiness without tenant data.",
	"GET /version":    "Instance-wide build metadata (version, commit, date); contains no workspace state.",
	"GET /poller":     "Instance-wide ledger ingestion cursor and chain lag status.",
	"GET /rule-types": "Process-wide static catalog of supported rule evaluators and their parameter schemas.",
	"POST /ingest":    "Federated alert ingestion endpoint authenticated by HMAC X-SoroBeacon-Signature rather than workspace bearer credentials.",

	// Instance-wide operational tables (documented in internal/store/postgres.go and sqlite.go):
	"GET /maintenance-windows":         "maintenance_windows is an instance-wide table; windows reference target IDs checked at creation time.",
	"POST /maintenance-windows":        "maintenance_windows is an instance-wide operational table.",
	"GET /maintenance-windows/{id}":    "maintenance_windows is an instance-wide operational table.",
	"PATCH /maintenance-windows/{id}":  "maintenance_windows is an instance-wide operational table.",
	"DELETE /maintenance-windows/{id}": "maintenance_windows is an instance-wide operational table.",

	"GET /inhibitions":                          "alert_inhibitions is an instance-wide table pairing source and target rule IDs.",
	"POST /inhibitions":                         "alert_inhibitions is an instance-wide table.",
	"DELETE /inhibitions/{sourceID}/{targetID}": "alert_inhibitions is an instance-wide table.",

	"GET /graphql/operations":         "persisted_operations is an instance-wide registry of GraphQL query documents keyed by SHA-256.",
	"POST /graphql/operations":        "persisted_operations is an instance-wide registry of GraphQL query documents.",
	"GET /graphql/operations/{id}":    "persisted_operations is an instance-wide registry of GraphQL query documents.",
	"DELETE /graphql/operations/{id}": "persisted_operations is an instance-wide registry of GraphQL query documents.",

	"GET /audit": "audit_log is an instance-wide administrative audit trail gated to RoleAdmin.",
}

// assertSameNotFoundStatus verifies that presenting workspace beta's object ID
// to an endpoint as workspace alpha fails and returns the exact same HTTP
// status code (and taxonomy code) as presenting a nonexistent object ID, so
// the surface cannot be used to probe whether an ID exists in another workspace.
func (fx *isolationFixture) assertSameNotFoundStatus(
	t *testing.T,
	routeKey string,
	method string,
	betaPath string,
	missingPath string,
	betaBody string,
	missingBody string,
) {
	t.Helper()
	betaStatus, betaRaw := fx.doREST(t, fx.alphaToken, method, betaPath, betaBody)
	missingStatus, missingRaw := fx.doREST(t, fx.alphaToken, method, missingPath, missingBody)

	if betaStatus >= 200 && betaStatus < 300 {
		t.Fatalf("%s: cross-workspace request to %s succeeded with status %d (body: %s)", routeKey, betaPath, betaStatus, string(betaRaw))
	}
	assert.Equalf(t, missingStatus, betaStatus,
		"%s: cross-workspace ID (%s -> %d) must return the same status as a missing ID (%s -> %d) to prevent existence probing",
		routeKey, betaPath, betaStatus, missingPath, missingStatus)

	var betaEnv, missingEnv map[string]any
	if json.Unmarshal(betaRaw, &betaEnv) == nil && json.Unmarshal(missingRaw, &missingEnv) == nil {
		assert.Equalf(t, missingEnv["code"], betaEnv["code"],
			"%s: cross-workspace error code (%v) must match missing-ID error code (%v)",
			routeKey, betaEnv["code"], missingEnv["code"])
	}
}

func buildRESTIsolationChecks(fx *isolationFixture) map[string]func(t *testing.T) {
	idStr := func(id int64) string { return strconv.FormatInt(id, 10) }
	betaMon := idStr(fx.betaMonitorID)
	betaDelMon := idStr(fx.betaDeletedMonitorID)
	betaRule := idStr(fx.betaRuleID)
	betaCh := idStr(fx.betaChannelID)
	betaKey := fx.betaActiveKeyID
	betaRetireKey := fx.betaRetirableKeyID
	betaTpl := idStr(fx.betaTemplateID)
	betaAlert := idStr(fx.betaAlertID)
	betaDL := idStr(fx.betaDeadLetterID)
	betaTok := idStr(fx.betaTokenID)
	betaRotTok := idStr(fx.betaRotatingTokenID)
	missing := idStr(propMissingID)

	return map[string]func(t *testing.T){
		// --- Monitors ---
		"GET /monitors": func(t *testing.T) {
			code, raw := fx.doREST(t, fx.alphaToken, http.MethodGet, "/monitors", "")
			require.Equal(t, http.StatusOK, code)
			assert.Contains(t, string(raw), "shared-vault-monitor")
		},
		"POST /monitors": func(t *testing.T) {
			// Creating a monitor in alpha attempting to attach beta's channel ID must not attach beta's channel.
			code, raw := fx.doREST(t, fx.alphaToken, http.MethodPost, "/monitors",
				fmt.Sprintf(`{"name":"alpha-new-mon","contract_ids":[%q],"enabled":true}`, propSharedContractID))
			require.Equal(t, http.StatusCreated, code, string(raw))
		},
		"POST /monitors/bulk": func(t *testing.T) {
			// Bulk update targeting beta's monitor ID must affect 0 rows and report beta's ID in unknown_ids, matching a missing ID.
			codeBeta, betaRaw := fx.doREST(t, fx.alphaToken, http.MethodPost, "/monitors/bulk",
				fmt.Sprintf(`{"ids":[%d],"enabled":false}`, fx.betaMonitorID))
			codeMiss, _ := fx.doREST(t, fx.alphaToken, http.MethodPost, "/monitors/bulk",
				fmt.Sprintf(`{"ids":[%d],"enabled":false}`, propMissingID))
			require.Equal(t, http.StatusOK, codeBeta)
			require.Equal(t, codeMiss, codeBeta)
			assert.JSONEq(t, fmt.Sprintf(`{"enabled":false,"unknown_ids":[%d],"updated":0}`, fx.betaMonitorID), string(betaRaw))
		},
		"POST /monitors/import": func(t *testing.T) {
			code, raw := fx.doREST(t, fx.alphaToken, http.MethodPost, "/monitors/import",
				fmt.Sprintf(`{"name":"alpha-imported","contract_ids":[%q]}`, propSharedContractID))
			require.Contains(t, []int{http.StatusOK, http.StatusCreated}, code, string(raw))
		},
		"GET /monitors/trash": func(t *testing.T) {
			code, raw := fx.doREST(t, fx.alphaToken, http.MethodGet, "/monitors/trash", "")
			require.Equal(t, http.StatusOK, code)
			assert.NotContains(t, string(raw), idStr(fx.betaDeletedMonitorID))
		},
		"GET /monitors/{id}": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "GET /monitors/{id}", http.MethodGet,
				"/monitors/"+betaMon, "/monitors/"+missing, "", "")
		},
		"GET /monitors/{id}/stats": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "GET /monitors/{id}/stats", http.MethodGet,
				"/monitors/"+betaMon+"/stats", "/monitors/"+missing+"/stats", "", "")
		},
		"PATCH /monitors/{id}": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "PATCH /monitors/{id}", http.MethodPatch,
				"/monitors/"+betaMon, "/monitors/"+missing, `{"name":"hijacked"}`, `{"name":"hijacked"}`)
		},
		"DELETE /monitors/{id}": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "DELETE /monitors/{id}", http.MethodDelete,
				"/monitors/"+betaMon, "/monitors/"+missing, "", "")
		},
		"POST /monitors/{id}/soft-delete": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "POST /monitors/{id}/soft-delete", http.MethodPost,
				"/monitors/"+betaMon+"/soft-delete", "/monitors/"+missing+"/soft-delete", "", "")
		},
		"POST /monitors/{id}/restore": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "POST /monitors/{id}/restore", http.MethodPost,
				"/monitors/"+betaDelMon+"/restore", "/monitors/"+missing+"/restore", "", "")
		},
		"POST /monitors/{id}/duplicate": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "POST /monitors/{id}/duplicate", http.MethodPost,
				"/monitors/"+betaMon+"/duplicate", "/monitors/"+missing+"/duplicate", "", "")
		},
		"POST /monitors/{id}/rules/dry-run": func(t *testing.T) {
			body := `{"type":"event_emitted","params":{"event_name":"transfer"}}`
			fx.assertSameNotFoundStatus(t, "POST /monitors/{id}/rules/dry-run", http.MethodPost,
				"/monitors/"+betaMon+"/rules/dry-run", "/monitors/"+missing+"/rules/dry-run", body, body)
		},
		"POST /monitors/{id}/rules": func(t *testing.T) {
			body := `{"type":"event_emitted","params":{"event_name":"transfer"},"enabled":true}`
			fx.assertSameNotFoundStatus(t, "POST /monitors/{id}/rules", http.MethodPost,
				"/monitors/"+betaMon+"/rules", "/monitors/"+missing+"/rules", body, body)
		},
		"GET /monitors/{id}/rules": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "GET /monitors/{id}/rules", http.MethodGet,
				"/monitors/"+betaMon+"/rules", "/monitors/"+missing+"/rules", "", "")
		},
		"POST /monitors/{id}/rules/bulk": func(t *testing.T) {
			body := `{"rules":[{"type":"event_emitted","params":{"event_name":"transfer"},"enabled":true}]}`
			fx.assertSameNotFoundStatus(t, "POST /monitors/{id}/rules/bulk", http.MethodPost,
				"/monitors/"+betaMon+"/rules/bulk", "/monitors/"+missing+"/rules/bulk", body, body)
		},
		"PATCH /monitors/{id}/rules/{ruleID}": func(t *testing.T) {
			// Trying to patch beta's rule via beta's monitor OR via alpha's own monitor must 404 identically to missing ID.
			fx.assertSameNotFoundStatus(t, "PATCH /monitors/{id}/rules/{ruleID}", http.MethodPatch,
				"/monitors/"+betaMon+"/rules/"+betaRule, "/monitors/"+missing+"/rules/"+missing,
				`{"enabled":false}`, `{"enabled":false}`)
			fx.assertSameNotFoundStatus(t, "PATCH /monitors/{alpha}/rules/{betaRule}", http.MethodPatch,
				"/monitors/"+idStr(fx.alphaMonitorID)+"/rules/"+betaRule,
				"/monitors/"+idStr(fx.alphaMonitorID)+"/rules/"+missing,
				`{"enabled":false}`, `{"enabled":false}`)
		},
		"DELETE /monitors/{id}/rules/{ruleID}": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "DELETE /monitors/{id}/rules/{ruleID}", http.MethodDelete,
				"/monitors/"+betaMon+"/rules/"+betaRule, "/monitors/"+missing+"/rules/"+missing, "", "")
			fx.assertSameNotFoundStatus(t, "DELETE /monitors/{alpha}/rules/{betaRule}", http.MethodDelete,
				"/monitors/"+idStr(fx.alphaMonitorID)+"/rules/"+betaRule,
				"/monitors/"+idStr(fx.alphaMonitorID)+"/rules/"+missing, "", "")
		},
		"GET /monitors/{id}/escalation": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "GET /monitors/{id}/escalation", http.MethodGet,
				"/monitors/"+betaMon+"/escalation", "/monitors/"+missing+"/escalation", "", "")
		},
		"PUT /monitors/{id}/escalation": func(t *testing.T) {
			body := fmt.Sprintf(`{"steps":[{"delay":"0s","channel_ids":[%d]}]}`, fx.alphaChannelID)
			fx.assertSameNotFoundStatus(t, "PUT /monitors/{id}/escalation", http.MethodPut,
				"/monitors/"+betaMon+"/escalation", "/monitors/"+missing+"/escalation", body, body)
		},
		"DELETE /monitors/{id}/escalation": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "DELETE /monitors/{id}/escalation", http.MethodDelete,
				"/monitors/"+betaMon+"/escalation", "/monitors/"+missing+"/escalation", "", "")
		},

		// --- Channels (including Channel.Config secret canary checks) ---
		"GET /channels": func(t *testing.T) {
			code, raw := fx.doREST(t, fx.alphaToken, http.MethodGet, "/channels", "")
			require.Equal(t, http.StatusOK, code)
			assert.Contains(t, string(raw), "shared-ops-webhook")
		},
		"POST /channels": func(t *testing.T) {
			code, raw := fx.doREST(t, fx.alphaToken, http.MethodPost, "/channels",
				`{"name":"alpha-extra-ch","type":"webhook","enabled":true,"config":{"url":"https://example.com/hook","secret":"alpha-hook-secret"}}`)
			require.Equal(t, http.StatusCreated, code, string(raw))
		},
		"POST /channels/bulk": func(t *testing.T) {
			codeBeta, betaRaw := fx.doREST(t, fx.alphaToken, http.MethodPost, "/channels/bulk",
				fmt.Sprintf(`{"ids":[%d],"enabled":false}`, fx.betaChannelID))
			codeMiss, _ := fx.doREST(t, fx.alphaToken, http.MethodPost, "/channels/bulk",
				fmt.Sprintf(`{"ids":[%d],"enabled":false}`, propMissingID))
			require.Equal(t, http.StatusOK, codeBeta)
			require.Equal(t, codeMiss, codeBeta)
			assert.JSONEq(t, fmt.Sprintf(`{"enabled":false,"unknown_ids":[%d],"updated":0}`, fx.betaChannelID), string(betaRaw))
		},
		"POST /channels/{id}/duplicate": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "POST /channels/{id}/duplicate", http.MethodPost,
				"/channels/"+betaCh+"/duplicate", "/channels/"+missing+"/duplicate", "", "")
		},
		"GET /channels/{id}": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "GET /channels/{id}", http.MethodGet,
				"/channels/"+betaCh, "/channels/"+missing, "", "")
		},
		"GET /channels/{id}/stats": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "GET /channels/{id}/stats", http.MethodGet,
				"/channels/"+betaCh+"/stats", "/channels/"+missing+"/stats", "", "")
		},
		"PATCH /channels/{id}": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "PATCH /channels/{id}", http.MethodPatch,
				"/channels/"+betaCh, "/channels/"+missing, `{"name":"hijacked"}`, `{"name":"hijacked"}`)
		},
		"DELETE /channels/{id}": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "DELETE /channels/{id}", http.MethodDelete,
				"/channels/"+betaCh, "/channels/"+missing, "", "")
		},
		"POST /channels/{id}/test": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "POST /channels/{id}/test", http.MethodPost,
				"/channels/"+betaCh+"/test", "/channels/"+missing+"/test", "", "")
		},
		"GET /channels/{id}/keys": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "GET /channels/{id}/keys", http.MethodGet,
				"/channels/"+betaCh+"/keys", "/channels/"+missing+"/keys", "", "")
		},
		"POST /channels/{id}/keys/rotate": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "POST /channels/{id}/keys/rotate", http.MethodPost,
				"/channels/"+betaCh+"/keys/rotate", "/channels/"+missing+"/keys/rotate", "", "")
		},
		"POST /channels/{id}/keys/{keyID}/retire": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "POST /channels/{id}/keys/{keyID}/retire", http.MethodPost,
				"/channels/"+betaCh+"/keys/"+betaRetireKey+"/retire",
				"/channels/"+missing+"/keys/"+missing+"/retire", "", "")
			fx.assertSameNotFoundStatus(t, "POST /channels/{alpha}/keys/{betaKey}/retire", http.MethodPost,
				"/channels/"+idStr(fx.alphaChannelID)+"/keys/"+betaRetireKey+"/retire",
				"/channels/"+idStr(fx.alphaChannelID)+"/keys/"+missing+"/retire", "", "")
		},
		"POST /channels/{id}/keys/{keyID}/revoke": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "POST /channels/{id}/keys/{keyID}/revoke", http.MethodPost,
				"/channels/"+betaCh+"/keys/"+betaKey+"/revoke",
				"/channels/"+missing+"/keys/"+missing+"/revoke", "", "")
			fx.assertSameNotFoundStatus(t, "POST /channels/{alpha}/keys/{betaKey}/revoke", http.MethodPost,
				"/channels/"+idStr(fx.alphaChannelID)+"/keys/"+betaKey+"/revoke",
				"/channels/"+idStr(fx.alphaChannelID)+"/keys/"+missing+"/revoke", "", "")
		},

		// --- Templates ---
		"GET /templates": func(t *testing.T) {
			code, raw := fx.doREST(t, fx.alphaToken, http.MethodGet, "/templates", "")
			require.Equal(t, http.StatusOK, code)
			assert.Contains(t, string(raw), "shared-template")
		},
		"POST /templates": func(t *testing.T) {
			code, _ := fx.doREST(t, fx.alphaToken, http.MethodPost, "/templates",
				`{"name":"alpha-created-tpl","rules":[{"type":"event_emitted","params":{"event_name":"transfer"},"enabled":true}]}`)
			require.Equal(t, http.StatusCreated, code)
		},
		"GET /templates/{id}": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "GET /templates/{id}", http.MethodGet,
				"/templates/"+betaTpl, "/templates/"+missing, "", "")
		},
		"PATCH /templates/{id}": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "PATCH /templates/{id}", http.MethodPatch,
				"/templates/"+betaTpl, "/templates/"+missing, `{"name":"hijacked"}`, `{"name":"hijacked"}`)
		},
		"DELETE /templates/{id}": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "DELETE /templates/{id}", http.MethodDelete,
				"/templates/"+betaTpl, "/templates/"+missing, "", "")
		},
		"POST /templates/{id}/instantiate": func(t *testing.T) {
			body := fmt.Sprintf(`{"name":"from-tpl","contract_ids":[%q]}`, propSharedContractID)
			fx.assertSameNotFoundStatus(t, "POST /templates/{id}/instantiate", http.MethodPost,
				"/templates/"+betaTpl+"/instantiate", "/templates/"+missing+"/instantiate", body, body)
		},
		"POST /templates/{id}/instantiate/bulk": func(t *testing.T) {
			body := fmt.Sprintf(`{"monitors":[{"name":"from-tpl-bulk","contract_ids":[%q]}]}`, propSharedContractID)
			fx.assertSameNotFoundStatus(t, "POST /templates/{id}/instantiate/bulk", http.MethodPost,
				"/templates/"+betaTpl+"/instantiate/bulk", "/templates/"+missing+"/instantiate/bulk", body, body)
		},

		// --- Alerts, Exports, Deliveries, Triggers, Stream ---
		"GET /alerts": func(t *testing.T) {
			code, raw := fx.doREST(t, fx.alphaToken, http.MethodGet, "/alerts?contract_id="+propSharedContractID, "")
			require.Equal(t, http.StatusOK, code)
			assert.Contains(t, string(raw), "alpha-alert-ev-001")
			// Filtering by beta's monitor_id or rule_id as alpha must return empty.
			code, raw = fx.doREST(t, fx.alphaToken, http.MethodGet, "/alerts?monitor_id="+betaMon, "")
			require.Equal(t, http.StatusOK, code)
			assert.NotContains(t, string(raw), "alpha-alert-ev-001")
		},
		"GET /alerts.csv": func(t *testing.T) {
			code, raw := fx.doREST(t, fx.alphaToken, http.MethodGet, "/alerts.csv", "")
			require.Equal(t, http.StatusOK, code)
			assert.Contains(t, string(raw), "alpha-alert-ev-001")
		},
		"GET /alerts/export": func(t *testing.T) {
			code, raw := fx.doREST(t, fx.alphaToken, http.MethodGet, "/alerts/export", "")
			require.Equal(t, http.StatusOK, code)
			assert.Contains(t, string(raw), "alpha-alert-ev-001")
		},
		"GET /alerts/stream": func(t *testing.T) {
			// Subscribing to beta's monitor_id must return the same 404 as a missing monitor_id.
			fx.assertSameNotFoundStatus(t, "GET /alerts/stream?monitor_id", http.MethodGet,
				"/alerts/stream?monitor_id="+betaMon, "/alerts/stream?monitor_id="+missing, "", "")
		},
		"GET /alerts/{id}/deliveries": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "GET /alerts/{id}/deliveries", http.MethodGet,
				"/alerts/"+betaAlert+"/deliveries", "/alerts/"+missing+"/deliveries", "", "")
		},
		"POST /alerts/{id}/deliveries/{channelID}/retry": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "POST /alerts/{id}/deliveries/{channelID}/retry", http.MethodPost,
				"/alerts/"+betaAlert+"/deliveries/"+betaCh+"/retry",
				"/alerts/"+missing+"/deliveries/"+missing+"/retry", "", "")
		},
		"POST /alerts/{id}/acknowledge": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "POST /alerts/{id}/acknowledge", http.MethodPost,
				"/alerts/"+betaAlert+"/acknowledge", "/alerts/"+missing+"/acknowledge", "", "")
		},
		"POST /triggers": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "POST /triggers", http.MethodPost,
				"/triggers", "/triggers",
				fmt.Sprintf(`{"monitor_id":%d}`, fx.betaMonitorID),
				fmt.Sprintf(`{"monitor_id":%d}`, propMissingID))
		},

		// --- Dead letters ---
		"GET /dead-letters": func(t *testing.T) {
			code, raw := fx.doREST(t, fx.alphaToken, http.MethodGet, "/dead-letters", "")
			require.Equal(t, http.StatusOK, code)
			assert.NotContains(t, string(raw), idStr(fx.betaDeadLetterID))
		},
		"GET /dead-letters/groups": func(t *testing.T) {
			code, _ := fx.doREST(t, fx.alphaToken, http.MethodGet, "/dead-letters/groups?group_by=channel_id", "")
			require.Equal(t, http.StatusOK, code)
		},
		"POST /dead-letters/{id}/redrive": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "POST /dead-letters/{id}/redrive", http.MethodPost,
				"/dead-letters/"+betaDL+"/redrive", "/dead-letters/"+missing+"/redrive", "", "")
		},
		"POST /dead-letters/{id}/discard": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "POST /dead-letters/{id}/discard", http.MethodPost,
				"/dead-letters/"+betaDL+"/discard", "/dead-letters/"+missing+"/discard", "", "")
		},
		"POST /dead-letters/bulk/redrive": func(t *testing.T) {
			_, betaRaw := fx.doREST(t, fx.alphaToken, http.MethodPost, "/dead-letters/bulk/redrive",
				fmt.Sprintf(`{"ids":[%d]}`, fx.betaDeadLetterID))
			_, missRaw := fx.doREST(t, fx.alphaToken, http.MethodPost, "/dead-letters/bulk/redrive",
				fmt.Sprintf(`{"ids":[%d]}`, propMissingID))
			assert.JSONEq(t, string(missRaw), string(betaRaw))
		},
		"POST /dead-letters/bulk/discard": func(t *testing.T) {
			_, betaRaw := fx.doREST(t, fx.alphaToken, http.MethodPost, "/dead-letters/bulk/discard",
				fmt.Sprintf(`{"ids":[%d]}`, fx.betaDeadLetterID))
			_, missRaw := fx.doREST(t, fx.alphaToken, http.MethodPost, "/dead-letters/bulk/discard",
				fmt.Sprintf(`{"ids":[%d]}`, propMissingID))
			assert.JSONEq(t, string(missRaw), string(betaRaw))
		},

		// --- Tokens ---
		"GET /tokens": func(t *testing.T) {
			code, _ := fx.doREST(t, fx.alphaToken, http.MethodGet, "/tokens", "")
			require.Equal(t, http.StatusOK, code)
		},
		"POST /tokens": func(t *testing.T) {
			code, _ := fx.doREST(t, fx.alphaToken, http.MethodPost, "/tokens",
				`{"name":"alpha-minted-tok","role":"viewer","scopes":["monitors:read"]}`)
			require.Equal(t, http.StatusCreated, code)
		},
		"GET /tokens/{id}": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "GET /tokens/{id}", http.MethodGet,
				"/tokens/"+betaTok, "/tokens/"+missing, "", "")
		},
		"POST /tokens/{id}/rotate": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "POST /tokens/{id}/rotate", http.MethodPost,
				"/tokens/"+betaTok+"/rotate", "/tokens/"+missing+"/rotate", `{"overlap":"1h"}`, `{"overlap":"1h"}`)
		},
		"POST /tokens/{id}/abandon": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "POST /tokens/{id}/abandon", http.MethodPost,
				"/tokens/"+betaRotTok+"/abandon", "/tokens/"+missing+"/abandon", "", "")
		},
		"POST /tokens/{id}/revoke": func(t *testing.T) {
			fx.assertSameNotFoundStatus(t, "POST /tokens/{id}/revoke", http.MethodPost,
				"/tokens/"+betaTok+"/revoke", "/tokens/"+missing+"/revoke", "", "")
		},

		// --- Stats ---
		"GET /stats": func(t *testing.T) {
			code, raw := fx.doREST(t, fx.alphaToken, http.MethodGet, "/stats", "")
			require.Equal(t, http.StatusOK, code)
			var st store.Stats
			require.NoError(t, json.Unmarshal(raw, &st))
			assert.Equal(t, int64(1), st.Alerts, "stats.alerts must count only alpha's alerts, never beta's")
		},
		"GET /stats/alerts-daily": func(t *testing.T) {
			code, raw := fx.doREST(t, fx.alphaToken, http.MethodGet, "/stats/alerts-daily", "")
			require.Equal(t, http.StatusOK, code)
			var resp alertsDailyResponse
			require.NoError(t, json.Unmarshal(raw, &resp))
			var total int64
			for _, d := range resp.Days {
				total += d.Count
			}
			assert.Equal(t, int64(1), total, "alerts-daily must count only alpha's alerts, never beta's")
		},
	}
}

func TestWorkspaceIsolationProperty_RESTSurfaceSweep(t *testing.T) {
	for _, backend := range isolationStores(t) {
		t.Run(backend.name, func(t *testing.T) {
			fx := newIsolationFixture(t, backend.open(t))
			checks := buildRESTIsolationChecks(fx)

			// 1. Dynamically walk the Chi router to discover every registered route.
			var discovered []string
			err := chi.Walk(fx.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
				clean := strings.TrimRight(route, "/")
				if clean == "" {
					clean = "/"
				}
				discovered = append(discovered, method+" "+clean)
				return nil
			})
			require.NoError(t, err)
			require.NotEmpty(t, discovered)

			registered := make(map[string]bool, len(discovered))
			for _, key := range discovered {
				registered[key] = true
				_, hasCheck := checks[key]
				reason, isException := restCrossWorkspaceExceptions[key]
				if hasCheck && isException {
					t.Fatalf("route %q appears in both buildRESTIsolationChecks and restCrossWorkspaceExceptions", key)
				}
				if !hasCheck && !isException {
					t.Fatalf("route %q is registered on the router but has neither a workspace-isolation test builder nor an entry in restCrossWorkspaceExceptions", key)
				}
				if isException {
					require.NotEmpty(t, strings.TrimSpace(reason), "exception route %q must state a non-empty reason", key)
				}
			}

			for key := range checks {
				require.Truef(t, registered[key], "buildRESTIsolationChecks contains stale route %q not registered on the router", key)
			}
			for key := range restCrossWorkspaceExceptions {
				require.Truef(t, registered[key], "restCrossWorkspaceExceptions contains stale route %q not registered on the router", key)
			}

			// 2. Run every workspace-scoped route check.
			for _, key := range discovered {
				if check, ok := checks[key]; ok {
					t.Run(key, check)
				}
			}

			// 3. Verify beta's state is 100% intact after all alpha write/delete/mutation attempts.
			betaCtx := workspace.With(context.Background(), workspace.ID("beta"))
			mon, err := fx.st.GetMonitor(betaCtx, fx.betaMonitorID)
			require.NoError(t, err, "beta monitor must survive alpha mutation attempts")
			assert.Equal(t, canaryBetaMonitorName, mon.Name)

			ch, err := fx.st.GetChannel(betaCtx, fx.betaChannelID)
			require.NoError(t, err, "beta channel must survive alpha mutation attempts")
			assert.Contains(t, string(ch.Config), canaryBetaChannelSecret)

			rulesList, err := fx.st.ListRules(betaCtx, fx.betaMonitorID, false)
			require.NoError(t, err)
			require.Len(t, rulesList, 1)
			assert.True(t, rulesList[0].Enabled)

			pol, err := fx.st.GetEscalationPolicyForMonitor(betaCtx, fx.betaMonitorID)
			require.NoError(t, err, "beta escalation policy must survive alpha DELETE attempt")
			require.Len(t, pol.Steps, 1)

			tpl, err := fx.st.GetMonitorTemplate(betaCtx, fx.betaTemplateID)
			require.NoError(t, err, "beta template must survive alpha DELETE attempt")
			assert.Equal(t, canaryBetaTemplateName, tpl.Name)

			al, err := fx.st.GetAlert(betaCtx, fx.betaAlertID)
			require.NoError(t, err)
			assert.Nil(t, al.AcknowledgedAt, "beta alert must not be acknowledged by alpha")

			dls, err := fx.st.ListDeadLetters(betaCtx, store.DeadLetterFilter{Limit: 10})
			require.NoError(t, err)
			require.Len(t, dls, 1, "beta dead-letter must survive alpha discard/redrive attempts")

			tok, err := fx.tokens.Get(betaCtx, fx.betaTokenID)
			require.NoError(t, err)
			assert.True(t, tok.RevokedAt.IsZero(), "beta token must not be revoked by alpha")
		})
	}
}

func TestWorkspaceIsolationProperty_GraphQLSurfaceSweep(t *testing.T) {
	src, err := os.ReadFile("graphql/schema.graphqls")
	require.NoError(t, err)
	schema, err := gqlparser.LoadSchema(&ast.Source{Name: "schema.graphqls", Input: string(src)})
	require.NoError(t, err)
	require.NotNil(t, schema.Query)
	require.Nil(t, schema.Mutation, "GraphQL Mutation added without workspace-isolation sweep coverage")
	require.Nil(t, schema.Subscription, "GraphQL Subscription added without workspace-isolation sweep coverage")

	for _, backend := range isolationStores(t) {
		t.Run(backend.name, func(t *testing.T) {
			fx := newIsolationFixture(t, backend.open(t))
			alphaCtx := workspace.With(context.Background(), workspace.ID("alpha"))
			betaMonIDStr := strconv.FormatInt(fx.betaMonitorID, 10)
			betaRuleIDStr := strconv.FormatInt(fx.betaRuleID, 10)
			betaAlertIDStr := strconv.FormatInt(fx.betaAlertID, 10)
			missingIDStr := strconv.FormatInt(propMissingID, 10)

			queryChecks := map[string]func(t *testing.T){
				"monitor": func(t *testing.T) {
					betaGot, betaErr := fx.resolver.Query().Monitor(alphaCtx, betaMonIDStr)
					missGot, missErr := fx.resolver.Query().Monitor(alphaCtx, missingIDStr)
					require.NoError(t, betaErr)
					require.NoError(t, missErr)
					assert.Nil(t, betaGot, "Query.monitor for beta ID must return nil matching missing ID")
					assert.Equal(t, missGot, betaGot)

					got, err := fx.resolver.Query().Monitor(alphaCtx, strconv.FormatInt(fx.alphaMonitorID, 10))
					require.NoError(t, err)
					require.NotNil(t, got)
					raw, _ := json.Marshal(got)
					assertNoBetaCanaries(t, "Query.monitor", raw)
				},
				"monitors": func(t *testing.T) {
					conn, err := fx.resolver.Query().Monitors(alphaCtx, nil, nil, nil, nil, nil)
					require.NoError(t, err)
					raw, _ := json.Marshal(conn)
					assertNoBetaCanaries(t, "Query.monitors", raw)
					require.Len(t, conn.Edges, 1)
				},
				"alert": func(t *testing.T) {
					betaGot, betaErr := fx.resolver.Query().Alert(alphaCtx, betaAlertIDStr)
					missGot, missErr := fx.resolver.Query().Alert(alphaCtx, missingIDStr)
					require.NoError(t, betaErr)
					require.NoError(t, missErr)
					assert.Nil(t, betaGot, "Query.alert for beta ID must return nil matching missing ID")
					assert.Equal(t, missGot, betaGot)

					got, err := fx.resolver.Query().Alert(alphaCtx, strconv.FormatInt(fx.alphaAlertID, 10))
					require.NoError(t, err)
					require.NotNil(t, got)
					raw, _ := json.Marshal(got)
					assertNoBetaCanaries(t, "Query.alert", raw)
				},
				"alerts": func(t *testing.T) {
					sharedContract := propSharedContractID
					conn, err := fx.resolver.Query().Alerts(alphaCtx, nil, nil, &sharedContract, nil, nil, nil, nil, nil, nil)
					require.NoError(t, err)
					raw, _ := json.Marshal(conn)
					assertNoBetaCanaries(t, "Query.alerts(sharedContract)", raw)
					require.Len(t, conn.Edges, 1)

					// Guessing beta's monitorId or ruleId returns empty, never beta's alerts.
					connByBetaMon, err := fx.resolver.Query().Alerts(alphaCtx, &betaMonIDStr, &betaRuleIDStr, nil, nil, nil, nil, nil, nil, nil)
					require.NoError(t, err)
					assert.Empty(t, connByBetaMon.Edges)
				},
				"stats": func(t *testing.T) {
					st, err := fx.resolver.Query().Stats(alphaCtx)
					require.NoError(t, err)
					raw, _ := json.Marshal(st)
					assertNoBetaCanaries(t, "Query.stats", raw)
					assert.Equal(t, 1, st.Monitors)
					assert.Equal(t, 1, st.Alerts)
					assert.Equal(t, 1, st.Channels)
				},
				"alertSeries": func(t *testing.T) {
					series, err := fx.resolver.Query().AlertSeries(alphaCtx, nil)
					require.NoError(t, err)
					raw, _ := json.Marshal(series)
					assertNoBetaCanaries(t, "Query.alertSeries", raw)
					total := 0
					for _, d := range series {
						total += d.Count
					}
					assert.Equal(t, 1, total)
				},
			}

			for _, f := range schema.Query.Fields {
				if strings.HasPrefix(f.Name, "__") {
					continue
				}
				check, ok := queryChecks[f.Name]
				if !ok {
					t.Fatalf("schema.graphqls declares Query.%s without a workspace-isolation check", f.Name)
				}
				t.Run("Query."+f.Name, check)
			}

			// Nested resolver fields on Monitor and Alert:
			t.Run("Monitor.rules", func(t *testing.T) {
				rulesOut, err := fx.resolver.Monitor().Rules(alphaCtx, &apigql.Monitor{ID: betaMonIDStr})
				require.NoError(t, err)
				assert.Empty(t, rulesOut, "Monitor.rules for beta's monitor ID under alpha context must be empty")
			})
			t.Run("Monitor.channels", func(t *testing.T) {
				chOut, err := fx.resolver.Monitor().Channels(alphaCtx, &apigql.Monitor{ID: betaMonIDStr})
				require.NoError(t, err)
				assert.Empty(t, chOut, "Monitor.channels for beta's monitor ID under alpha context must be empty")

				alphaChOut, err := fx.resolver.Monitor().Channels(alphaCtx, &apigql.Monitor{ID: strconv.FormatInt(fx.alphaMonitorID, 10)})
				require.NoError(t, err)
				raw, _ := json.Marshal(alphaChOut)
				assertNoBetaCanaries(t, "Monitor.channels(alpha)", raw)
			})
			t.Run("Alert.inhibitedByRule", func(t *testing.T) {
				ruleOut, err := fx.resolver.Alert().InhibitedByRule(alphaCtx, &apigql.Alert{
					ID:                strconv.FormatInt(fx.alphaAlertID, 10),
					MonitorID:         strconv.FormatInt(fx.alphaMonitorID, 10),
					InhibitedByRuleID: &betaRuleIDStr,
				})
				require.NoError(t, err)
				assert.Nil(t, ruleOut, "Alert.inhibitedByRule must not resolve a rule belonging to beta")
			})
		})
	}
}

func TestWorkspaceIsolationProperty_GRPCSurfaceSweep(t *testing.T) {
	desc := sbgrpc.ServiceDesc()
	require.NotEmpty(t, desc.Methods)
	require.NotEmpty(t, desc.Streams)

	for _, backend := range isolationStores(t) {
		t.Run(backend.name, func(t *testing.T) {
			fx := newIsolationFixture(t, backend.open(t))
			alphaCtx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+fx.alphaToken))

			unaryChecks := map[string]func(t *testing.T){
				"GetMonitor": func(t *testing.T) {
					var betaResp, missResp sbgrpc.MonitorProto
					betaErr := fx.grpcConn.Invoke(alphaCtx, "/sorobeacon.v1.MonitorService/GetMonitor",
						&sbgrpc.GetMonitorRequest{ID: fx.betaMonitorID}, &betaResp)
					missErr := fx.grpcConn.Invoke(alphaCtx, "/sorobeacon.v1.MonitorService/GetMonitor",
						&sbgrpc.GetMonitorRequest{ID: propMissingID}, &missResp)
					require.Equal(t, codes.NotFound, status.Code(betaErr))
					require.Equal(t, status.Code(missErr), status.Code(betaErr))

					var alphaResp sbgrpc.MonitorProto
					require.NoError(t, fx.grpcConn.Invoke(alphaCtx, "/sorobeacon.v1.MonitorService/GetMonitor",
						&sbgrpc.GetMonitorRequest{ID: fx.alphaMonitorID}, &alphaResp))
					raw, _ := json.Marshal(alphaResp)
					assertNoBetaCanaries(t, "gRPC GetMonitor", raw)
				},
				"ListMonitors": func(t *testing.T) {
					var resp sbgrpc.ListMonitorsResponse
					require.NoError(t, fx.grpcConn.Invoke(alphaCtx, "/sorobeacon.v1.MonitorService/ListMonitors",
						&sbgrpc.ListMonitorsRequest{Limit: 50}, &resp))
					raw, _ := json.Marshal(resp)
					assertNoBetaCanaries(t, "gRPC ListMonitors", raw)
					require.Len(t, resp.Monitors, 1)
					assert.Equal(t, fx.alphaMonitorID, resp.Monitors[0].ID)
				},
				"ListAlerts": func(t *testing.T) {
					var resp sbgrpc.ListAlertsResponse
					require.NoError(t, fx.grpcConn.Invoke(alphaCtx, "/sorobeacon.v1.MonitorService/ListAlerts",
						&sbgrpc.ListAlertsRequest{Limit: 50}, &resp))
					raw, _ := json.Marshal(resp)
					assertNoBetaCanaries(t, "gRPC ListAlerts", raw)
					require.Len(t, resp.Alerts, 1)
					assert.Equal(t, fx.alphaAlertID, resp.Alerts[0].ID)

					// Guessing beta's MonitorID or RuleID returns empty.
					var byBeta sbgrpc.ListAlertsResponse
					require.NoError(t, fx.grpcConn.Invoke(alphaCtx, "/sorobeacon.v1.MonitorService/ListAlerts",
						&sbgrpc.ListAlertsRequest{MonitorID: fx.betaMonitorID, RuleID: fx.betaRuleID, Limit: 50}, &byBeta))
					assert.Empty(t, byBeta.Alerts)
				},
			}

			streamChecks := map[string]func(t *testing.T){
				"StreamAlerts": func(t *testing.T) {
					// Subscribing to beta's MonitorID must fail with NotFound matching a missing MonitorID.
					descStream := desc.Streams[0]
					openStream := func(monID int64) error {
						ctx, cancel := context.WithTimeout(alphaCtx, 2*time.Second)
						defer cancel()
						st, err := fx.grpcConn.NewStream(ctx, &descStream, "/sorobeacon.v1.MonitorService/StreamAlerts")
						require.NoError(t, err)
						require.NoError(t, st.SendMsg(&sbgrpc.StreamAlertsRequest{MonitorID: monID}))
						require.NoError(t, st.CloseSend())
						return st.RecvMsg(&sbgrpc.AlertProto{})
					}
					betaErr := openStream(fx.betaMonitorID)
					missErr := openStream(propMissingID)
					require.Equal(t, codes.NotFound, status.Code(betaErr))
					require.Equal(t, status.Code(missErr), status.Code(betaErr))
				},
			}

			for _, m := range desc.Methods {
				check, ok := unaryChecks[m.MethodName]
				if !ok {
					t.Fatalf("gRPC ServiceDesc declares unary method %q without a workspace-isolation check", m.MethodName)
				}
				t.Run("Unary."+m.MethodName, check)
			}
			for _, s := range desc.Streams {
				check, ok := streamChecks[s.StreamName]
				if !ok {
					t.Fatalf("gRPC ServiceDesc declares stream %q without a workspace-isolation check", s.StreamName)
				}
				t.Run("Stream."+s.StreamName, check)
			}
		})
	}
}

func TestWorkspaceIsolationProperty_StreamingAndMidStreamRevocation(t *testing.T) {
	for _, backend := range isolationStores(t) {
		t.Run(backend.name, func(t *testing.T) {
			fx := newIsolationFixture(t, backend.open(t))
			alphaCtx := workspace.With(context.Background(), workspace.ID("alpha"))
			betaCtx := workspace.With(context.Background(), workspace.ID("beta"))

			// Mint a database-backed token in workspace alpha so we can revoke it mid-stream.
			secret, minted, err := fx.tokens.Mint(alphaCtx, "alpha-stream-watcher", []auth.Scope{auth.ScopeAlertsRead, auth.ScopeMonitorsRead}, time.Time{})
			require.NoError(t, err)

			// 1. SSE /alerts/stream stays scoped to alpha and stops delivering when revoked mid-stream.
			t.Run("SSE_AlertsStream", func(t *testing.T) {
				httpSrv := httptest.NewServer(fx.router)
				defer httpSrv.Close()

				reqCtx, cancel := context.WithCancel(context.Background())
				defer cancel()
				req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, httpSrv.URL+"/alerts/stream", nil)
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer "+secret)

				res, err := http.DefaultClient.Do(req)
				require.NoError(t, err)
				defer res.Body.Close()
				require.Equal(t, http.StatusOK, res.StatusCode)

				lines := streamReader(res)
				awaitLine(t, lines, time.Second, func(l string) bool { return strings.HasPrefix(l, ":") })
				waitForSubscribers(t, fx.broadcaster, 1)

				// Publish a beta alert followed by an alpha alert; only alpha's alert may arrive.
				fx.broadcaster.Publish(broadcast.Alert{
					ID:          9001,
					MonitorID:   fx.betaMonitorID,
					MonitorName: canaryBetaMonitorName,
					RuleID:      fx.betaRuleID,
					EventID:     canaryBetaAlertEventID,
					Payload:     json.RawMessage(`{"secret":"` + canaryBetaChannelSecret + `"}`),
					CreatedAt:   time.Now().UTC(),
				})
				fx.broadcaster.Publish(broadcast.Alert{
					ID:          9002,
					MonitorID:   fx.alphaMonitorID,
					MonitorName: "shared-vault-monitor",
					RuleID:      fx.alphaRuleID,
					EventID:     "alpha-live-ev-1",
					Payload:     json.RawMessage(`{"event_name":"transfer"}`),
					CreatedAt:   time.Now().UTC(),
				})

				dataLine := awaitLine(t, lines, 2*time.Second, func(l string) bool { return strings.HasPrefix(l, "data: ") })
				assertNoBetaCanaries(t, "SSE /alerts/stream", []byte(dataLine))
				assert.Contains(t, dataLine, "alpha-live-ev-1")

				// Revoke the token mid-stream and publish another alpha alert; the stream must close without delivering it.
				code, _ := fx.doREST(t, fx.alphaToken, http.MethodPost, fmt.Sprintf("/tokens/%d/revoke", minted.ID), "")
				require.Equal(t, http.StatusNoContent, code)

				fx.broadcaster.Publish(broadcast.Alert{
					ID:          9003,
					MonitorID:   fx.alphaMonitorID,
					MonitorName: "shared-vault-monitor",
					RuleID:      fx.alphaRuleID,
					EventID:     "alpha-after-revoke",
					Payload:     json.RawMessage(`{"event_name":"transfer"}`),
					CreatedAt:   time.Now().UTC(),
				})

				deadline := time.After(2 * time.Second)
				for {
					select {
					case line, ok := <-lines:
						if !ok {
							return // Stream closed cleanly after revocation!
						}
						if strings.Contains(line, "alpha-after-revoke") {
							t.Fatalf("revoked token received post-revocation alert line: %s", line)
						}
					case <-deadline:
						t.Fatal("SSE stream did not close after token revocation")
					}
				}
			})

			// 2. gRPC StreamAlerts stays scoped to alpha and stops delivering with Unauthenticated when revoked mid-stream.
			t.Run("gRPC_StreamAlerts", func(t *testing.T) {
				grpcSecret, grpcTok, err := fx.tokens.Mint(alphaCtx, "alpha-grpc-stream-watcher", []auth.Scope{auth.ScopeAlertsRead, auth.ScopeMonitorsRead}, time.Time{})
				require.NoError(t, err)

				streamCtx, cancel := context.WithTimeout(
					metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+grpcSecret)),
					4*time.Second,
				)
				defer cancel()

				desc := sbgrpc.ServiceDesc()
				st, err := fx.grpcConn.NewStream(streamCtx, &desc.Streams[0], "/sorobeacon.v1.MonitorService/StreamAlerts")
				require.NoError(t, err)
				require.NoError(t, st.SendMsg(&sbgrpc.StreamAlertsRequest{}))
				require.NoError(t, st.CloseSend())

				// Wait briefly for StreamAlerts to seed lastID, then create a beta alert and an alpha alert.
				time.Sleep(40 * time.Millisecond)
				_, err = fx.st.CreateAlert(betaCtx, &store.Alert{
					MonitorID: fx.betaMonitorID,
					RuleID:    fx.betaRuleID,
					EventID:   canaryBetaAlertEventID + "-stream",
					Payload:   json.RawMessage(`{"secret":"` + canaryBetaChannelSecret + `"}`),
					Ledger:    propSharedLedger + 1,
				})
				require.NoError(t, err)
				_, err = fx.st.CreateAlert(alphaCtx, &store.Alert{
					MonitorID: fx.alphaMonitorID,
					RuleID:    fx.alphaRuleID,
					EventID:   "alpha-grpc-live-1",
					Payload:   json.RawMessage(`{"event_name":"transfer"}`),
					Ledger:    propSharedLedger + 1,
				})
				require.NoError(t, err)

				var got sbgrpc.AlertProto
				require.NoError(t, st.RecvMsg(&got))
				raw, _ := json.Marshal(got)
				assertNoBetaCanaries(t, "gRPC StreamAlerts", raw)
				assert.Equal(t, "alpha-grpc-live-1", got.EventID)

				// Revoke the token mid-stream; the next poll tick must terminate the stream with Unauthenticated.
				require.NoError(t, fx.tokens.Revoke(alphaCtx, grpcTok.ID))
				_, err = fx.st.CreateAlert(alphaCtx, &store.Alert{
					MonitorID: fx.alphaMonitorID,
					RuleID:    fx.alphaRuleID,
					EventID:   "alpha-grpc-after-revoke",
					Payload:   json.RawMessage(`{"event_name":"transfer"}`),
					Ledger:    propSharedLedger + 2,
				})
				require.NoError(t, err)

				var afterRevoke sbgrpc.AlertProto
				recvErr := st.RecvMsg(&afterRevoke)
				require.Error(t, recvErr, "gRPC stream must stop after token revocation")
				assert.Equal(t, codes.Unauthenticated, status.Code(recvErr))
				assert.NotEqual(t, "alpha-grpc-after-revoke", afterRevoke.EventID)
			})
		})
	}
}
