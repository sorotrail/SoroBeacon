package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestVerifyAcceptsConfiguredTokens(t *testing.T) {
	// Two tokens is the rotation case: both have to work at once, otherwise
	// an operator cannot add the new token before removing the old one.
	a := New([]string{"old-token", " new-token "}, 0)
	for _, token := range []string{"old-token", "new-token"} {
		if !a.Verify(token) {
			t.Errorf("Verify(%q) = false, want true", token)
		}
	}
}

func TestVerifyRejectsEverythingElse(t *testing.T) {
	a := New([]string{"correct-horse"}, 0)
	for name, candidate := range map[string]string{
		"empty":           "",
		"wrong":           "correct-horse-battery",
		"prefix":          "correct",
		"superset":        "correct-horse-staple",
		"differing case":  "Correct-Horse",
		"trailing space":  "correct-horse ",
		"newline suffix":  "correct-horse\n",
		"only whitespace": "   ",
	} {
		if a.Verify(candidate) {
			t.Errorf("Verify(%q) = true for %s, want false", candidate, name)
		}
	}
}

func TestVerifyUnconfiguredIsNeverTrue(t *testing.T) {
	a := New(nil, 0)
	if a.Enabled() {
		t.Fatal("no tokens must mean auth is disabled")
	}
	if a.Verify("") || a.Verify("anything") {
		t.Fatal("an unconfigured authenticator must not verify any token")
	}
}

// A nil *Authenticator is what a server built without WithAuth holds, so the
// methods have to be safe to call on it.
func TestNilAuthenticatorIsDisabledAndSafe(t *testing.T) {
	var a *Authenticator
	if a.Enabled() {
		t.Fatal("nil authenticator must not report Enabled")
	}
	if a.Verify("x") {
		t.Fatal("nil authenticator must not verify")
	}
	if a.HasSession("x") {
		t.Fatal("nil authenticator must not report a session")
	}
	a.DropSession("x") // must not panic
	if !a.Authenticated(httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Fatal("nil authenticator must leave requests open")
	}
}

func TestBearerParsesSchemeCaseInsensitively(t *testing.T) {
	for _, header := range []string{"Bearer abc", "bearer abc", "BEARER abc", "Bearer   abc  "} {
		token, ok := Bearer(header)
		if !ok || token != "abc" {
			t.Errorf("Bearer(%q) = %q, %v; want abc, true", header, token, ok)
		}
	}
	for name, header := range map[string]string{
		"empty":         "",
		"scheme only":   "Bearer",
		"blank token":   "Bearer ",
		"other scheme":  "Basic dXNlcjpwYXNz",
		"no scheme":     "abc",
		"prefix only":   "Beare abc",
		"leading space": " Bearer abc",
		"query-shaped":  "?token=abc",
		"tab separated": "Bearer\tabc",
	} {
		if token, ok := Bearer(header); ok {
			t.Errorf("Bearer(%q) = %q, true for %s; want no token", header, token, name)
		}
	}
}

func TestLoginMintsSessionOnlyForAValidToken(t *testing.T) {
	a := New([]string{"s3cret"}, 0)

	if id, ok := a.Login("wrong"); ok || id != "" {
		t.Fatalf("Login(wrong) = %q, %v; want no session", id, ok)
	}
	if id, ok := a.Login(""); ok || id != "" {
		t.Fatalf("Login(empty) = %q, %v; want no session", id, ok)
	}

	id, ok := a.Login("s3cret")
	if !ok || id == "" {
		t.Fatalf("Login(s3cret) = %q, %v; want a session", id, ok)
	}
	if !a.HasSession(id) {
		t.Fatal("a freshly minted session must be live")
	}
	if !a.Enabled() {
		t.Fatal("configured tokens must report Enabled")
	}
}

func TestSessionIDsAreOpaqueAndUnique(t *testing.T) {
	a := New([]string{"s3cret"}, 0)
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		id := a.NewSession()
		if len(id) < 32 {
			t.Fatalf("session id %q is shorter than 32 chars; it must not be guessable", id)
		}
		if seen[id] {
			t.Fatalf("duplicate session id %q", id)
		}
		seen[id] = true
	}
}

func TestHasSessionExpiresAndDrops(t *testing.T) {
	a := New([]string{"s3cret"}, time.Minute)
	now := time.Unix(1_700_000_000, 0)
	a.now = func() time.Time { return now }

	id := a.NewSession()
	if !a.HasSession(id) {
		t.Fatal("session must be live before its TTL elapses")
	}

	now = now.Add(59 * time.Second)
	if !a.HasSession(id) {
		t.Fatal("session must still be live just inside its TTL")
	}

	now = now.Add(2 * time.Second)
	if a.HasSession(id) {
		t.Fatal("session must expire once its TTL elapses")
	}
	if a.HasSession(id) {
		t.Fatal("an expired session must stay gone")
	}

	first := a.NewSession()
	second := a.NewSession()
	a.DropSession(first)
	if a.HasSession(first) {
		t.Fatal("DropSession must end that session")
	}
	if !a.HasSession(second) {
		t.Fatal("dropping one session must not sign the others out")
	}
	a.DropSession("never-issued") // must not panic or disturb live sessions
	if !a.HasSession(second) {
		t.Fatal("an unknown session id must not change anything")
	}
}

func TestZeroTTLFallsBackToDefault(t *testing.T) {
	a := New([]string{"s3cret"}, 0)
	if a.SessionTTL() != DefaultSessionTTL {
		t.Fatalf("SessionTTL() = %v, want %v", a.SessionTTL(), DefaultSessionTTL)
	}
}

func requestWithHeader(name, value string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/monitors", nil)
	if name != "" {
		r.Header.Set(name, value)
	}
	return r
}

func TestAuthenticatedAcceptsBearerOrSessionCookie(t *testing.T) {
	a := New([]string{"s3cret"}, 0)
	sessionID, ok := a.Login("s3cret")
	if !ok {
		t.Fatal("Login failed")
	}

	tests := map[string]struct {
		req  *http.Request
		want bool
	}{
		"valid bearer": {requestWithHeader("Authorization", "Bearer s3cret"), true},
		"wrong bearer": {requestWithHeader("Authorization", "Bearer nope"), false},
		"basic":        {requestWithHeader("Authorization", "Basic czNjcmV0"), false},
		"no header":    {requestWithHeader("", ""), false},
		"live cookie": {func() *http.Request {
			r := requestWithHeader("", "")
			r.AddCookie(&http.Cookie{Name: SessionCookie, Value: sessionID})
			return r
		}(), true},
		"made-up cookie": {func() *http.Request {
			r := requestWithHeader("", "")
			r.AddCookie(&http.Cookie{Name: SessionCookie, Value: strings.Repeat("a", 43)})
			return r
		}(), false},
	}
	for name, tc := range tests {
		if got := a.Authenticated(tc.req); got != tc.want {
			t.Errorf("Authenticated(%s) = %v, want %v", name, got, tc.want)
		}
	}
}

func TestSessionIDReadsTheCookie(t *testing.T) {
	r := requestWithHeader("", "")
	if got := SessionID(r); got != "" {
		t.Fatalf("SessionID without a cookie = %q, want empty", got)
	}
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: "abc"})
	if got := SessionID(r); got != "abc" {
		t.Fatalf("SessionID = %q, want abc", got)
	}
	if got := SessionID(nil); got != "" {
		t.Fatalf("SessionID(nil) = %q, want empty", got)
	}
}
