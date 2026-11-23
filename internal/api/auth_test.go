package api

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/oidctest"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

const (
	testToken    = "s3cret-token"
	rotatedToken = "rotated-token"
)

// authServer builds an API router with the given tokens configured. No
// tokens is the unconfigured case: open access, as before authentication
// existed.
func authServer(tokens ...string) chi.Router {
	return authServerOn(&fakeStore{}, auth.New(tokens, 0))
}

// authServerOn builds a router over a caller-supplied store and
// authenticator, so a session minted by the dashboard can be presented to
// the API — which is exactly how the cookie reaches it in production.
func authServerOn(st store.Store, a *auth.Authenticator) chi.Router {
	s := New(st, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger()).WithAuth(a)
	return s.Routes()
}

func get(t *testing.T, h http.Handler, path string, header map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	return res.Result()
}

func TestAuthAllowsValidToken(t *testing.T) {
	// The scheme is case-insensitive and surrounding whitespace is trimmed,
	// so a header pasted from documentation or a shell script still works.
	for _, header := range []string{"Bearer " + testToken, "bearer " + testToken, "Bearer  " + testToken, "Bearer " + testToken + " "} {
		res := get(t, authServer(testToken), "/version", map[string]string{"Authorization": header})
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%q = %d, want 200", header, res.StatusCode)
		}
	}
}

func TestAuthAllowsEveryTokenDuringRotation(t *testing.T) {
	h := authServer(testToken, rotatedToken)
	for _, token := range []string{testToken, rotatedToken} {
		res := get(t, h, "/version", map[string]string{"Authorization": "Bearer " + token})
		if res.StatusCode != http.StatusOK {
			t.Fatalf("token %q = %d, want 200", token, res.StatusCode)
		}
	}
}

func TestAuthRejectsMissingHeader(t *testing.T) {
	res := get(t, authServer(testToken), "/version", nil)
	assertJSONErrorEnvelope(t, res, http.StatusUnauthorized)

	if got := res.Header.Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Fatalf("WWW-Authenticate = %q, want a Bearer challenge", got)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), testToken) {
		t.Fatalf("401 body echoes the configured token: %s", body)
	}
}

func TestAuthRejectsMalformedHeader(t *testing.T) {
	// Each of these is a syntactically wrong credential, not a wrong token:
	// they must be rejected the same way, with no hint about which part was
	// wrong.
	for name, header := range map[string]string{
		"scheme only":  "Bearer",
		"blank token":  "Bearer ",
		"other scheme": "Basic dXNlcjpwYXNzd29yZA==",
		"no scheme":    testToken,
		"typo scheme":  "Beraer " + testToken,
	} {
		res := get(t, authServer(testToken), "/version", map[string]string{"Authorization": header})
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s (%q) = %d, want 401", name, header, res.StatusCode)
		}
	}
}

func TestAuthRejectsWrongToken(t *testing.T) {
	for _, token := range []string{"wrong", "", "s3cret", testToken + testToken, strings.ToUpper(testToken)} {
		res := get(t, authServer(testToken), "/version", map[string]string{"Authorization": "Bearer " + token})
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q = %d, want 401", token, res.StatusCode)
		}
		hdr := res.Header.Get("WWW-Authenticate")
		if !strings.Contains(hdr, "Bearer") {
			t.Fatalf("token %q: WWW-Authenticate = %q", token, hdr)
		}
	}
}

// TestAuthRejectsLegacyAdminLiterals pins the removed RoleEnforcer's
// backdoor: "admin" and "token-admin" were mapped to RoleAdmin without being
// configured. They must be treated like any other unknown credential.
func TestAuthRejectsLegacyAdminLiterals(t *testing.T) {
	for _, token := range []string{"admin", "token-admin", "editor", "token-editor"} {
		res := get(t, authServer(testToken), "/version", map[string]string{"Authorization": "Bearer " + token})
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q = %d, want 401", token, res.StatusCode)
		}
	}
}

func TestAuthProtectsDataRoutesNotJustVersion(t *testing.T) {
	res := get(t, authServer(testToken), "/monitors", nil)
	assertJSONErrorEnvelope(t, res, http.StatusUnauthorized)

	// The same route serves once the token is presented, and the handler
	// still runs (the middleware must not swallow authenticated requests).
	h := authServerOn(&pageStore{n: 1}, auth.New([]string{testToken}, 0))
	res = get(t, h, "/monitors", map[string]string{"Authorization": "Bearer " + testToken})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("authorised GET /monitors = %d, want 200", res.StatusCode)
	}
}

func TestAuthUnconfiguredKeepsOpenAccess(t *testing.T) {
	res := get(t, authServer(), "/version", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("unconfigured deployment = %d, want 200", res.StatusCode)
	}
	if got := res.Header.Get("WWW-Authenticate"); got != "" {
		t.Fatalf("unconfigured deployment sent a challenge: %q", got)
	}

	// A nil authenticator (Server built without WithAuth) must behave the
	// same way: no panic, no challenge.
	s := New(&fakeStore{}, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger())
	res = get(t, s.Routes(), "/version", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("nil authenticator = %d, want 200", res.StatusCode)
	}
}

func TestAuthExemptsProbes(t *testing.T) {
	h := authServer(testToken)
	for _, path := range []string{"/health", "/livez", "/readyz"} {
		res := get(t, h, path, nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s without a token = %d, want 200 (probes must stay reachable)", path, res.StatusCode)
		}
	}
	// Everything else stays behind the token, including the info endpoints.
	for _, path := range []string{"/version", "/stats", "/monitors"} {
		res := get(t, h, path, nil)
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without a token = %d, want 401", path, res.StatusCode)
		}
	}
}

func TestAuthAcceptsDashboardSessionCookie(t *testing.T) {
	authn := auth.New([]string{testToken}, 0)
	id, ok := authn.Login(testToken)
	if !ok {
		t.Fatal("login with the configured token failed")
	}
	h := authServerOn(&fakeStore{}, authn)

	// The dashboard links to /api/v1/alerts.csv, which a browser fetches as
	// a plain navigation: the session cookie is the only credential it can
	// send.
	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: id})
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("session cookie = %d, want 200", res.Code)
	}

	// A cookie that was never issued is not a credential.
	req = httptest.NewRequest(http.MethodGet, "/version", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: strings.Repeat("A", 43)})
	res = httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("forged session cookie = %d, want 401", res.Code)
	}
}

// The access log is written for every request and is the most likely place
// for a credential to leak, so pin that it never carries one — not from the
// header, and not from a URL that tried to smuggle the token in a query
// string (the log records the matched route pattern, never the raw URL).
func TestAuthTokenNeverReachesTheAccessLog(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	s := New(&fakeStore{}, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, log).
		WithAuth(auth.New([]string{testToken}, 0))
	h := RequestLog(log)(s.Routes())

	reqs := []*http.Request{
		requestWithToken("/version", testToken),                  // accepted
		requestWithToken("/version?token="+testToken, testToken), // accepted, token in query
		requestWithToken("/version", "wrong-token"),              // rejected
		requestWithToken("/monitors", ""),                        // rejected, no header at all
	}
	for _, req := range reqs {
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
	}

	if strings.Contains(buf.String(), testToken) {
		t.Fatalf("access log leaked the token:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "http request") {
		t.Fatalf("expected access log lines, got:\n%s", buf.String())
	}
}

func requestWithToken(path, token string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

type oidcTestStore struct {
	fakeStore
	lastWorkspace workspace.ID
}

func (s *oidcTestStore) ListMonitorsPage(ctx context.Context, _ store.ListFilter) ([]store.Monitor, error) {
	s.lastWorkspace, _ = workspace.From(ctx)
	return nil, nil
}

func (s *oidcTestStore) CreateMonitor(ctx context.Context, m *store.Monitor) error {
	s.lastWorkspace, _ = workspace.From(ctx)
	m.ID = 1
	return nil
}

func (s *oidcTestStore) RecordAudit(_ context.Context, _ *store.AuditEntry) error {
	return nil
}

func (s *oidcTestStore) ListAuditEntries(ctx context.Context, _ store.AuditFilter) ([]store.AuditEntry, error) {
	s.lastWorkspace, _ = workspace.From(ctx)
	return nil, nil
}

func TestOIDCGroupMappingAPIComposesWithRoleCheckAndRouteScopes(t *testing.T) {
	idp := oidctest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	oidcProvider, err := auth.NewOIDCProvider(ctx, auth.OIDCConfig{
		Issuer:       idp.Issuer(),
		ClientID:     idp.ClientID,
		ClientSecret: idp.ClientSecret,
		RedirectURL:  "https://beacon.example.com/login/oidc/callback",
		GroupClaim:   "groups",
		GroupMappings: []auth.GroupMapping{
			{Group: "sre-prod", Workspace: "prod", Role: auth.RoleAdmin},
			{Group: "devs-prod", Workspace: "prod", Role: auth.RoleEditor},
			{Group: "auditors-prod", Workspace: "prod", Role: auth.RoleViewer},
			{Group: "devs-staging", Workspace: "staging", Role: auth.RoleEditor},
		},
	})
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}

	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, nil))
	st := &oidcTestStore{}
	authn := auth.New(nil, 0).WithOIDC(oidcProvider)
	srv := New(st, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, log).WithAuth(authn)
	h := RequestLog(log)(srv.Routes())

	mintToken := func(sub string, groups any, scopes []string) string {
		now := time.Now()
		claims := map[string]any{
			"iss":   idp.Issuer(),
			"sub":   sub,
			"aud":   idp.ClientID,
			"iat":   now.Unix(),
			"exp":   now.Add(time.Hour).Unix(),
			"email": sub + "@example.com",
		}
		if groups != nil {
			claims["groups"] = groups
		}
		if len(scopes) > 0 {
			claims["sorobeacon_scopes"] = scopes
		}
		return idp.IDToken(claims)
	}

	call := func(method, path, token, body string) *httptest.ResponseRecorder {
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, path, rdr)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		return res
	}

	// 1. Unmapped group returns 403 Forbidden and never logs or echoes raw claim values or tokens.
	unmappedTok := mintToken("user-unmapped", []string{"canary-unmapped-group-secret-99"}, nil)
	res := call(http.MethodGet, "/monitors", unmappedTok, "")
	if res.Code != http.StatusForbidden {
		t.Fatalf("unmapped group token = %d, want 403: %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), "canary-unmapped-group-secret-99") || strings.Contains(res.Body.String(), unmappedTok) {
		t.Fatalf("403 response body leaked claim or token: %s", res.Body.String())
	}

	// 2. Missing groups claim also fails closed with 403.
	missingClaimTok := mintToken("user-nogroups", nil, nil)
	if res := call(http.MethodGet, "/monitors", missingClaimTok, ""); res.Code != http.StatusForbidden {
		t.Fatalf("missing group claim token = %d, want 403", res.Code)
	}

	// 3. Forged JWT (signed with an unknown key) returns 401 Unauthorized.
	forgedTok := idp.IDTokenWith(idp.OtherKey(), map[string]any{
		"iss":    idp.Issuer(),
		"sub":    "attacker",
		"aud":    idp.ClientID,
		"exp":    time.Now().Add(time.Hour).Unix(),
		"groups": []string{"sre-prod"},
	})
	if res := call(http.MethodGet, "/monitors", forgedTok, ""); res.Code != http.StatusUnauthorized {
		t.Fatalf("forged JWT = %d, want 401", res.Code)
	}

	// 4. Viewer group can read in mapped workspace ("prod"), but RoleCheck denies writes and admin routes (403).
	viewerTok := mintToken("user-viewer", []string{"auditors-prod"}, nil)
	st.lastWorkspace = ""
	if res := call(http.MethodGet, "/monitors", viewerTok, ""); res.Code != http.StatusOK || st.lastWorkspace != "prod" {
		t.Fatalf("viewer GET /monitors = %d (ws=%q), want 200 (ws=prod)", res.Code, st.lastWorkspace)
	}
	const validCreateMonitor = `{"contract_ids":["CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"],"name":"m1"}`
	if res := call(http.MethodPost, "/monitors", viewerTok, validCreateMonitor); res.Code != http.StatusForbidden {
		t.Fatalf("viewer POST /monitors = %d, want 403", res.Code)
	}
	if res := call(http.MethodGet, "/audit", viewerTok, ""); res.Code != http.StatusForbidden {
		t.Fatalf("viewer GET /audit = %d, want 403", res.Code)
	}

	// 5. Editor group can write in mapped workspace, but RoleCheck denies admin routes (403).
	editorTok := mintToken("user-editor", []string{"devs-staging"}, nil)
	st.lastWorkspace = ""
	if res := call(http.MethodPost, "/monitors", editorTok, validCreateMonitor); res.Code != http.StatusCreated || st.lastWorkspace != "staging" {
		t.Fatalf("editor POST /monitors = %d (ws=%q), want 201 (ws=staging): %s", res.Code, st.lastWorkspace, res.Body.String())
	}
	if res := call(http.MethodGet, "/audit", editorTok, ""); res.Code != http.StatusForbidden {
		t.Fatalf("editor GET /audit = %d, want 403", res.Code)
	}

	// 6. Multi-group token ("auditors-prod" + "sre-prod") resolves to highest role (admin in prod).
	multiTok := mintToken("user-multi", []string{"auditors-prod", "sre-prod"}, nil)
	st.lastWorkspace = ""
	if res := call(http.MethodGet, "/audit", multiTok, ""); res.Code != http.StatusOK || st.lastWorkspace != "prod" {
		t.Fatalf("multi-group admin GET /audit = %d (ws=%q), want 200 (ws=prod): %s", res.Code, st.lastWorkspace, res.Body.String())
	}

	// 7. Composition with routeScopes: an editor token further constrained to monitors:read
	// passes GET /monitors, but is denied POST /monitors (missing monitors:write) and GET /channels.
	scopedEditorTok := mintToken("user-scoped-editor", []string{"devs-prod"}, []string{"monitors:read"})
	if res := call(http.MethodGet, "/monitors", scopedEditorTok, ""); res.Code != http.StatusOK {
		t.Fatalf("scoped editor GET /monitors = %d, want 200", res.Code)
	}
	if res := call(http.MethodPost, "/monitors", scopedEditorTok, validCreateMonitor); res.Code != http.StatusForbidden {
		t.Fatalf("scoped editor POST /monitors = %d, want 403", res.Code)
	}
	if res := call(http.MethodGet, "/channels", scopedEditorTok, ""); res.Code != http.StatusForbidden {
		t.Fatalf("scoped editor GET /channels = %d, want 403", res.Code)
	}

	// 8. Live mapping reload removes devs-staging without restart; subsequent requests fail closed with 403.
	if err := oidcProvider.SetGroupMappings([]auth.GroupMapping{
		{Group: "sre-prod", Workspace: "prod", Role: auth.RoleAdmin},
	}); err != nil {
		t.Fatalf("SetGroupMappings: %v", err)
	}
	if res := call(http.MethodGet, "/monitors", editorTok, ""); res.Code != http.StatusForbidden {
		t.Fatalf("after removing devs-staging mapping, GET /monitors = %d, want 403", res.Code)
	}

	// 9. Verify logs never contain any raw group claim values or bearer tokens.
	logs := logBuf.String()
	for _, secret := range []string{
		"canary-unmapped-group-secret-99",
		unmappedTok,
		viewerTok,
		editorTok,
		multiTok,
	} {
		if strings.Contains(logs, secret) {
			t.Fatalf("logs leaked secret or claim value %q:\n%s", secret, logs)
		}
	}
}
