package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/auth"
)

const testToken = "s3cret-token"

// authDashboard is a dashboard server with authentication configured, plus
// the authenticator behind it so a test can mint sessions directly.
func authDashboard(t *testing.T) (*Server, *auth.Authenticator) {
	t.Helper()
	a := auth.New([]string{testToken}, 0)
	return newTestServer(t).WithAuth(a), a
}

func getPath(t *testing.T, h http.Handler, path string, cookies ...*http.Cookie) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	return res.Result()
}

func signIn(t *testing.T, h http.Handler, token, next string) *http.Response {
	t.Helper()
	form := url.Values{"token": {token}}
	if next != "" {
		form.Set("next", next)
	}
	req := httptest.NewRequest(http.MethodPost, loginPath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	return res.Result()
}

// sessionCookieOf returns the session cookie a sign-in response set, or nil.
func sessionCookieOf(t *testing.T, res *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range res.Cookies() {
		if c.Name == auth.SessionCookie {
			return c
		}
	}
	return nil
}

// With no token configured the dashboard stays exactly as it was: no
// redirect, no cookie, no sign-in page to get stuck on. This is the case the
// docker-compose quickstart and every existing deployment run in.
func TestDashboardUnconfiguredStaysOpen(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t).Routes())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/monitors")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("unconfigured GET /monitors = %d, want 200", res.StatusCode)
	}
	if c := sessionCookieOf(t, res); c != nil {
		t.Fatalf("unconfigured dashboard set a session cookie: %v", c)
	}

	// /login has nothing to offer, so it must not trap the operator on a
	// form that cannot succeed.
	res, err = http.Get(srv.URL + loginPath)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("unconfigured GET /login = %d, want a redirect followed to the overview", res.StatusCode)
	}
}

func TestDashboardRedirectsAnonymousBrowsersToLogin(t *testing.T) {
	s, _ := authDashboard(t)
	h := s.Routes()

	for _, path := range []string{"/", "/monitors", "/channels", "/alerts", "/alerts/1"} {
		res := getPath(t, h, path)
		if res.StatusCode != http.StatusSeeOther {
			t.Fatalf("GET %s without a session = %d, want 303", path, res.StatusCode)
		}
		loc := res.Header.Get("Location")
		if !strings.HasPrefix(loc, loginPath+"?next=") {
			t.Fatalf("GET %s redirected to %q, want the sign-in page", path, loc)
		}
		if !strings.Contains(loc, url.QueryEscape(path)) {
			t.Fatalf("GET %s lost its return path: %q", path, loc)
		}
	}
}

// The sign-in page and the icon have to load before anyone has a session,
// otherwise the browser shows a redirect loop instead of a form.
func TestDashboardExemptsLoginPageAndFavicon(t *testing.T) {
	s, _ := authDashboard(t)
	h := s.Routes()

	res := getPath(t, h, loginPath)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /login = %d, want 200", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "API token") {
		t.Fatalf("sign-in page does not ask for the token:\n%s", body)
	}
	if strings.Contains(string(body), `action="/logout"`) {
		t.Fatal("the sign-in page offers a sign-out button, which is a dead end there")
	}
	if res := getPath(t, h, "/favicon.ico"); res.StatusCode != http.StatusOK {
		t.Fatalf("GET /favicon.ico = %d, want 200", res.StatusCode)
	}
}

func TestDashboardRejectsWrongToken(t *testing.T) {
	s, _ := authDashboard(t)
	h := s.Routes()

	res := signIn(t, h, "not-the-token", "/monitors")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("sign-in with a wrong token = %d, want 401", res.StatusCode)
	}
	if c := sessionCookieOf(t, res); c != nil {
		t.Fatalf("a failed sign-in must not start a session, got %v", c)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "not-the-token") {
		t.Fatalf("the sign-in page echoed the submitted token:\n%s", body)
	}
	// The form comes back so the operator can retry, with the reason on it.
	if !strings.Contains(string(body), "not accepted") {
		t.Fatalf("failed sign-in page gives no feedback:\n%s", body)
	}
}

func TestDashboardSignInStartsASession(t *testing.T) {
	s, a := authDashboard(t)
	h := s.Routes()

	res := signIn(t, h, testToken, "/monitors")
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign-in = %d, want 303", res.StatusCode)
	}
	if loc := res.Header.Get("Location"); loc != "/monitors" {
		t.Fatalf("sign-in redirected to %q, want /monitors", loc)
	}

	c := sessionCookieOf(t, res)
	if c == nil {
		t.Fatal("sign-in set no session cookie")
	}
	if !c.HttpOnly {
		t.Error("session cookie must be HttpOnly so page scripts cannot read it")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax so cross-site POSTs cannot carry it", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("Path = %q, want / so the API routes see it too", c.Path)
	}
	if !a.HasSession(c.Value) {
		t.Error("cookie value is not a live server-side session")
	}

	// With the cookie, the pages serve.
	if res := getPath(t, h, "/monitors", c); res.StatusCode != http.StatusOK {
		t.Fatalf("GET /monitors with a session = %d, want 200", res.StatusCode)
	}
	// ...and the sign-out button appears, because there is something to
	// sign out of.
	res2 := getPath(t, h, "/monitors", c)
	body, err := io.ReadAll(res2.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `action="/logout"`) {
		t.Fatal("authenticated dashboard offers no sign-out control")
	}
}

func TestDashboardSignOutEndsTheSession(t *testing.T) {
	s, a := authDashboard(t)
	h := s.Routes()

	c := sessionCookieOf(t, signIn(t, h, testToken, "/"))
	if c == nil {
		t.Fatal("sign-in set no session cookie")
	}

	req := httptest.NewRequest(http.MethodPost, logoutPath, nil)
	req.AddCookie(c)
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	out := res.Result()

	if out.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign-out = %d, want 303", out.StatusCode)
	}
	if loc := out.Header.Get("Location"); loc != loginPath {
		t.Fatalf("sign-out redirected to %q, want %s", loc, loginPath)
	}
	cleared := sessionCookieOf(t, out)
	if cleared == nil || cleared.MaxAge >= 0 {
		t.Fatalf("sign-out did not clear the cookie: %v", cleared)
	}
	if a.HasSession(c.Value) {
		t.Fatal("sign-out left the server-side session live")
	}
	if res := getPath(t, h, "/", c); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("a signed-out cookie still served /: %d", res.StatusCode)
	}
}

// A POST is answered with a redirect to the sign-in page rather than a
// resubmission prompt, and the body is dropped: an unauthenticated write
// must never be replayed once the operator signs in.
func TestDashboardAnonymousPostRedirectsWithoutReplaying(t *testing.T) {
	s, _ := authDashboard(t)
	h := s.Routes()

	form := url.Values{"name": {"sneaky"}, "contract_ids": {"CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABSC4"}}
	req := httptest.NewRequest(http.MethodPost, "/monitors", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	out := res.Result()

	if out.StatusCode != http.StatusSeeOther {
		t.Fatalf("anonymous POST /monitors = %d, want 303", out.StatusCode)
	}
	if loc := out.Header.Get("Location"); !strings.HasPrefix(loc, loginPath) {
		t.Fatalf("anonymous POST redirected to %q, want the sign-in page", loc)
	}
}

// The sign-in form's return path must stay on this site, or the login link
// becomes an open redirect that lends the dashboard's domain to someone
// else's page.
func TestDashboardSignInIgnoresOffSiteReturnPaths(t *testing.T) {
	s, _ := authDashboard(t)
	h := s.Routes()

	for _, next := range []string{"//evil.example/x", "https://evil.example/x", "\\\\evil.example", ""} {
		res := signIn(t, h, testToken, next)
		if loc := res.Header.Get("Location"); loc != "/" {
			t.Fatalf("next=%q redirected to %q, want /", next, loc)
		}
	}

	// A same-site path is honoured.
	if loc := signIn(t, h, testToken, "/alerts?sort=created_at_asc").Header.Get("Location"); loc != "/alerts?sort=created_at_asc" {
		t.Fatalf("same-site next was dropped: %q", loc)
	}
}

// A session id that was never issued is not a credential, even though it
// looks like one.
func TestDashboardRejectsForgedSessionCookie(t *testing.T) {
	s, _ := authDashboard(t)
	h := s.Routes()

	forged := &http.Cookie{Name: auth.SessionCookie, Value: strings.Repeat("A", 43)}
	if res := getPath(t, h, "/monitors", forged); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("forged session cookie = %d, want 303 to the sign-in page", res.StatusCode)
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"":                  "/",
		"/":                 "/",
		"/monitors":         "/monitors",
		"/alerts?a=b":       "/alerts?a=b",
		"//evil.example":    "/",
		"/\\evil.example":   "/",
		"https://evil":      "/",
		"javascript:alert1": "/",
		"monitors":          "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}
