package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"

	"github.com/sorotrail/sorobeacon/internal/auth"
	sbgrpc "github.com/sorotrail/sorobeacon/internal/grpc"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// auditFakeStore records the entries the middleware writes and serves a
// canned list, without needing a database.
type auditFakeStore struct {
	store.Store
	created    []*store.AuditEntry
	entries    []store.AuditEntry
	listErr    error
	lastFilter store.AuditFilter
}

func (f *auditFakeStore) CreateAuditEntry(_ context.Context, e *store.AuditEntry) error {
	e.ID = int64(len(f.created) + 1)
	f.created = append(f.created, e)
	return nil
}

func (f *auditFakeStore) ListAuditEntries(_ context.Context, fl store.AuditFilter) ([]store.AuditEntry, error) {
	f.lastFilter = fl
	return f.entries, f.listErr
}

func auditTestRouter(fs *auditFakeStore) chi.Router {
	r := chi.NewRouter()
	r.Use(AuditMiddleware(fs, discardLogger()))
	r.Route("/channels", func(r chi.Router) {
		r.Post("/", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusCreated, map[string]any{"id": 7})
		})
		r.Patch("/{id}", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"id": r.PathValue("id")})
		})
		r.Delete("/{id}", func(w http.ResponseWriter, _ *http.Request) {
			writeNoContent(w)
		})
		// Not a configuration change: must not be audited.
		r.Post("/{id}/test", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"status": "sent"})
		})
	})
	r.Route("/monitors", func(r chi.Router) {
		r.Post("/", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusCreated, map[string]any{"id": 3})
		})
		r.Route("/{id}/rules", func(r chi.Router) {
			r.Post("/", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusCreated, map[string]any{"id": 9})
			})
		})
	})
	return r
}

// secretConfig is the kind of body a channel write carries. Its value must
// never appear in an audit entry.
const secretConfig = `{"name":"ops","config":{"webhook_url":"https://hooks.example/SUPER-SECRET-TOKEN"}}`

func TestAuditMiddlewareRecordsOneEntryPerMutation(t *testing.T) {
	fs := &auditFakeStore{}
	r := auditTestRouter(fs)

	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	if rec := post("/channels/", secretConfig); rec.Code != http.StatusCreated {
		t.Fatalf("create channel = %d", rec.Code)
	}
	if rec := post("/monitors/", `{"name":"m"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create monitor = %d", rec.Code)
	}
	if rec := post("/monitors/3/rules/", `{"type":"event_emitted"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create rule = %d", rec.Code)
	}
	// A channel test-send is not a config change.
	post("/channels/1/test", `{}`)

	req := httptest.NewRequest(http.MethodPatch, "/channels/1", strings.NewReader(`{"config":{"webhook_url":"x"}}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update channel = %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodDelete, "/channels/1", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete channel = %d", rec.Code)
	}

	if len(fs.created) != 5 {
		t.Fatalf("recorded %d entries, want 5: %+v", len(fs.created), fs.created)
	}
	want := []struct {
		action, target string
		id             int64
	}{
		{store.AuditActionCreate, "channel", 7},
		{store.AuditActionCreate, "monitor", 3},
		{store.AuditActionCreate, "rule", 9},
		{store.AuditActionUpdate, "channel", 1},
		{store.AuditActionDelete, "channel", 1},
	}
	for i, w := range want {
		got := fs.created[i]
		if got.Action != w.action || got.TargetType != w.target || got.TargetID != w.id {
			t.Errorf("entry %d = (%s %s %d), want (%s %s %d)",
				i, got.Action, got.TargetType, got.TargetID, w.action, w.target, w.id)
		}
		if got.Actor == "" {
			t.Errorf("entry %d has no actor (request id)", i)
		}
		if got.Outcome != store.AuditOutcomeSucceeded {
			t.Errorf("entry %d outcome = %q, want %q", i, got.Outcome, store.AuditOutcomeSucceeded)
		}
		if got.Surface != store.AuditSurfaceREST {
			t.Errorf("entry %d surface = %q, want %q", i, got.Surface, store.AuditSurfaceREST)
		}
	}
	// The secret in the request body must never reach the log.
	for _, e := range fs.created {
		if strings.Contains(string(e.Diff), "SUPER-SECRET-TOKEN") {
			t.Fatalf("audit diff leaked a secret: %s", e.Diff)
		}
	}
	// The create entry records the field names, not their values.
	if !strings.Contains(string(fs.created[0].Diff), "config") {
		t.Errorf("create diff = %s, want the config field name recorded", fs.created[0].Diff)
	}
}

func TestListAudit(t *testing.T) {
	fs := &auditFakeStore{entries: []store.AuditEntry{{ID: 2, Action: store.AuditActionCreate, TargetType: "channel"}}}
	s := &Server{store: fs, log: discardLogger()}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/audit?target_type=channel&target_id=3&outcome=succeeded&actor_type=automation&surface=rest&action=create&resource_id=k1&limit=5")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /audit = %d", res.StatusCode)
	}
	if fs.lastFilter.TargetType != "channel" || fs.lastFilter.TargetID != 3 || fs.lastFilter.Limit != 5 ||
		fs.lastFilter.Outcome != "succeeded" || fs.lastFilter.ActorType != "automation" ||
		fs.lastFilter.Surface != "rest" || fs.lastFilter.Action != "create" || fs.lastFilter.ResourceID != "k1" {
		t.Fatalf("filter = %+v", fs.lastFilter)
	}
}

func TestListAuditRejectsBadFilters(t *testing.T) {
	s := &Server{store: &auditFakeStore{}, log: discardLogger()}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	// "alert" is deliberately absent: an acknowledgement writes an alert
	// entry, so the type is valid now and a widget is what is left to reject.
	for _, q := range []string{
		"target_type=widget",
		"target_id=x",
		"outcome=maybe",
		"actor_type=robot",
		"surface=cli",
		"from=yesterday",
		"to=nope",
		"limit=0",
		"cursor=zero",
	} {
		res, err := http.Get(srv.URL + "/audit?" + q)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("GET /audit?%s = %d, want 400", q, res.StatusCode)
		}
	}
}

// verifyRESTAuditCoverage checks that every mutating route in routes is
// present in either table or exempt (mutually exclusive) and that neither
// contains stale routes. Extracted so the test can also prove it fails when an
// unmapped mutating route is added.
func verifyRESTAuditCoverage(routes []route, table *auth.RouteTable[AuditRouteSpec], exempt map[string]string) []string {
	var errs []string
	tablePatterns := make(map[string]bool)
	for _, k := range table.Patterns() {
		tablePatterns[k] = true
	}
	registeredMutating := make(map[string]bool)
	for _, rt := range routes {
		if !mutatingMethod(rt.Method) {
			continue
		}
		key := rt.Method + " " + rt.Path
		registeredMutating[key] = true

		inTable := tablePatterns[key]
		reason, inExempt := exempt[key]
		if inTable && inExempt {
			errs = append(errs, "route "+key+" appears in both AuditRouteTable and AuditExemptRESTRoutes")
		}
		if !inTable && !inExempt {
			errs = append(errs, "mutating route "+key+" is missing from AuditRouteTable and AuditExemptRESTRoutes")
		}
		if inTable {
			spec, ok := table.Lookup(rt.Method, rt.Path)
			if !ok || spec.Action == "" || !store.ValidAuditTargetType(spec.TargetType) {
				errs = append(errs, "route "+key+" has invalid AuditRouteSpec")
			}
		}
		if inExempt && strings.TrimSpace(reason) == "" {
			errs = append(errs, "exempt route "+key+" must state a non-empty reason")
		}
	}
	for _, key := range table.Patterns() {
		if !registeredMutating[key] {
			errs = append(errs, "AuditRouteTable names stale or non-mutating route "+key)
		}
	}
	for key := range exempt {
		if !registeredMutating[key] {
			errs = append(errs, "AuditExemptRESTRoutes names stale or non-mutating route "+key)
		}
	}
	return errs
}

// TestAuditRouteTableCoversEveryMutatingRoute walks the REST router and fails
// when a mutating endpoint is added without being wired into AuditRouteTable
// (or explicitly exempted in AuditExemptRESTRoutes with a documented reason).
func TestAuditRouteTableCoversEveryMutatingRoute(t *testing.T) {
	routes := walkedRoutes(t)
	require.GreaterOrEqual(t, len(routes), 25)

	errs := verifyRESTAuditCoverage(routes, AuditRouteTable, AuditExemptRESTRoutes)
	require.Empty(t, errs, "REST router audit coverage violations:\n%s", strings.Join(errs, "\n"))

	// Mutation check: adding an unmapped mutating endpoint must fail the check.
	withUnmapped := append(append([]route(nil), routes...), route{Method: http.MethodPost, Path: "/unmapped-admin-endpoint"})
	mutErrs := verifyRESTAuditCoverage(withUnmapped, AuditRouteTable, AuditExemptRESTRoutes)
	require.NotEmpty(t, mutErrs, "coverage guard must fail when an unmapped mutating endpoint is registered")
	assert.Contains(t, mutErrs[0], "POST /unmapped-admin-endpoint")
}

// TestAuditGraphQLSchemaCoversEveryMutationField parses the GraphQL schema and
// verifies that any GraphQL Mutation field is registered in AuditGraphQLMutationTable
// or AuditExemptGraphQLMutations.
func TestAuditGraphQLSchemaCoversEveryMutationField(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("graphql", "schema.graphqls"))
	require.NoError(t, err)

	schema, gqlErr := gqlparser.LoadSchema(&ast.Source{Name: "schema.graphqls", Input: string(raw)})
	require.Nil(t, gqlErr)

	registered := make(map[string]bool)
	if schema.Mutation != nil {
		for _, field := range schema.Mutation.Fields {
			if strings.HasPrefix(field.Name, "__") {
				continue
			}
			registered[field.Name] = true
			_, inTable := AuditGraphQLMutationTable[field.Name]
			reason, inExempt := AuditExemptGraphQLMutations[field.Name]
			if inTable && inExempt {
				t.Errorf("GraphQL mutation %q appears in both AuditGraphQLMutationTable and AuditExemptGraphQLMutations", field.Name)
			}
			if !inTable && !inExempt {
				t.Errorf("GraphQL mutation %q is missing from AuditGraphQLMutationTable and AuditExemptGraphQLMutations", field.Name)
			}
			if inExempt && strings.TrimSpace(reason) == "" {
				t.Errorf("exempt GraphQL mutation %q must state a non-empty reason", field.Name)
			}
		}
	}
	for name := range AuditGraphQLMutationTable {
		if !registered[name] {
			t.Errorf("AuditGraphQLMutationTable contains stale GraphQL mutation %q", name)
		}
	}
	for name := range AuditExemptGraphQLMutations {
		if !registered[name] {
			t.Errorf("AuditExemptGraphQLMutations contains stale GraphQL mutation %q", name)
		}
	}
}

// TestAuditGRPCServiceDescCoversEveryRPC walks the gRPC ServiceDesc and
// verifies that every unary and streaming RPC is accounted for in either
// AuditGRPCMethodTable or AuditExemptGRPCMethods.
func TestAuditGRPCServiceDescCoversEveryRPC(t *testing.T) {
	desc := sbgrpc.ServiceDesc()
	registered := make(map[string]bool)
	for _, m := range desc.Methods {
		registered[m.MethodName] = true
	}
	for _, s := range desc.Streams {
		registered[s.StreamName] = true
	}
	require.NotEmpty(t, registered)

	for name := range registered {
		_, inTable := sbgrpc.AuditGRPCMethodTable[name]
		reason, inExempt := sbgrpc.AuditExemptGRPCMethods[name]
		if inTable && inExempt {
			t.Errorf("gRPC method %q appears in both AuditGRPCMethodTable and AuditExemptGRPCMethods", name)
		}
		if !inTable && !inExempt {
			t.Errorf("gRPC method %q is missing from AuditGRPCMethodTable and AuditExemptGRPCMethods", name)
		}
		if inExempt && strings.TrimSpace(reason) == "" {
			t.Errorf("exempt gRPC method %q must state a non-empty reason", name)
		}
	}
	for name := range sbgrpc.AuditGRPCMethodTable {
		if !registered[name] {
			t.Errorf("AuditGRPCMethodTable contains stale gRPC method %q", name)
		}
	}
	for name := range sbgrpc.AuditExemptGRPCMethods {
		if !registered[name] {
			t.Errorf("AuditExemptGRPCMethods contains stale gRPC method %q", name)
		}
	}
}

// TestAdminActionAuditEndToEnd_SucceededFailedSecretsActorTypeAndWorkspaceScoping
// verifies on a real SQLite store:
// 1. Succeeded and failed administrative actions (401, 403, 404, 400) are recorded with
//    actor, actor_type (person vs automation vs anonymous), action, target_type, target_id,
//    resource_id, workspace_id, source_ip, surface, outcome, status_code, reason, and
//    tamper-evident hash chain (prev_hash, record_hash).
// 2. Rejected attempt to revoke another workspace's token is recorded as a failed audit entry.
// 3. Channel config / token secrets never appear in audit records.
// 4. Workspace scoping: a tenant sees only its own audit records, while an operator
//    (cross-workspace static admin token with ?all_workspaces=true or ?workspace=beta) sees all.
func TestAdminActionAuditEndToEnd_SucceededFailedSecretsActorTypeAndWorkspaceScoping(t *testing.T) {
	dbURL := "sqlite://" + filepath.Join(t.TempDir(), "audit-e2e.db")
	require.NoError(t, store.Migrate(dbURL))
	st, err := store.NewSQLite(context.Background(), dbURL)
	require.NoError(t, err)
	t.Cleanup(st.Close)

	ctx := context.Background()
	wsAlpha := workspace.ID("alpha")
	wsBeta := workspace.ID("beta")
	require.NoError(t, st.EnsureWorkspace(ctx, wsAlpha))
	require.NoError(t, st.EnsureWorkspace(ctx, wsBeta))

	const (
		operatorToken = "static-operator-admin-token"
		alphaAdminTok = "alpha-admin-tok"
		alphaViewTok  = "alpha-viewer-tok"
		betaAdminTok  = "beta-admin-tok"
	)

	authn := auth.NewBound([]auth.Binding{
		{Workspace: workspace.Default, Token: operatorToken + ":admin"},
		{Workspace: wsAlpha, Token: alphaAdminTok + ":admin"},
		{Workspace: wsAlpha, Token: alphaViewTok + ":viewer"},
		{Workspace: wsBeta, Token: betaAdminTok + ":admin"},
	}, time.Hour)
	tokenMgr := auth.NewManager(st, discardLogger())
	authn.WithTokens(tokenMgr)

	// Mint a dashboard session for alpha to verify person actor_type vs automation token actor_type.
	sessID, ok := authn.Login(alphaAdminTok)
	require.True(t, ok)

	// Mint a token in beta so alpha can attempt to revoke beta's token and get a 404 recorded.
	betaCtx := workspace.With(ctx, wsBeta)
	_, betaMintedTok, err := tokenMgr.Mint(betaCtx, "beta-ci-token", []auth.Scope{auth.ScopeMonitorsRead}, time.Time{})
	require.NoError(t, err)

	apiSrv := New(st, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger()).
		WithAuth(authn).
		WithTokens(tokenMgr)
	srv := httptest.NewServer(apiSrv.Routes())
	t.Cleanup(srv.Close)

	// 1. Automation actor (API bearer token) creates a channel with secret config in alpha.
	const chSecret = "https://hooks.slack.com/services/T000/B000/ULTRA-SECRET-WEBHOOK-KEY"
	res, raw := do(t, http.MethodPost, srv.URL+"/channels",
		`{"name":"alpha-slack","type":"slack","config":{"webhook_url":"`+chSecret+`"}}`,
		map[string]string{"Authorization": "Bearer " + alphaAdminTok})
	require.Equal(t, http.StatusCreated, res.StatusCode, string(raw))

	// 2. Person actor (dashboard session cookie) creates a monitor in alpha.
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/monitors",
		strings.NewReader(`{"name":"alpha-mon","contract_ids":["`+validContract+`"],"enabled":true}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: sessID})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	// 3. Rejected attempt by alpha to revoke beta's token (404 Not Found).
	res, _ = do(t, http.MethodPost, srv.URL+"/tokens/"+itoa(betaMintedTok.ID)+"/revoke", "",
		map[string]string{"Authorization": "Bearer " + alphaAdminTok})
	require.Equal(t, http.StatusNotFound, res.StatusCode)

	// 4. Rejected attempt by a viewer token in alpha to delete a monitor (403 Forbidden).
	res, _ = do(t, http.MethodDelete, srv.URL+"/monitors/1", "",
		map[string]string{"Authorization": "Bearer " + alphaViewTok})
	require.Equal(t, http.StatusForbidden, res.StatusCode)

	// 5. Rejected unauthenticated attempt to revoke a token (401 Unauthorized) -> recorded in default workspace as anonymous.
	res, _ = do(t, http.MethodPost, srv.URL+"/tokens/"+itoa(betaMintedTok.ID)+"/revoke", "", nil)
	require.Equal(t, http.StatusUnauthorized, res.StatusCode)

	// 6. Beta admin creates a monitor in beta.
	res, raw = do(t, http.MethodPost, srv.URL+"/monitors",
		`{"name":"beta-mon","contract_ids":["`+validContract+`"],"enabled":true}`,
		map[string]string{"Authorization": "Bearer " + betaAdminTok})
	require.Equal(t, http.StatusCreated, res.StatusCode, string(raw))

	// Verify alpha's audit entries via GET /audit:
	res, raw = do(t, http.MethodGet, srv.URL+"/audit", "",
		map[string]string{"Authorization": "Bearer " + alphaAdminTok})
	require.Equal(t, http.StatusOK, res.StatusCode, string(raw))
	require.NotContains(t, string(raw), chSecret, "channel secret must never appear in audit records")

	var alphaList struct {
		Entries []store.AuditEntry `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(raw, &alphaList))
	require.Len(t, alphaList.Entries, 4, "alpha should have 4 audit entries (2 succeeded + 2 failed)")

	// Entries are newest-first:
	// [0]: 403 DELETE /monitors/1 (failed)
	// [1]: 404 POST /tokens/{id}/revoke (failed attempt to revoke beta's token)
	// [2]: 201 POST /monitors (person actor via session cookie)
	// [3]: 201 POST /channels (automation actor via bearer token)
	eForbidden := alphaList.Entries[0]
	assert.Equal(t, string(wsAlpha), eForbidden.WorkspaceID)
	assert.Equal(t, store.AuditActionDelete, eForbidden.Action)
	assert.Equal(t, store.AuditTargetMonitor, eForbidden.TargetType)
	assert.Equal(t, int64(1), eForbidden.TargetID)
	assert.Equal(t, store.AuditOutcomeFailed, eForbidden.Outcome)
	assert.Equal(t, http.StatusForbidden, eForbidden.StatusCode)
	assert.NotEmpty(t, eForbidden.Reason)
	assert.NotEmpty(t, eForbidden.SourceIP)
	assert.Equal(t, store.AuditSurfaceREST, eForbidden.Surface)

	eRevokeOther := alphaList.Entries[1]
	assert.Equal(t, string(wsAlpha), eRevokeOther.WorkspaceID)
	assert.Equal(t, store.AuditActionDelete, eRevokeOther.Action)
	assert.Equal(t, store.AuditTargetToken, eRevokeOther.TargetType)
	assert.Equal(t, betaMintedTok.ID, eRevokeOther.TargetID)
	assert.Equal(t, store.AuditOutcomeFailed, eRevokeOther.Outcome)
	assert.Equal(t, http.StatusNotFound, eRevokeOther.StatusCode)
	assert.Contains(t, eRevokeOther.Reason, "not found")

	ePersonMon := alphaList.Entries[2]
	assert.Equal(t, store.AuditActorPerson, ePersonMon.ActorType)
	assert.Equal(t, store.AuditOutcomeSucceeded, ePersonMon.Outcome)
	assert.Equal(t, http.StatusCreated, ePersonMon.StatusCode)

	eAutoChan := alphaList.Entries[3]
	assert.Equal(t, store.AuditActorAutomation, eAutoChan.ActorType)
	assert.Equal(t, store.AuditOutcomeSucceeded, eAutoChan.Outcome)
	assert.Equal(t, http.StatusCreated, eAutoChan.StatusCode)

	// Verify tamper-evident hash chain for alpha's entries.
	chainOK, brokenID, err := store.VerifyAuditHashChain(alphaList.Entries)
	require.NoError(t, err)
	require.True(t, chainOK, "broken at entry %d", brokenID)

	// Filter by outcome=failed on GET /audit:
	res, raw = do(t, http.MethodGet, srv.URL+"/audit?outcome=failed", "",
		map[string]string{"Authorization": "Bearer " + alphaAdminTok})
	require.Equal(t, http.StatusOK, res.StatusCode)
	var failedOnly struct {
		Entries []store.AuditEntry `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(raw, &failedOnly))
	require.Len(t, failedOnly.Entries, 2)

	// Tenant workspace cannot query beta's audit log:
	res, _ = do(t, http.MethodGet, srv.URL+"/audit?workspace=beta", "",
		map[string]string{"Authorization": "Bearer " + alphaAdminTok})
	require.Equal(t, http.StatusForbidden, res.StatusCode)

	// Operator (default-workspace static admin token) can query a specific workspace or all workspaces:
	res, raw = do(t, http.MethodGet, srv.URL+"/audit?workspace=beta", "",
		map[string]string{"Authorization": "Bearer " + operatorToken})
	require.Equal(t, http.StatusOK, res.StatusCode, string(raw))
	var betaViaOp struct {
		Entries []store.AuditEntry `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(raw, &betaViaOp))
	require.Len(t, betaViaOp.Entries, 1)
	assert.Equal(t, string(wsBeta), betaViaOp.Entries[0].WorkspaceID)

	res, raw = do(t, http.MethodGet, srv.URL+"/audit?all_workspaces=true", "",
		map[string]string{"Authorization": "Bearer " + operatorToken})
	require.Equal(t, http.StatusOK, res.StatusCode, string(raw))
	var allViaOp struct {
		Entries []store.AuditEntry `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(raw, &allViaOp))
	// 4 in alpha + 1 in beta + 1 unauthenticated 401 in default = 6 total
	require.Len(t, allViaOp.Entries, 6)
}

func TestAllowUnauthenticatedAuditWrite_RateLimits401BurstWhileRecording403(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	const ip = "203.0.113.99"

	unauthAuditMu.Lock()
	delete(unauthAuditBuckets, ip)
	unauthAuditMu.Unlock()

	for i := 0; i < 50; i++ {
		require.True(t, allowUnauthenticatedAuditWrite(ip, now), "burst request %d should be allowed", i+1)
	}
	require.False(t, allowUnauthenticatedAuditWrite(ip, now), "51st unauthenticated 401 audit write in burst must be rate-limited")

	// After 200ms at 5 RPS refill, 1 token is replenished.
	require.True(t, allowUnauthenticatedAuditWrite(ip, now.Add(200*time.Millisecond)), "refilled token should allow 1 write")
	require.False(t, allowUnauthenticatedAuditWrite(ip, now.Add(200*time.Millisecond)), "subsequent immediate write should be rate-limited again")

	// End-to-end verification: when >50 unauthenticated 401 requests arrive from the same source_ip,
	// only 50 unauthenticated 401 audit entries are persisted, whereas an authenticated 403 from the
	// same source_ip is still recorded even when the unauthenticated 401 bucket is exhausted.
	dbURL := "sqlite://" + filepath.Join(t.TempDir(), "audit-ratelimit.db")
	require.NoError(t, store.Migrate(dbURL))
	st, err := store.NewSQLite(context.Background(), dbURL)
	require.NoError(t, err)
	t.Cleanup(st.Close)

	const (
		adminTok  = "ratelimit-admin-tok"
		viewerTok = "ratelimit-viewer-tok"
	)
	authn := auth.NewBound([]auth.Binding{
		{Workspace: workspace.Default, Token: adminTok + ":admin"},
		{Workspace: workspace.Default, Token: viewerTok + ":viewer"},
	}, time.Hour)

	apiSrv := New(st, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger()).
		WithAuth(authn)
	srv := httptest.NewServer(apiSrv.Routes())
	t.Cleanup(srv.Close)

	unauthAuditMu.Lock()
	unauthAuditBuckets = make(map[string]*unauthAuditBucket)
	unauthAuditMu.Unlock()

	for i := 0; i < 60; i++ {
		res, _ := do(t, http.MethodDelete, srv.URL+"/monitors/1", "", nil)
		require.Equal(t, http.StatusUnauthorized, res.StatusCode)
	}

	// Send an authenticated 403 Forbidden from the same client after the 401 burst is exhausted.
	res, _ := do(t, http.MethodDelete, srv.URL+"/monitors/1", "",
		map[string]string{"Authorization": "Bearer " + viewerTok})
	require.Equal(t, http.StatusForbidden, res.StatusCode)

	entries, err := st.ListAuditEntries(context.Background(), store.AuditFilter{Limit: 200})
	require.NoError(t, err)
	// 50 rate-limited 401 burst entries (+ up to 2 refilled tokens under -race) + 1 authenticated 403 entry.
	require.GreaterOrEqual(t, len(entries), 51)
	require.LessOrEqual(t, len(entries), 53)
	assert.Equal(t, http.StatusForbidden, entries[0].StatusCode)
	assert.Equal(t, http.StatusUnauthorized, entries[1].StatusCode)
}
