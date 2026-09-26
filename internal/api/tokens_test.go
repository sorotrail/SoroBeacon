package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// newTokenServer builds the real API over a real SQLite database with token
// storage wired the way cmd/sorobeacon wires it. A real store rather than a
// fake, because half of this feature lives in storage — the digest column and
// the workspace predicate on every token read — and a fake would leave exactly
// that part untested.
func newTokenServer(t *testing.T, log *slog.Logger) (chi.Router, *auth.Manager, store.Store) {
	t.Helper()
	if log == nil {
		log = discardLogger()
	}
	st := newTenantStore(t)
	mgr := auth.NewManager(st, log)
	s := New(st, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, log).
		WithAuth(twoWorkspaces()).WithTokens(mgr)
	return s.Routes(), mgr, st
}

// callJSON issues one request with an optional bearer token and returns the
// recorded response.
func callJSON(t *testing.T, h http.Handler, method, path, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	return res
}

// mint calls POST /tokens as acme and returns the secret it issued, failing the
// test unless the mint itself succeeded.
func mint(t *testing.T, h chi.Router, body string) string {
	t.Helper()
	res := callJSON(t, h, http.MethodPost, "/tokens", acmeToken, body)
	if res.Code != http.StatusCreated {
		t.Fatalf("POST /tokens %s = %d (%s), want 201", body, res.Code, res.Body)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode mint response: %v", err)
	}
	if out.Token == "" {
		t.Fatalf("mint response carries no token: %s", res.Body)
	}
	return out.Token
}

// tokenIDOf looks a minted token up in the listing by its prefix — the only
// handle the listing offers, which is the point of it.
func tokenIDOf(t *testing.T, h http.Handler, bearer, raw string) int64 {
	t.Helper()
	res := callJSON(t, h, http.MethodGet, "/tokens", bearer, "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET /tokens = %d (%s)", res.Code, res.Body)
	}
	var listed struct {
		Tokens []struct {
			ID     int64  `json:"id"`
			Prefix string `json:"prefix"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode listing: %v", err)
	}
	want := raw[:len(auth.TokenPrefix)+8]
	for _, tok := range listed.Tokens {
		if tok.Prefix == want {
			return tok.ID
		}
	}
	t.Fatalf("listing has no token with prefix %q: %s", want, res.Body)
	return 0
}

func TestMintedTokenAuthenticatesWithinItsScopes(t *testing.T) {
	h, _, _ := newTokenServer(t, nil)
	raw := mint(t, h, `{"name":"ci","scopes":["monitors:read"]}`)

	// Read is enough to list, and the tenant comes from the token's own row:
	// newTenantStore gives acme and beta one monitor each, so this response also
	// proves a scoped token cannot read the other workspace.
	res := callJSON(t, h, http.MethodGet, "/monitors", raw, "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET /monitors with monitors:read = %d, want 200", res.Code)
	}
	if body := res.Body.String(); !strings.Contains(body, "acme monitor") || strings.Contains(body, "beta monitor") {
		t.Fatalf("the scoped token saw the wrong tenants: %s", body)
	}

	for _, denied := range []struct{ method, path string }{
		{http.MethodPost, "/monitors"},
		{http.MethodGet, "/channels"},
		{http.MethodGet, "/alerts"},
		{http.MethodGet, "/stats"},
		{http.MethodGet, "/tokens"},
		{http.MethodDelete, "/monitors/1"},
	} {
		res := callJSON(t, h, denied.method, denied.path, raw, `{}`)
		if res.Code != http.StatusForbidden {
			t.Errorf("%s %s with monitors:read = %d, want 403", denied.method, denied.path, res.Code)
		}
	}

	// A denial is 403, not 401: the credential was accepted and the permission
	// was missing, and that difference is what an operator debugging a pipeline
	// needs. /version is the route that asks for a credential and no permission.
	res = callJSON(t, h, http.MethodGet, "/version", raw, "")
	if res.Code != http.StatusOK {
		t.Errorf("GET /version = %d, want 200 for any valid credential", res.Code)
	}

	// The write scope is what buys the write, and it is a different token.
	write := mint(t, h, `{"name":"ci-write","scopes":["monitors:write"]}`)
	res = callJSON(t, h, http.MethodPost, "/monitors", write,
		`{"name":"from ci","contract_ids":["`+validContract+`"]}`)
	if res.Code != http.StatusCreated {
		t.Fatalf("POST /monitors with monitors:write = %d (%s), want 201", res.Code, res.Body)
	}
}

// TestRawTokenAppearsOnlyInItsCreationResponse is #293's acceptance bar: the
// secret is printed once and cannot be read back. Every response that touches
// the token afterwards is checked — listings, revocation, the bodies of requests
// the token itself makes, the rows the store hands back, and the access log that
// runs alongside all of it — with a positive control so the test cannot pass by
// having never minted anything.
func TestRawTokenAppearsOnlyInItsCreationResponse(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	h, mgr, st := newTokenServer(t, logger)
	// The access log is the realistic leak: it sees every request, including the
	// mint, and is written by code this feature does not control.
	recorded := RequestLog(logger)(h)

	res := callJSON(t, recorded, http.MethodPost, "/tokens", acmeToken, `{"name":"leak-check","scopes":["monitors:read"]}`)
	if res.Code != http.StatusCreated {
		t.Fatalf("mint = %d (%s)", res.Code, res.Body)
	}
	creation := res.Body.String()
	var created struct {
		ID    int64  `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(creation), &created); err != nil {
		t.Fatalf("decode mint: %v", err)
	}
	raw := created.Token
	if raw == "" || !strings.Contains(creation, raw) {
		t.Fatalf("the creation response does not carry its own token: %s", creation)
	}

	check := func(name string, res *httptest.ResponseRecorder) {
		body := res.Body.String()
		if strings.Contains(body, raw) {
			t.Errorf("%s leaked the token: %s", name, body)
		}
		// Nor the digest: it is the lookup key for every future request, so a
		// body containing it is a credential in everything but name.
		if strings.Contains(body, auth.HashToken(raw)) {
			t.Errorf("%s leaked the stored digest: %s", name, body)
		}
	}

	check("GET /tokens", callJSON(t, recorded, http.MethodGet, "/tokens", acmeToken, ""))
	check("revoke", callJSON(t, recorded, http.MethodPost,
		"/tokens/"+strconv.FormatInt(created.ID, 10)+"/revoke", acmeToken, ""))
	check("list made with the token", callJSON(t, recorded, http.MethodGet, "/monitors", raw, ""))
	check("denied route", callJSON(t, recorded, http.MethodGet, "/channels", raw, ""))
	check("refused credential", callJSON(t, recorded, http.MethodGet, "/monitors", auth.TokenPrefix+"never-minted", ""))

	// The rows behind those responses: the API type has no secret field, and
	// neither may the stored row.
	toks, err := mgr.List(workspace.With(context.Background(), "acme"))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(toks) == 0 {
		t.Fatal("nothing was listed after minting")
	}
	for _, tok := range toks {
		if tok.Hash == raw || strings.Contains(tok.Name+"|"+tok.Prefix, strings.TrimPrefix(raw, auth.TokenPrefix)) {
			t.Errorf("listed row %+v holds the secret", tok)
		}
	}
	stored, found, err := st.TokenByHash(context.Background(), auth.HashToken(raw))
	if err != nil || !found {
		t.Fatalf("TokenByHash = found %v, err %v", found, err)
	}
	if stored.Hash != auth.HashToken(raw) || stored.Name != "leak-check" {
		t.Errorf("stored row = %+v", stored)
	}

	if !strings.Contains(logs.String(), "http request") {
		t.Fatalf("expected access log lines, got:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), raw) || strings.Contains(logs.String(), auth.HashToken(raw)) {
		t.Errorf("logs leaked the credential:\n%s", logs.String())
	}
}

func TestCreateTokenValidation(t *testing.T) {
	h, _, _ := newTokenServer(t, nil)

	for _, tc := range []struct {
		name      string
		body      string
		wantCode  int
		wantField string
	}{
		{"no scopes", `{"name":"x"}`, http.StatusBadRequest, "scopes"},
		{"empty scopes", `{"scopes":[]}`, http.StatusBadRequest, "scopes"},
		{"unknown scope", `{"scopes":["monitors:see"]}`, http.StatusBadRequest, "scopes"},
		{"bad duration", `{"scopes":["monitors:read"],"expires_in":"72"}`, http.StatusBadRequest, "expires_in"},
		{"negative duration", `{"scopes":["monitors:read"],"expires_in":"-1h"}`, http.StatusBadRequest, "expires_in"},
		{"over-long name", `{"name":"` + strings.Repeat("n", auth.MaxTokenNameLen+1) + `","scopes":["monitors:read"]}`, http.StatusBadRequest, "name"},
		{"malformed json", `{"scopes":`, http.StatusBadRequest, ""},
	} {
		res := callJSON(t, h, http.MethodPost, "/tokens", acmeToken, tc.body)
		if res.Code != tc.wantCode {
			t.Errorf("%s = %d (%s), want %d", tc.name, res.Code, res.Body, tc.wantCode)
			continue
		}
		if tc.wantField == "" {
			continue
		}
		// A validation problem is a 400 naming the field, per the repo's envelope
		// convention, so a client can fix the request without guessing.
		var env errorEnvelope
		if err := json.Unmarshal(res.Body.Bytes(), &env); err != nil {
			t.Errorf("%s: decode envelope: %v (%s)", tc.name, err, res.Body)
			continue
		}
		found := false
		for _, d := range env.Details {
			if d.Field == tc.wantField {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: envelope %+v names no detail on %q", tc.name, env, tc.wantField)
		}
	}

	// A credential that can call nothing is refused at the door, and an
	// unrestricted caller may mint "*" — that is what API_TOKEN's reach looks like
	// in a revocable form.
	star := mint(t, h, `{"name":"everything","scopes":["*"]}`)
	if res := callJSON(t, h, http.MethodGet, "/channels", star, ""); res.Code != http.StatusOK {
		t.Errorf("a * token on GET /channels = %d, want 200", res.Code)
	}
}

func TestScopedTokenCannotMintStrongerTokens(t *testing.T) {
	// The escalation this closes: a token that can mint tokens must not be able
	// to mint a better one, or tokens:write is a road back to full control.
	h, _, _ := newTokenServer(t, nil)
	limited := mint(t, h, `{"name":"limited","scopes":["monitors:read","tokens:write"]}`)

	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"wider scope", `{"name":"wider","scopes":["channels:write"]}`, http.StatusForbidden},
		{"star", `{"name":"star","scopes":["*"]}`, http.StatusForbidden},
		{"one held and one not", `{"scopes":["monitors:read","channels:write"]}`, http.StatusForbidden},
		{"mints what it holds", `{"scopes":["monitors:read"]}`, http.StatusCreated},
		{"mints nothing", `{"scopes":[]}`, http.StatusBadRequest},
	} {
		res := callJSON(t, h, http.MethodPost, "/tokens", limited, tc.body)
		if res.Code != tc.want {
			t.Errorf("%s = %d (%s), want %d", tc.name, res.Code, res.Body, tc.want)
		}
	}

	// Within one resource write carries read (Principal.Allowed), so this caller
	// may list tokens — and that listing, which never shows a secret or a digest,
	// is the whole of the extra reach. What it cannot do is mint wider, which the
	// table above checks.
	if res := callJSON(t, h, http.MethodGet, "/tokens", limited, ""); res.Code != http.StatusOK {
		t.Errorf("GET /tokens with tokens:write = %d, want 200", res.Code)
	}
	// A resource it holds no scope for is closed in both directions.
	if res := callJSON(t, h, http.MethodGet, "/channels", limited, ""); res.Code != http.StatusForbidden {
		t.Errorf("GET /channels = %d, want 403", res.Code)
	}
}

func TestRevokedAndExpiredTokensAreRejectedTheSameWay(t *testing.T) {
	h, _, _ := newTokenServer(t, nil)

	revoked := mint(t, h, `{"name":"retire-me","scopes":["monitors:read"]}`)
	id := tokenIDOf(t, h, acmeToken, revoked)
	path := "/tokens/" + strconv.FormatInt(id, 10) + "/revoke"
	if res := callJSON(t, h, http.MethodPost, path, acmeToken, ""); res.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d (%s), want 204", res.Code, res.Body)
	}
	// Revoking twice is idempotent for the caller: the row is already revoked,
	// and a 404 would make a retried request look like a bug.
	if res := callJSON(t, h, http.MethodPost, path, acmeToken, ""); res.Code != http.StatusNoContent {
		t.Errorf("second revoke = %d, want 204", res.Code)
	}

	// "1ns" is positive, so the request is valid; the stored expiry truncates to
	// the second it was minted in, which has already gone.
	expired := mint(t, h, `{"name":"already-gone","scopes":["monitors:read"],"expires_in":"1ns"}`)

	bodies := map[string]string{}
	for name, raw := range map[string]string{
		"revoked": revoked,
		"expired": expired,
		"unknown": auth.TokenPrefix + strings.Repeat("0", 43),
	} {
		res := callJSON(t, h, http.MethodGet, "/monitors", raw, "")
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("%s token = %d, want 401", name, res.Code)
		}
		// request_id is per-response by design, so it is the envelope's meaning
		// that has to be identical.
		var env errorEnvelope
		if err := json.Unmarshal(res.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s: decode envelope: %v (%s)", name, err, res.Body)
		}
		bodies[name] = env.Error + "|" + env.Code
	}
	// Same status, same envelope. Which of the three it was is precisely the
	// information a caller probing a guessed credential could use.
	if bodies["revoked"] != bodies["expired"] || bodies["revoked"] != bodies["unknown"] {
		t.Errorf("401 bodies differ: revoked=%s expired=%s unknown=%s",
			bodies["revoked"], bodies["expired"], bodies["unknown"])
	}

	// And a live token still works, so the refusals above are about those
	// credentials rather than about the wiring.
	if res := callJSON(t, h, http.MethodGet, "/monitors", mint(t, h, `{"scopes":["monitors:read"]}`), ""); res.Code != http.StatusOK {
		t.Errorf("a fresh token = %d, want 200", res.Code)
	}
}

func TestRevokeIsScopedToTheOwningWorkspace(t *testing.T) {
	h, _, _ := newTokenServer(t, nil)
	raw := mint(t, h, `{"name":"acme-only","scopes":["monitors:read"]}`)
	id := tokenIDOf(t, h, acmeToken, raw)

	// Beta holds the same kind of credential and must neither retire acme's token
	// nor learn that the id exists.
	res := callJSON(t, h, http.MethodPost, "/tokens/"+strconv.FormatInt(id, 10)+"/revoke", betaToken, "")
	if res.Code != http.StatusNotFound {
		t.Fatalf("cross-workspace revoke = %d, want 404", res.Code)
	}
	if res := callJSON(t, h, http.MethodGet, "/monitors", raw, ""); res.Code != http.StatusOK {
		t.Errorf("the token still works, so the cross-tenant revoke did not touch it: %d", res.Code)
	}

	// Beta's listing holds no acme rows at all.
	res = callJSON(t, h, http.MethodGet, "/tokens", betaToken, "")
	if res.Code != http.StatusOK {
		t.Fatalf("beta GET /tokens = %d", res.Code)
	}
	if strings.Contains(res.Body.String(), "acme-only") {
		t.Errorf("beta listed acme's tokens: %s", res.Body.String())
	}
}

func TestTokenLifecycleIsAuditable(t *testing.T) {
	h, _, _ := newTokenServer(t, nil)
	raw := mint(t, h, `{"name":"audit-me","scopes":["monitors:read","alerts:read"]}`)

	// Nothing has used it yet.
	body := callJSON(t, h, http.MethodGet, "/tokens", acmeToken, "").Body.String()
	if !strings.Contains(body, `"last_used_at":null`) {
		t.Fatalf("a fresh token should have no last use: %s", body)
	}

	if res := callJSON(t, h, http.MethodGet, "/monitors", raw, ""); res.Code != http.StatusOK {
		t.Fatalf("using the token = %d", res.Code)
	}

	var listed struct {
		Tokens []struct {
			Name       string   `json:"name"`
			Prefix     string   `json:"prefix"`
			Scopes     []string `json:"scopes"`
			CreatedAt  string   `json:"created_at"`
			LastUsedAt *string  `json:"last_used_at"`
			ExpiresAt  *string  `json:"expires_at"`
			RevokedAt  *string  `json:"revoked_at"`
		} `json:"tokens"`
	}
	body = callJSON(t, h, http.MethodGet, "/tokens", acmeToken, "").Body.String()
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Fatalf("decode listing: %v (%s)", err, body)
	}
	if len(listed.Tokens) != 1 {
		t.Fatalf("listing = %s", body)
	}
	tok := listed.Tokens[0]
	if tok.Name != "audit-me" || tok.ExpiresAt != nil || tok.RevokedAt != nil {
		t.Errorf("listing row = %+v", tok)
	}
	if tok.CreatedAt == "" {
		t.Error("created_at is missing")
	}
	if !slices.Equal(tok.Scopes, []string{"alerts:read", "monitors:read"}) {
		t.Errorf("scopes = %v, want the full grant in stable order", tok.Scopes)
	}
	// The prefix is the display handle: the secret's own leading characters,
	// which is what lets an operator match a token pasted somewhere to a row.
	if want := raw[:len(auth.TokenPrefix)+8]; tok.Prefix != want {
		t.Errorf("prefix = %q, want %q", tok.Prefix, want)
	}
	if tok.LastUsedAt == nil {
		t.Error("using a token must record when it was used")
	}

	id := tokenIDOf(t, h, acmeToken, raw)
	callJSON(t, h, http.MethodPost, "/tokens/"+strconv.FormatInt(id, 10)+"/revoke", acmeToken, "")
	body = callJSON(t, h, http.MethodGet, "/tokens", acmeToken, "").Body.String()
	// Revocation keeps the row: the answer worth having afterwards is which token
	// this was and when it was last used.
	if !strings.Contains(body, `"revoked_at":"`) || !strings.Contains(body, "audit-me") {
		t.Errorf("a revoked token should stay listed with its timestamp: %s", body)
	}
}

func TestTokenEndpointsWithoutStorageAreNotPretendAvailable(t *testing.T) {
	// An instance whose store cannot hold tokens must say so, rather than mint
	// credentials that would vanish on the next request.
	st := newTenantStore(t)
	h := New(st, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger()).
		WithAuth(twoWorkspaces()).Routes()

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/tokens"},
		{http.MethodGet, "/tokens"},
		{http.MethodPost, "/tokens/1/revoke"},
	} {
		res := callJSON(t, h, tc.method, tc.path, acmeToken, `{"scopes":["monitors:read"]}`)
		if res.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503", tc.method, tc.path, res.Code)
		}
	}
}

// TestScopedTokenDeniedOnUnmappedRoute proves the fail-closed branch end to end:
// a route that exists but was never mapped is denied to a scoped token, rather
// than treated as public to a credential that authenticated.
func TestScopedTokenDeniedOnUnmappedRoute(t *testing.T) {
	h, _, _ := newTokenServer(t, nil)
	raw := mint(t, h, `{"name":"ci","scopes":["monitors:read"]}`)
	// Registered after the table was written, so it is genuinely unmapped.
	h.Get("/unmapped-for-test", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("the handler must not run"))
	})

	res := callJSON(t, h, http.MethodGet, "/unmapped-for-test", raw, "")
	if res.Code != http.StatusForbidden {
		t.Fatalf("unmapped route with a scoped token = %d, want 403", res.Code)
	}
	if strings.Contains(res.Body.String(), "the handler must not run") {
		t.Error("the handler ran for a request the scope check denied")
	}

	// The static token still reaches it: this denial is about scoped credentials,
	// not a new gate on the operator's own.
	res = callJSON(t, h, http.MethodGet, "/unmapped-for-test", acmeToken, "")
	if res.Code != http.StatusOK {
		t.Fatalf("unmapped route with a static token = %d, want 200", res.Code)
	}
}
