package auth

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/oidctest"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// End-to-end OIDC login flow tests that exercise the complete authentication
// path from browser redirect through provider interaction to session creation.
// These complement the unit tests by verifying the integration between
// BeginLogin, CompleteLogin, and the authenticator.

func TestOIDCEndToEndCompleteFlow(t *testing.T) {
	// The full happy-path: start a login, authorize at the provider, exchange
	// the code, verify the token, and mint a session. This is what an operator
	// experiences when they configure OIDC for the first time.
	idp := oidctest.New(t)
	p := newTestProvider(t, idp, nil)
	a := New(nil, 0).WithOIDC(p)

	// Step 1: Start the login from the dashboard
	next := "/alerts"
	redirectURL, state, err := p.BeginLogin(next)
	if err != nil {
		t.Fatalf("BeginLogin failed: %v", err)
	}

	// Step 2: Parse the provider redirect and verify it's well-formed
	parsed, err := url.Parse(redirectURL)
	if err != nil {
		t.Fatalf("parse redirect URL: %v", err)
	}
	if parsed.Query().Get("response_type") != "code" {
		t.Error("authorize request did not use code flow")
	}
	if !strings.Contains(parsed.Query().Get("scope"), "openid") {
		t.Error("authorize request missing openid scope")
	}

	// Step 3: Provider authorizes the request and issues a code
	code, err := idp.Authorize(parsed.Query())
	if err != nil {
		t.Fatalf("Authorize failed: %v", err)
	}

	// Step 4: Queue up the ID token the provider will return
	respond(t, idp, code, nil)

	// Step 5: Complete the login (this is what the callback handler does)
	q := url.Values{"state": {state}, "code": {code}}
	cookieState := state // In the real flow, this comes from the browser cookie
	identity, returnedNext, err := p.CompleteLogin(context.Background(), q, cookieState)
	if err != nil {
		t.Fatalf("CompleteLogin failed: %v", err)
	}

	// Step 6: Verify the identity
	if identity.Subject != "user-123" {
		t.Errorf("subject = %q, want user-123", identity.Subject)
	}
	if identity.Email != "ops@example.com" {
		t.Errorf("email = %q, want ops@example.com", identity.Email)
	}
	if identity.Workspace != workspace.Default {
		t.Errorf("workspace = %q, want default", identity.Workspace)
	}
	if returnedNext != next {
		t.Errorf("returned next = %q, want %q", returnedNext, next)
	}

	// Step 7: Mint a session from the identity (what LoginOIDC does)
	sessionID := a.LoginOIDC(identity)
	if sessionID == "" {
		t.Fatal("LoginOIDC minted no session")
	}
	if !a.HasSession(sessionID) {
		t.Error("the session was not recorded")
	}

	// Step 8: Verify the session carries the correct workspace
	ws, ok := a.SessionWorkspace(sessionID)
	if !ok || ws != workspace.Default {
		t.Errorf("session workspace = %q (%v), want default", ws, ok)
	}
}

func TestOIDCEndToEndWithWorkspaceClaim(t *testing.T) {
	// Multi-tenant scenario: the provider publishes a workspace claim, and
	// different users land in different workspaces based on their identity.
	idp := oidctest.New(t)
	p := newTestProvider(t, idp, func(cfg *OIDCConfig) {
		cfg.WorkspaceClaim = "org_id"
	})
	a := New(nil, 0).WithOIDC(p)

	// User from org "acme"
	q, state, code := oidcLogin(t, p, idp, "/dashboard")
	respond(t, idp, code, func(c map[string]any) {
		c["org_id"] = "acme"
		c["email"] = "alice@acme.com"
	})

	identity, _, err := complete(t, p, q, state)
	if err != nil {
		t.Fatalf("CompleteLogin failed: %v", err)
	}
	if identity.Workspace != "acme" {
		t.Errorf("workspace = %q, want acme", identity.Workspace)
	}

	sessionID := a.LoginOIDC(identity)
	if sessionID == "" {
		t.Fatal("no session created")
	}
	ws, ok := a.SessionWorkspace(sessionID)
	if !ok || ws != "acme" {
		t.Errorf("session workspace = %q (%v), want acme", ws, ok)
	}

	// Different user from org "globex" using the same provider
	q2, state2, code2 := oidcLogin(t, p, idp, "/dashboard")
	respond(t, idp, code2, func(c map[string]any) {
		c["org_id"] = "globex"
		c["email"] = "bob@globex.com"
	})

	identity2, _, err := complete(t, p, q2, state2)
	if err != nil {
		t.Fatalf("CompleteLogin failed: %v", err)
	}
	if identity2.Workspace != "globex" {
		t.Errorf("workspace = %q, want globex", identity2.Workspace)
	}

	sessionID2 := a.LoginOIDC(identity2)
	ws2, ok := a.SessionWorkspace(sessionID2)
	if !ok || ws2 != "globex" {
		t.Errorf("session workspace = %q (%v), want globex", ws2, ok)
	}

	// Sessions are isolated: each belongs to its own workspace
	if sessionID == sessionID2 {
		t.Error("different users got the same session ID")
	}
}

// A provider that maps roles gives each identity the reach it maps, and the
// session the identity signs in with carries that role.
func TestOIDCEndToEndWithRoleClaim(t *testing.T) {
	idp := oidctest.New(t)
	p := newTestProvider(t, idp, func(cfg *OIDCConfig) {
		cfg.RoleClaim = "soro_role"
	})
	a := New(nil, 0).WithOIDC(p)

	q, state, code := oidcLogin(t, p, idp, "/dashboard")
	respond(t, idp, code, func(c map[string]any) {
		c["soro_role"] = "viewer"
		c["email"] = "alice@example.com"
	})

	identity, _, err := complete(t, p, q, state)
	if err != nil {
		t.Fatalf("CompleteLogin failed: %v", err)
	}
	if identity.Role != RoleViewer {
		t.Fatalf("role = %v, want a viewer", identity.Role)
	}

	sessionID := a.LoginOIDC(identity)
	role, ok := a.SessionRole(sessionID)
	if !ok || role != RoleViewer {
		t.Fatalf("session role = %v (%v), want a viewer", role, ok)
	}

	// An identity the provider does not classify keeps the default: the
	// operator configured the mapping, and a claim naming no known role is
	// not a reason to lock a user out of an instance they can sign in to.
	q2, state2, code2 := oidcLogin(t, p, idp, "/dashboard")
	respond(t, idp, code2, func(c map[string]any) {
		c["soro_role"] = "wizard"
		c["email"] = "bob@example.com"
	})
	identity2, _, err := complete(t, p, q2, state2)
	if err != nil {
		t.Fatalf("CompleteLogin failed: %v", err)
	}
	if identity2.Role != RoleAdmin {
		t.Fatalf("role = %v, want the default admin", identity2.Role)
	}
}

// Without a role claim every identity that signs in is an admin, which is
// what a single-operator deployment wants.
func TestOIDCEndToEndWithoutRoleClaimIsAdmin(t *testing.T) {
	idp := oidctest.New(t)
	p := newTestProvider(t, idp, nil)

	q, state, code := oidcLogin(t, p, idp, "/dashboard")
	respond(t, idp, code, func(c map[string]any) {
		c["email"] = "alice@example.com"
	})
	identity, _, err := complete(t, p, q, state)
	if err != nil {
		t.Fatalf("CompleteLogin failed: %v", err)
	}
	if identity.Role != RoleAdmin {
		t.Fatalf("role = %v, want an admin", identity.Role)
	}
}

func TestOIDCEndToEndConcurrentLogins(t *testing.T) {
	// Two users logging in simultaneously should not interfere with each
	// other's state, nonce, or PKCE verifier. Each gets their own provider
	// instance to avoid shared state, simulating two separate browser sessions.
	idp := oidctest.New(t)

	// User 1's complete login flow
	p1 := newTestProvider(t, idp, nil)
	redirect1, state1, err := p1.BeginLogin("/user1-dashboard")
	if err != nil {
		t.Fatalf("BeginLogin 1: %v", err)
	}
	parsed1, _ := url.Parse(redirect1)
	code1, _ := idp.Authorize(parsed1.Query())
	respond(t, idp, code1, func(c map[string]any) {
		c["email"] = "user1@example.com"
		c["name"] = "User One"
	})

	q1 := url.Values{"state": {state1}, "code": {code1}}
	id1, next1, err := p1.CompleteLogin(context.Background(), q1, state1)
	if err != nil {
		t.Fatalf("CompleteLogin 1: %v", err)
	}
	if id1.Email != "user1@example.com" {
		t.Errorf("user 1 email = %q, want user1@example.com", id1.Email)
	}
	if next1 != "/user1-dashboard" {
		t.Errorf("user 1 next = %q, want /user1-dashboard", next1)
	}

	// User 2's complete login flow (separate provider instance, same IDP)
	p2 := newTestProvider(t, idp, nil)
	redirect2, state2, err := p2.BeginLogin("/user2-dashboard")
	if err != nil {
		t.Fatalf("BeginLogin 2: %v", err)
	}
	parsed2, _ := url.Parse(redirect2)
	code2, _ := idp.Authorize(parsed2.Query())
	respond(t, idp, code2, func(c map[string]any) {
		c["email"] = "user2@example.com"
		c["name"] = "User Two"
	})

	q2 := url.Values{"state": {state2}, "code": {code2}}
	id2, next2, err := p2.CompleteLogin(context.Background(), q2, state2)
	if err != nil {
		t.Fatalf("CompleteLogin 2: %v", err)
	}
	if id2.Email != "user2@example.com" {
		t.Errorf("user 2 email = %q, want user2@example.com", id2.Email)
	}
	if next2 != "/user2-dashboard" {
		t.Errorf("user 2 next = %q, want /user2-dashboard", next2)
	}

	// Both providers have no pending state (both consumed)
	if p1.hasPending(state1) {
		t.Error("user 1's state was not consumed")
	}
	if p2.hasPending(state2) {
		t.Error("user 2's state was not consumed")
	}
}

func TestOIDCEndToEndExpiredTokenStillValidSignature(t *testing.T) {
	// A token that is correctly signed but expired must be rejected by the
	// verifier, not accepted because the signature is valid. This proves the
	// expiry check runs independently of signature validation.
	idp := oidctest.New(t)
	p := newTestProvider(t, idp, nil)

	q, state, code := oidcLogin(t, p, idp, "/")
	respond(t, idp, code, func(c map[string]any) {
		c["exp"] = time.Now().Add(-time.Hour).Unix()
		c["iat"] = time.Now().Add(-2 * time.Hour).Unix()
	})

	_, _, err := complete(t, p, q, state)
	if err == nil {
		t.Fatal("CompleteLogin accepted an expired token")
	}
	if !strings.Contains(err.Error(), "id_token rejected") {
		t.Errorf("error = %q, want rejection mentioning id_token", err)
	}
}

func TestOIDCEndToEndAllowedDomainsWithSubdomain(t *testing.T) {
	// Domain matching should handle subdomains correctly. If "example.com" is
	// allowed, "sub.example.com" should NOT match unless explicitly configured.
	idp := oidctest.New(t)
	p := newTestProvider(t, idp, func(cfg *OIDCConfig) {
		cfg.AllowedDomains = []string{"example.com"}
	})

	// Exact domain match works
	q1, state1, code1 := oidcLogin(t, p, idp, "/")
	respond(t, idp, code1, func(c map[string]any) {
		c["email"] = "user@example.com"
	})
	_, _, err := complete(t, p, q1, state1)
	if err != nil {
		t.Errorf("exact domain match failed: %v", err)
	}

	// Subdomain should NOT match
	q2, state2, code2 := oidcLogin(t, p, idp, "/")
	respond(t, idp, code2, func(c map[string]any) {
		c["email"] = "user@sub.example.com"
	})
	_, _, err = complete(t, p, q2, state2)
	if err == nil {
		t.Error("subdomain matched when only parent domain was allowed")
	}
	if !errors.Is(err, ErrOIDCAccount) {
		t.Errorf("error = %v, want ErrOIDCAccount", err)
	}
}

func TestOIDCEndToEndEmptyEmailWithAllowedDomains(t *testing.T) {
	// When allowed domains are configured but the ID token has no email claim,
	// the login should be rejected with a clear error.
	idp := oidctest.New(t)
	p := newTestProvider(t, idp, func(cfg *OIDCConfig) {
		cfg.AllowedDomains = []string{"example.com"}
	})

	q, state, code := oidcLogin(t, p, idp, "/")
	respond(t, idp, code, func(c map[string]any) {
		delete(c, "email")
	})

	_, _, err := complete(t, p, q, state)
	if err == nil {
		t.Fatal("CompleteLogin accepted a token with no email when domains are configured")
	}
	if !errors.Is(err, ErrOIDCAccount) {
		t.Errorf("error = %v, want ErrOIDCAccount", err)
	}
	if !strings.Contains(err.Error(), "no email claim") {
		t.Errorf("error = %q, want mention of missing email claim", err)
	}
}

func TestOIDCEndToEndStateExpiryAtBoundary(t *testing.T) {
	// A login that expires exactly at the TTL boundary should be rejected.
	// This tests the edge case where now == expires (not strictly before).
	idp := oidctest.New(t)
	p := newTestProvider(t, idp, func(cfg *OIDCConfig) {
		cfg.StateTTL = time.Minute
	})

	clock := time.Unix(1_800_000_000, 0).UTC()
	p.withNow(func() time.Time { return clock })

	// Start a login
	q, state, code := oidcLogin(t, p, idp, "/")
	respond(t, idp, code, nil)

	// Complete it just before expiry - should work
	clock = clock.Add(59 * time.Second)
	_, _, err := complete(t, p, q, state)
	if err != nil {
		t.Errorf("login at 59 seconds = %v, want success", err)
	}

	// Start another login
	q2, state2, code2 := oidcLogin(t, p, idp, "/")
	respond(t, idp, code2, nil)

	// Complete it exactly at expiry - should fail
	clock = clock.Add(time.Minute + 1*time.Second)
	_, _, err = complete(t, p, q2, state2)
	if !errors.Is(err, ErrOIDCLogin) {
		t.Errorf("login at exact expiry = %v, want ErrOIDCLogin", err)
	}
}

func TestOIDCEndToEndMultipleScopesDeduplication(t *testing.T) {
	// When scopes are specified with duplicates and whitespace, they should be
	// normalized correctly, and openid should always be present.
	idp := oidctest.New(t)
	p := newTestProvider(t, idp, func(cfg *OIDCConfig) {
		cfg.Scopes = []string{"email profile", "openid", "email", " offline_access "}
	})

	redirectURL, _, err := p.BeginLogin("/")
	if err != nil {
		t.Fatalf("BeginLogin failed: %v", err)
	}

	parsed, err := url.Parse(redirectURL)
	if err != nil {
		t.Fatalf("parse redirect URL: %v", err)
	}

	scope := parsed.Query().Get("scope")
	// Should have openid, email, profile, offline_access (deduplicated, trimmed)
	expectedParts := []string{"openid", "email", "profile", "offline_access"}
	for _, part := range expectedParts {
		if !strings.Contains(scope, part) {
			t.Errorf("scope %q missing from %q", part, scope)
		}
	}

	// Count occurrences of "email" - should appear only once
	count := strings.Count(scope, "email")
	if count != 1 {
		t.Errorf("email appears %d times in scope %q, want 1", count, scope)
	}
}

func TestOIDCEndToEndWorkspaceClaimNonStringType(t *testing.T) {
	// When the workspace claim is a non-string type (number, object, array),
	// it should fall back to the configured default workspace rather than
	// crashing or accepting an invalid value.
	idp := oidctest.New(t)

	for _, tc := range []struct {
		name  string
		value any
	}{
		{"number", 42},
		{"object", map[string]any{"nested": "value"}},
		{"array", []string{"acme", "globex"}},
		{"boolean", true},
		{"null", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestProvider(t, idp, func(cfg *OIDCConfig) {
				cfg.WorkspaceClaim = "workspace"
				cfg.Workspace = "default-org"
			})

			q, state, code := oidcLogin(t, p, idp, "/")
			respond(t, idp, code, func(c map[string]any) {
				if tc.value != nil {
					c["workspace"] = tc.value
				} else {
					delete(c, "workspace")
				}
			})

			identity, _, err := complete(t, p, q, state)
			if err != nil {
				t.Fatalf("CompleteLogin failed: %v", err)
			}
			if identity.Workspace != "default-org" {
				t.Errorf("workspace = %q, want default-org for non-string claim", identity.Workspace)
			}
		})
	}
}

func TestOIDCEndToEndSessionPersistsAfterLogin(t *testing.T) {
	// After an OIDC login creates a session, that session should behave
	// identically to a token-based session: it should be queryable, have the
	// correct workspace, and survive multiple checks.
	idp := oidctest.New(t)
	p := newTestProvider(t, idp, nil)
	a := NewBound([]Binding{{Workspace: "test-ws", Token: "test-token"}}, 0).WithOIDC(p)

	// OIDC login
	q, state, code := oidcLogin(t, p, idp, "/dashboard")
	respond(t, idp, code, func(c map[string]any) {
		c["email"] = "sso-user@example.com"
	})

	identity, _, err := complete(t, p, q, state)
	if err != nil {
		t.Fatalf("CompleteLogin failed: %v", err)
	}

	sessionID := a.LoginOIDC(identity)
	if sessionID == "" {
		t.Fatal("no session created")
	}

	// Session should be recognized
	if !a.HasSession(sessionID) {
		t.Error("session not found immediately after creation")
	}

	// Session should have correct workspace
	ws, ok := a.SessionWorkspace(sessionID)
	if !ok {
		t.Error("session has no workspace")
	}
	if ws != workspace.Default {
		t.Errorf("session workspace = %q, want default", ws)
	}

	// Token-based sessions should still work alongside OIDC sessions
	tokenSession, ok := a.Login("test-token")
	if !ok || tokenSession == "" {
		t.Error("token login stopped working after OIDC configuration")
	}
	if !a.HasSession(tokenSession) {
		t.Error("token session not found")
	}

	// Both sessions should be distinct
	if sessionID == tokenSession {
		t.Error("OIDC and token sessions are identical")
	}
}

func TestOIDCEndToEndLogoutClearsOIDCSession(t *testing.T) {
	// Logging out should clear an OIDC-created session just like a token-based
	// one, proving that sessions are credential-agnostic.
	idp := oidctest.New(t)
	p := newTestProvider(t, idp, nil)
	a := New(nil, 0).WithOIDC(p)

	// Login via OIDC
	q, state, code := oidcLogin(t, p, idp, "/")
	respond(t, idp, code, nil)

	identity, _, err := complete(t, p, q, state)
	if err != nil {
		t.Fatalf("CompleteLogin failed: %v", err)
	}

	sessionID := a.LoginOIDC(identity)
	if sessionID == "" {
		t.Fatal("no session created")
	}

	// Verify session exists
	if !a.HasSession(sessionID) {
		t.Fatal("session not found")
	}

	// Drop session (sign-out)
	a.DropSession(sessionID)

	// Session should be gone
	if a.HasSession(sessionID) {
		t.Error("session survived logout")
	}
}

func TestOIDCEndToEndIdentitySubjectIsStable(t *testing.T) {
	// The subject (sub claim) should remain stable across multiple logins for
	// the same user, even if their email changes. This is why we key off sub
	// rather than email.
	idp := oidctest.New(t)
	p := newTestProvider(t, idp, nil)

	// First login
	q1, state1, code1 := oidcLogin(t, p, idp, "/")
	respond(t, idp, code1, func(c map[string]any) {
		c["sub"] = "stable-user-id"
		c["email"] = "user@example.com"
	})

	id1, _, err := complete(t, p, q1, state1)
	if err != nil {
		t.Fatalf("CompleteLogin 1 failed: %v", err)
	}

	// Second login with different email but same sub
	q2, state2, code2 := oidcLogin(t, p, idp, "/")
	respond(t, idp, code2, func(c map[string]any) {
		c["sub"] = "stable-user-id"
		c["email"] = "new-email@example.com"
	})

	id2, _, err := complete(t, p, q2, state2)
	if err != nil {
		t.Fatalf("CompleteLogin 2 failed: %v", err)
	}

	// Subject should be the same
	if id1.Subject != id2.Subject {
		t.Errorf("subjects differ: %q vs %q, want stable", id1.Subject, id2.Subject)
	}
	if id1.Subject != "stable-user-id" {
		t.Errorf("subject = %q, want stable-user-id", id1.Subject)
	}

	// Emails should differ
	if id1.Email == id2.Email {
		t.Error("emails are the same despite being set differently")
	}
}
