package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/oidctest"
	"github.com/sorotrail/sorobeacon/internal/rules"
)

// The dashboard's half of single sign-on: two GET routes, and the promise that
// what comes out of the callback is an ordinary session — gated, tenant-scoped
// and Cookie-bound exactly like the one the token form mints.

// ssoDashboard is a gated dashboard with a provider configured. tokens are the
// static credentials to keep beside it, which is the migration state most
// deployments sit in.
func ssoDashboard(t *testing.T, idp *oidctest.Provider, mutate func(*auth.OIDCConfig), logs *strings.Builder, tokens ...string) (http.Handler, *auth.Authenticator) {
	t.Helper()
	sink := io.Discard
	if logs != nil {
		sink = logs
	}
	logger := slog.New(slog.NewTextHandler(sink, nil))
	s, err := New(emptyStore{}, rules.NewRegistry(), notify.DefaultFactory(), logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a := auth.New(tokens, 0).WithOIDC(ssoProvider(t, idp, mutate))
	return s.WithAuth(a).Routes(), a
}

func ssoProvider(t *testing.T, idp *oidctest.Provider, mutate func(*auth.OIDCConfig)) *auth.OIDCProvider {
	t.Helper()
	cfg := auth.OIDCConfig{
		Issuer:       idp.Issuer(),
		ClientID:     idp.ClientID,
		ClientSecret: idp.ClientSecret,
		RedirectURL:  "https://beacon.example.com" + oidcCallbackPath,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := auth.NewOIDCProvider(ctx, cfg)
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}
	return p
}

// beginLogin performs the browser's first hop and returns what the callback has
// to present: the state in the query *and* the cookie, read from the provider's
// redirect rather than handed over by the test, so the two halves being the
// same value is something this proves rather than assumes.
//
// The returned path is the callback URL with a code from the provider and an
// ID token queued up, ready for the browser to be sent back to.
func beginLogin(t *testing.T, h http.Handler, idp *oidctest.Provider, next string, claims func(map[string]any)) (url.Values, []*http.Cookie) {
	t.Helper()
	res := getPath(t, h, oidcStartPath+"?next="+url.QueryEscape(next))
	defer res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET %s = %d, want a redirect to the provider:\n%s", oidcStartPath, res.StatusCode, readBody(t, res))
	}
	loc := res.Header.Get("Location")
	if loc == "" {
		t.Fatalf("GET %s did not redirect to the provider", oidcStartPath)
	}
	parsed, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse provider redirect: %v", err)
	}
	q := parsed.Query()
	code, err := idp.Authorize(q)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	tok := idp.ClaimsFor(code)
	if claims != nil {
		claims(tok)
	}
	idp.RespondWith(idp.IDToken(tok))
	return url.Values{"state": {q.Get("state")}, "code": {code}}, res.Cookies()
}

// signOut is the dashboard's sign-out button: a POST carrying the session
// cookie, which is what the logout route accepts.
func signOut(t *testing.T, h http.Handler, session *http.Cookie) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, logoutPath, strings.NewReader(url.Values{}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(session)
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	return res.Result()
}

func callback(t *testing.T, h http.Handler, q url.Values, cookies []*http.Cookie, extra url.Values) *http.Response {
	t.Helper()
	for k, vs := range extra {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	return getPath(t, h, oidcCallbackPath+"?"+q.Encode(), cookies...)
}

func cookieOf(t *testing.T, res *http.Response, name string) *http.Cookie {
	t.Helper()
	for _, c := range res.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestSignInPageOffersWhatTheInstanceAccepts(t *testing.T) {
	// Both ways in: an operator turning SSO on does not lose the token form,
	// and one who has only ever used SSO is not shown a form that cannot work.
	// The SSO marker is the form's action rather than its button label because
	// the muted help text says "Sign in with …" for the token path too.
	t.Run("provider and token", func(t *testing.T) {
		idp := oidctest.New(t)
		h, _ := ssoDashboard(t, idp, nil, nil, testToken)
		page := readBody(t, getPath(t, h, loginPath))
		if !strings.Contains(page, `action="`+oidcStartPath+`"`) {
			t.Errorf("no SSO form on the sign-in page:\n%s", page)
		}
		if !strings.Contains(page, `name="token"`) {
			t.Error("the token form disappeared where a static token is configured")
		}
	})
	t.Run("provider only", func(t *testing.T) {
		idp := oidctest.New(t)
		h, _ := ssoDashboard(t, idp, nil, nil)
		page := readBody(t, getPath(t, h, loginPath))
		if !strings.Contains(page, `action="`+oidcStartPath+`"`) {
			t.Errorf("no SSO form on the sign-in page:\n%s", page)
		}
		if strings.Contains(page, `name="token"`) {
			t.Error("the page offered a token form that could never be accepted")
		}
	})
	t.Run("token only", func(t *testing.T) {
		srv, _ := authDashboard(t)
		page := readBody(t, getPath(t, srv.Routes(), loginPath))
		if strings.Contains(page, `action="`+oidcStartPath+`"`) {
			t.Error("the page offered SSO that this instance has not configured")
		}
		if !strings.Contains(page, `name="token"`) {
			t.Error("the token form is missing")
		}
	})
}

func TestSignInRoutesNeedNoSession(t *testing.T) {
	// The chicken-and-egg case: the sign-in flow must be reachable by someone
	// who is by definition not signed in. If the gate covered these paths,
	// every SSO login would bounce back to the start and never finish.
	idp := oidctest.New(t)
	h, a := ssoDashboard(t, idp, nil, nil, testToken)
	q, cookies := beginLogin(t, h, idp, "/alerts", nil)
	if len(cookies) == 0 {
		t.Fatal("the start response set no state cookie")
	}
	cb := callback(t, h, q, cookies, nil)
	defer cb.Body.Close()
	if cb.StatusCode != http.StatusSeeOther {
		t.Fatalf("callback = %d, want a signed-in redirect:\n%s", cb.StatusCode, readBody(t, cb))
	}
	if cookieOf(t, cb, auth.SessionCookie) == nil {
		t.Fatal("the callback set no session cookie")
	}
	if !a.HasSession(cookieOf(t, cb, auth.SessionCookie).Value) {
		t.Error("the cookie names no live session")
	}
}

func TestSSOLoginLandsInTheMappedWorkspace(t *testing.T) {
	// The one property that makes SSO safe on a shared instance: the tenant is
	// decided by the verified identity, and nothing the browser sends.
	idp := oidctest.New(t)
	var logs strings.Builder
	h, a := ssoDashboard(t, idp, func(cfg *auth.OIDCConfig) { cfg.WorkspaceClaim = "workspace" }, &logs)

	q, cookies := beginLogin(t, h, idp, "/monitors", func(c map[string]any) {
		c["workspace"] = "acme"
	})
	res := callback(t, h, q, cookies, nil)
	defer res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("callback = %d:\n%s", res.StatusCode, readBody(t, res))
	}
	if got := res.Header.Get("Location"); got != "/monitors" {
		t.Errorf("Location = %q, want the page the login started from", got)
	}
	session := cookieOf(t, res, auth.SessionCookie)
	if session == nil {
		t.Fatal("no session cookie")
	}
	if ws, ok := a.SessionWorkspace(session.Value); !ok || ws != "acme" {
		t.Errorf("the session belongs to %q (%v), want acme", ws, ok)
	}
	// The signed-in browser now reaches the dashboard through the ordinary gate.
	if gated := getPath(t, h, "/monitors", session); gated.StatusCode != http.StatusOK {
		t.Errorf("GET /monitors with the SSO session = %d, want 200", gated.StatusCode)
	}
	line := logs.String()
	if !strings.Contains(line, "dashboard signed in") || !strings.Contains(line, "user-123") {
		t.Errorf("expected a sign-in line naming the subject, got:\n%s", line)
	}
	// The subject and workspace belong in the log; the address does not.
	if strings.Contains(line, "ops@example.com") {
		t.Errorf("the sign-in line carried the user's email:\n%s", line)
	}
}

func TestCallbackRefusalsMintNothing(t *testing.T) {
	// Every refusal path starts no session and says so on the sign-in page: a
	// half-completed login must never look like a signed-out state.
	for _, tc := range []struct {
		name string
		// replay means the callback is presented twice, so the refusal under test
		// is the second use of a state that already worked once.
		replay  bool
		mutateQ func(q url.Values)
		cookies func(cs []*http.Cookie) []*http.Cookie
	}{
		{
			name: "no state cookie",
			cookies: func([]*http.Cookie) []*http.Cookie {
				return nil
			},
		},
		{
			name:    "state the server never issued",
			mutateQ: func(q url.Values) { q.Set("state", "a-forged-state") },
			cookies: func(cs []*http.Cookie) []*http.Cookie { return cs },
		},
		{
			name: "cookie from a different login",
			cookies: func([]*http.Cookie) []*http.Cookie {
				return []*http.Cookie{{Name: auth.StateCookie, Value: "a-different-login"}}
			},
		},
		{
			name:    "replayed callback",
			replay:  true,
			cookies: func(cs []*http.Cookie) []*http.Cookie { return cs },
		},
		{
			name:    "provider reported a refusal",
			mutateQ: func(q url.Values) { q.Set("error", "access_denied") },
			cookies: func(cs []*http.Cookie) []*http.Cookie { return cs },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idp := oidctest.New(t)
			var logs strings.Builder
			h, _ := ssoDashboard(t, idp, nil, &logs, testToken)
			q, cookies := beginLogin(t, h, idp, "/alerts", nil)
			if tc.mutateQ != nil {
				tc.mutateQ(q)
			}
			if tc.replay {
				// Spend the state on a successful login first; the code is spent
				// with it, so the second callback cannot be a fresh login.
				first := callback(t, h, q, cookies, nil)
				if status := readStatus(t, first); status != http.StatusSeeOther {
					t.Fatalf("first callback = %d, want the login to succeed", status)
				}
			}
			res := callback(t, h, q, tc.cookies(cookies), nil)
			defer res.Body.Close()
			page := readBody(t, res)
			if res.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401:\n%s", res.StatusCode, page)
			}
			if !strings.Contains(page, "Sign-in did not complete") {
				t.Errorf("the page does not say the login failed:\n%s", page)
			}
			if cookieOf(t, res, auth.SessionCookie) != nil {
				t.Error("a refused callback set a session cookie")
			}
			if c := cookieOf(t, res, auth.StateCookie); c == nil || c.MaxAge >= 0 {
				t.Errorf("the state cookie was not cleared: %+v", c)
			}
			if !strings.Contains(logs.String(), "oidc sign-in rejected") {
				t.Errorf("the refusal left no log line:\n%s", logs.String())
			}
			// The refusal's own reason is for the operator, and the generic
			// message is what the browser sees.
			if strings.Contains(page, "nonce") || strings.Contains(page, "id_token") {
				t.Errorf("the page explained which check failed:\n%s", page)
			}
		})
	}
}

// readStatus drains and closes a response so a test can check the status without
// leaking the body.
func readStatus(t *testing.T, res *http.Response) int {
	t.Helper()
	defer res.Body.Close()
	_ = readBody(t, res)
	return res.StatusCode
}

func TestCallbackCannotBeGivenARedirectTarget(t *testing.T) {
	// The post-login target is read back from the consumed state, so the
	// callback's own query has no way to name one — and the start route's does
	// go through safeNext, so an off-site target never enters at all.
	idp := oidctest.New(t)
	h, _ := ssoDashboard(t, idp, nil, nil, testToken)
	q, cookies := beginLogin(t, h, idp, "//evil.example/pwned", nil)
	res := callback(t, h, q, cookies, url.Values{"next": {"https://evil.example/pwned"}})
	defer res.Body.Close()
	readBody(t, res)
	if got := res.Header.Get("Location"); got != "/" {
		t.Errorf("Location = %q, want the site root", got)
	}
}

func TestOIDCRoutesAreNotFoundWhenUnconfigured(t *testing.T) {
	// An instance with no provider has nothing to redirect to. A 404 is the
	// honest answer; sending the browser to a provider nobody configured would
	// be a redirect an operator cannot account for.
	srv, _ := authDashboard(t)
	h := srv.Routes()
	if res := getPath(t, h, oidcStartPath); res.StatusCode != http.StatusNotFound {
		t.Errorf("GET %s = %d, want 404", oidcStartPath, res.StatusCode)
	}
	res := getPath(t, h, oidcCallbackPath+"?state=x&code=y")
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("GET %s = %d, want 404", oidcCallbackPath, res.StatusCode)
	}
	if c := cookieOf(t, res, auth.StateCookie); c != nil {
		t.Error("an unconfigured callback set a cookie")
	}
}

func TestLocalLoginStillWorksWithAProviderConfigured(t *testing.T) {
	// The claim the issue states outright: OIDC is additive. This is the case
	// that breaks if someone "simplifies" the sign-in page to one path.
	idp := oidctest.New(t)
	h, a := ssoDashboard(t, idp, nil, nil, testToken)
	res := signIn(t, h, testToken, "/tokens")
	defer res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("token sign-in = %d:\n%s", res.StatusCode, readBody(t, res))
	}
	session := cookieOf(t, res, auth.SessionCookie)
	if session == nil || !a.HasSession(session.Value) {
		t.Fatal("the token form no longer mints a session once SSO is configured")
	}
}

func TestSignOutWorksAfterAnSSOLogin(t *testing.T) {
	// A session is a session: the sign-out button, the cookie clearing and the
	// gate all behave the same way, which is what made the in-memory session
	// the right thing for SSO to produce.
	idp := oidctest.New(t)
	h, a := ssoDashboard(t, idp, nil, nil, testToken)
	q, cookies := beginLogin(t, h, idp, "/alerts", nil)
	res := callback(t, h, q, cookies, nil)
	session := cookieOf(t, res, auth.SessionCookie)
	if session == nil {
		t.Fatal("no session to sign out of")
	}
	_ = readBody(t, res)
	if status := readStatus(t, signOut(t, h, session)); status != http.StatusSeeOther {
		t.Fatalf("POST %s = %d, want the sign-out redirect", logoutPath, status)
	}
	if a.HasSession(session.Value) {
		t.Error("signing out left the SSO session live")
	}
	if gated := getPath(t, h, "/alerts", session); gated.StatusCode == http.StatusOK {
		t.Error("the dropped session still reaches a gated page")
	}
}
