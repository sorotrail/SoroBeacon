package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// RoleForRequest is what any role enforcement is built on, so its answer for
// every credential is pinned here: the three credentials answer it
// differently, and two functions on the same type used to disagree about
// whether a request was authenticated at all.
func TestRoleForRequestResolvesEveryCredential(t *testing.T) {
	// A static token's role comes from its suffix. The suffix is stripped
	// when the token table is built, so the client presents the bare token.
	a := New([]string{
		"plain-token",
		"viewer-token:viewer",
		"editor-token:editor",
		"admin-token:admin",
	}, 0)

	live := a.NewSessionWithRole(RoleEditor)

	tests := []struct {
		name     string
		request  func() *http.Request
		wantRole Role
		wantOK   bool
	}{
		{
			name:     "a static token without a suffix is an admin",
			request:  bearer("plain-token"),
			wantRole: RoleAdmin,
			wantOK:   true,
		},
		{
			name:     "a viewer suffix",
			request:  bearer("viewer-token"),
			wantRole: RoleViewer,
			wantOK:   true,
		},
		{
			name:     "an editor suffix",
			request:  bearer("editor-token"),
			wantRole: RoleEditor,
			wantOK:   true,
		},
		{
			name:     "an admin suffix",
			request:  bearer("admin-token"),
			wantRole: RoleAdmin,
			wantOK:   true,
		},
		{
			name:     "an unknown token has no role",
			request:  bearer("not-a-token"),
			wantRole: RoleUnknown,
			wantOK:   false,
		},
		{
			name:     "no credential at all has no role",
			request:  func() *http.Request { return httptest.NewRequest(http.MethodGet, "/", nil) },
			wantRole: RoleUnknown,
			wantOK:   false,
		},
		{
			name: "a live session carries the role it was minted with",
			request: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "/", nil)
				req.AddCookie(&http.Cookie{Name: SessionCookie, Value: live})
				return req
			},
			wantRole: RoleEditor,
			wantOK:   true,
		},
		{
			name: "an expired session has no role",
			request: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "/", nil)
				req.AddCookie(&http.Cookie{Name: SessionCookie, Value: expiredSession(t, a)})
				return req
			},
			wantRole: RoleUnknown,
			wantOK:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			role, ok := a.RoleForRequest(tt.request())
			if role != tt.wantRole || ok != tt.wantOK {
				t.Fatalf("RoleForRequest = (%v, %v), want (%v, %v)", role, ok, tt.wantRole, tt.wantOK)
			}
		})
	}
}

// An instance with no credential source is open, and an open instance is an
// admin one: the role check must not close it.
func TestRoleForRequestWithAuthenticationDisabledIsAdmin(t *testing.T) {
	a := New(nil, 0)
	role, ok := a.RoleForRequest(httptest.NewRequest(http.MethodGet, "/", nil))
	if !ok || role != RoleAdmin {
		t.Fatalf("RoleForRequest = (%v, %v), want (admin, true)", role, ok)
	}
}

// A scoped, database-backed token carries no role of its own: its scopes are
// the grant, and the role is the coarsest one those scopes allow.
//
// It used to resolve to RoleUnknown, which made Authenticated call a request
// carrying one unauthenticated while Principal accepted it — two functions
// on the same type answering the same question differently. The role is now
// derived from the grant, so the two agree.
func TestRoleForRequestResolvesAScopedToken(t *testing.T) {
	raw, _, mgr, _ := minted(t, ScopeMonitorsRead)
	a := New([]string{"static-token"}, 0).WithTokens(mgr)

	role, ok := a.RoleForRequest(bearer(raw)())
	if !ok {
		t.Fatal("a valid scoped token must resolve a role")
	}
	if role != RoleViewer {
		t.Fatalf("a read-only scoped token = %v, want a viewer", role)
	}

	writer, _, mgr2, _ := minted(t, ScopeMonitorsRead, ScopeMonitorsWrite)
	a2 := New([]string{"static-token"}, 0).WithTokens(mgr2)
	role2, ok := a2.RoleForRequest(bearer(writer)())
	if !ok {
		t.Fatal("a valid scoped token must resolve a role")
	}
	if role2 != RoleEditor {
		t.Fatalf("a scoped token with a write scope = %v, want an editor", role2)
	}
}

// Authenticated and Principal answer the same question — is this request
// carrying a credential this instance accepts? — so they are run over the
// same inputs and must agree.
func TestAuthenticatedAndPrincipalAgree(t *testing.T) {
	raw, _, mgr, _ := minted(t, ScopeMonitorsRead)
	a := New([]string{"static-token"}, 0).WithTokens(mgr)
	live := a.NewSessionWithRole(RoleViewer)

	cases := []struct {
		name    string
		request func() *http.Request
	}{
		{"a static token", bearer("static-token")},
		{"a scoped token", bearer(raw)},
		{"an unknown token", bearer("not-a-token")},
		{"no credential", func() *http.Request { return httptest.NewRequest(http.MethodGet, "/", nil) }},
		{"a live session", func() *http.Request {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.AddCookie(&http.Cookie{Name: SessionCookie, Value: live})
			return req
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			authenticated := a.Authenticated(tc.request())
			_, principalOK := a.Principal(tc.request())
			if authenticated != principalOK {
				t.Fatalf("Authenticated = %v but Principal resolved = %v; the two must agree",
					authenticated, principalOK)
			}
		})
	}
}

// A session minted with a role belongs to the default workspace: there is
// no other workspace it could belong to, and a session that named one would
// be a tenancy decision taken by the wrong code.
func TestNewSessionWithRoleUsesTheDefaultWorkspace(t *testing.T) {
	a := New([]string{"static-token"}, 0)
	id := a.NewSessionWithRole(RoleEditor)
	if id == "" {
		t.Fatal("NewSessionWithRole returned no session id")
	}
	ws, ok := a.SessionWorkspace(id)
	if !ok {
		t.Fatal("the session must resolve to a workspace")
	}
	if ws != workspace.Default {
		t.Fatalf("session workspace = %q, want %q", ws, workspace.Default)
	}
	role, ok := a.SessionRole(id)
	if !ok {
		t.Fatal("the session must carry a role")
	}
	if role != RoleEditor {
		t.Fatalf("session role = %v, want an editor", role)
	}
}

// The OIDC accessor is how a deployment reads its single-sign-on settings
// back, and a nil provider is the unconfigured case rather than a crash.
func TestOIDCAccessor(t *testing.T) {
	var a *Authenticator
	if a.OIDC() != nil {
		t.Fatal("a nil authenticator must report no provider")
	}
	if a.OIDC().Enabled() {
		t.Fatal("a nil provider must not report Enabled")
	}

	auth := New([]string{"static-token"}, 0)
	if auth.OIDC() != nil {
		t.Fatal("an authenticator without a provider must report none")
	}
	if auth.OIDC().Enabled() {
		t.Fatal("a nil provider must not report Enabled")
	}
}

// bearer builds a request carrying a bearer token.
func bearer(token string) func() *http.Request {
	return func() *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		return req
	}
}

// expiredSession mints a session and moves the clock past its expiry, so
// the test does not have to sleep.
func expiredSession(t *testing.T, a *Authenticator) string {
	t.Helper()
	now := time.Now()
	a.now = func() time.Time { return now }
	id := a.NewSessionWithRole(RoleViewer)
	a.now = func() time.Time { return now.Add(2 * DefaultSessionTTL) }
	t.Cleanup(func() { a.now = time.Now })
	return id
}

// A context carries the role the authentication step resolved, so the role
// check does not resolve the credential a second time.
func TestRoleContextRoundTrip(t *testing.T) {
	ctx := WithRole(context.Background(), RoleEditor)
	role, ok := RoleFrom(ctx)
	if !ok || role != RoleEditor {
		t.Fatalf("RoleFrom = (%v, %v), want (editor, true)", role, ok)
	}
	if _, ok := RoleFrom(context.Background()); ok {
		t.Fatal("a context without a role must report none")
	}
}
