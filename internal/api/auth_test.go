package api

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
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
