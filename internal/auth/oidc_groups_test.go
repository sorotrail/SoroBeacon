package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/oidctest"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

func TestOIDCGroupMappingSingleAndMultiGroupPrecedence(t *testing.T) {
	mappings := []GroupMapping{
		{Group: "prod-viewers", Workspace: "prod", Role: RoleViewer},
		{Group: "prod-editors", Workspace: "prod", Role: RoleEditor},
		{Group: "prod-admins", Workspace: "prod", Role: RoleAdmin},
		{Group: "staging-editors", Workspace: "staging", Role: RoleEditor},
		{Group: "staging-admins", Workspace: "staging", Role: RoleAdmin},
		{Group: "audit-viewers", Workspace: "audit", Role: RoleViewer},
	}

	cases := []struct {
		name      string
		groups    any
		wantWS    workspace.ID
		wantRole  Role
		wantRoles map[workspace.ID]Role
	}{
		{
			name:     "single group viewer",
			groups:   []string{"prod-viewers"},
			wantWS:   "prod",
			wantRole: RoleViewer,
			wantRoles: map[workspace.ID]Role{
				"prod": RoleViewer,
			},
		},
		{
			name:     "single group string claim",
			groups:   "staging-editors",
			wantWS:   "staging",
			wantRole: RoleEditor,
			wantRoles: map[workspace.ID]Role{
				"staging": RoleEditor,
			},
		},
		{
			name:     "same workspace multiple groups resolves highest role regardless of claim order",
			groups:   []string{"prod-admins", "prod-viewers", "prod-editors"},
			wantWS:   "prod",
			wantRole: RoleAdmin,
			wantRoles: map[workspace.ID]Role{
				"prod": RoleAdmin,
			},
		},
		{
			name:     "across workspaces higher role wins over earlier lower role",
			groups:   []string{"prod-viewers", "staging-editors"},
			wantWS:   "staging",
			wantRole: RoleEditor,
			wantRoles: map[workspace.ID]Role{
				"prod":    RoleViewer,
				"staging": RoleEditor,
			},
		},
		{
			name:     "across workspaces equal role tie-breaks by declared mapping order",
			groups:   []string{"staging-admins", "prod-admins"},
			wantWS:   "prod",
			wantRole: RoleAdmin,
			wantRoles: map[workspace.ID]Role{
				"prod":    RoleAdmin,
				"staging": RoleAdmin,
			},
		},
		{
			name:     "unmapped groups alongside a mapped group are ignored",
			groups:   []string{"external-contractors", "audit-viewers", "random-group"},
			wantWS:   "audit",
			wantRole: RoleViewer,
			wantRoles: map[workspace.ID]Role{
				"audit": RoleViewer,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idp := oidctest.New(t)
			p := newTestProvider(t, idp, func(cfg *OIDCConfig) {
				cfg.GroupMappings = mappings
			})

			// 1. Browser callback flow (CompleteLogin + LoginOIDC)
			q, state, code := oidcLogin(t, p, idp, "/monitors")
			respond(t, idp, code, func(c map[string]any) {
				c["groups"] = tc.groups
			})
			id, next, err := complete(t, p, q, state)
			if err != nil {
				t.Fatalf("CompleteLogin: %v", err)
			}
			if next != "/monitors" {
				t.Errorf("next = %q, want /monitors", next)
			}
			if id.Workspace != tc.wantWS || id.Role != tc.wantRole {
				t.Errorf("Identity = (%q, %s), want (%q, %s)", id.Workspace, id.Role, tc.wantWS, tc.wantRole)
			}
			for ws, wantR := range tc.wantRoles {
				if gotR, ok := id.RoleInWorkspace(ws); !ok || gotR != wantR {
					t.Errorf("Identity.RoleInWorkspace(%q) = (%s, %v), want (%s, true)", ws, gotR, ok, wantR)
				}
			}

			// 2. Bearer ID token flow (VerifyBearer + AuthenticateRequest)
			claims := idp.ClaimsFor(code)
			delete(claims, "nonce")
			claims["sub"] = "bearer-user"
			claims["groups"] = tc.groups
			rawToken := idp.IDToken(claims)

			authn := New(nil, 0).WithOIDC(p)
			req := httptest.NewRequest(http.MethodGet, "/api/v1/monitors", nil)
			req.Header.Set("Authorization", "Bearer "+rawToken)

			principal, status := authn.AuthenticateRequest(req)
			if status != AuthOK || principal == nil {
				t.Fatalf("AuthenticateRequest status = %v, want AuthOK", status)
			}
			if principal.Workspace != tc.wantWS || principal.Role != tc.wantRole {
				t.Errorf("Principal = (%q, %s), want (%q, %s)", principal.Workspace, principal.Role, tc.wantWS, tc.wantRole)
			}
			for ws, wantR := range tc.wantRoles {
				if gotR, ok := principal.RoleInWorkspace(ws); !ok || gotR != wantR {
					t.Errorf("Principal.RoleInWorkspace(%q) = (%s, %v), want (%s, true)", ws, gotR, ok, wantR)
				}
			}
		})
	}
}

func TestOIDCGroupMappingFailClosedOnUnmappedClaims(t *testing.T) {
	mappings := []GroupMapping{
		{Group: "sre-prod", Workspace: "prod", Role: RoleAdmin},
	}

	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{
			name:   "missing groups claim",
			mutate: func(c map[string]any) { delete(c, "groups") },
		},
		{
			name:   "empty groups slice",
			mutate: func(c map[string]any) { c["groups"] = []string{} },
		},
		{
			name:   "blank group string",
			mutate: func(c map[string]any) { c["groups"] = "   " },
		},
		{
			name:   "non-string claim type",
			mutate: func(c map[string]any) { c["groups"] = 12345 },
		},
		{
			name:   "unmapped groups only",
			mutate: func(c map[string]any) { c["groups"] = []string{"secret-unmapped-group-alpha", "other-team"} },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idp := oidctest.New(t)
			p := newTestProvider(t, idp, func(cfg *OIDCConfig) {
				cfg.Workspace = "default"
				cfg.GroupMappings = mappings
			})
			authn := New(nil, 0).WithOIDC(p)

			// 1. Browser callback must fail closed with ErrOIDCAccount and never echo claims
			q, state, code := oidcLogin(t, p, idp, "/")
			respond(t, idp, code, tc.mutate)
			id, _, err := complete(t, p, q, state)
			if err == nil {
				t.Fatalf("CompleteLogin succeeded with %+v; want fail-closed denial", id)
			}
			if !errors.Is(err, ErrOIDCAccount) {
				t.Errorf("CompleteLogin err = %v, want ErrOIDCAccount", err)
			}
			if strings.Contains(err.Error(), "secret-unmapped-group-alpha") {
				t.Errorf("error echoed raw group claim value: %q", err.Error())
			}

			// 2. Bearer ID token must fail closed with AuthForbidden (403)
			claims := idp.ClaimsFor(code)
			delete(claims, "nonce")
			tc.mutate(claims)
			rawToken := idp.IDToken(claims)

			req := httptest.NewRequest(http.MethodGet, "/api/v1/monitors", nil)
			req.Header.Set("Authorization", "Bearer "+rawToken)
			principal, status := authn.AuthenticateRequest(req)
			if status != AuthForbidden || principal != nil {
				t.Errorf("AuthenticateRequest = (%+v, %v), want (nil, AuthForbidden)", principal, status)
			}

			// 3. LoginOIDC with an unmapped Identity must refuse to mint a session
			if sid := authn.LoginOIDC(&Identity{Subject: "u-1"}); sid != "" {
				t.Errorf("LoginOIDC minted session %q for unmapped identity", sid)
			}
		})
	}
}

func TestOIDCGroupMappingReloadAndBoundedTTLRevocation(t *testing.T) {
	idp := oidctest.New(t)
	now := time.Unix(1_800_000_000, 0).UTC()

	var mu sync.Mutex
	providerGroups := map[string][]string{
		"user-123": {"prod-editors"},
	}

	p := newTestProvider(t, idp, func(cfg *OIDCConfig) {
		cfg.GroupClaim = "groups"
		cfg.GroupCacheTTL = 2 * time.Minute
		cfg.GroupMappings = []GroupMapping{
			{Group: "prod-editors", Workspace: "prod", Role: RoleEditor},
			{Group: "prod-viewers", Workspace: "prod", Role: RoleViewer},
		}
		cfg.GroupResolver = func(_ context.Context, subject string) ([]string, error) {
			mu.Lock()
			defer mu.Unlock()
			out := append([]string(nil), providerGroups[subject]...)
			return out, nil
		}
	})
	p.withNow(func() time.Time { return now })

	authn := New(nil, time.Hour).WithOIDC(p)
	authn.now = func() time.Time { return now }

	// Step 1: Sign in and also prepare a bearer token for user-123
	q, state, code := oidcLogin(t, p, idp, "/")
	respond(t, idp, code, func(c map[string]any) {
		c["groups"] = []string{"prod-editors"}
	})
	id, _, err := complete(t, p, q, state)
	if err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}
	sid := authn.LoginOIDC(id)
	if sid == "" {
		t.Fatal("LoginOIDC returned empty session id")
	}

	claims := idp.ClaimsFor(code)
	delete(claims, "nonce")
	claims["exp"] = now.Add(time.Hour).Unix()
	claims["groups"] = []string{"prod-editors"}
	rawBearer := idp.IDToken(claims)

	sessionReq := func() (*Principal, AuthStatus) {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/monitors", nil)
		r.AddCookie(&http.Cookie{Name: SessionCookie, Value: sid})
		return authn.AuthenticateRequest(r)
	}
	bearerReq := func() (*Principal, AuthStatus) {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/monitors", nil)
		r.Header.Set("Authorization", "Bearer "+rawBearer)
		return authn.AuthenticateRequest(r)
	}

	if pr, st := sessionReq(); st != AuthOK || pr.Role != RoleEditor || pr.Workspace != "prod" {
		t.Fatalf("initial session = (%+v, %v), want prod:editor", pr, st)
	}
	if pr, st := bearerReq(); st != AuthOK || pr.Role != RoleEditor || pr.Workspace != "prod" {
		t.Fatalf("initial bearer = (%+v, %v), want prod:editor", pr, st)
	}

	// Step 2: Live mapping reload without restart downgrades prod-editors to viewer
	if err := p.SetGroupMappings([]GroupMapping{
		{Group: "prod-editors", Workspace: "prod", Role: RoleViewer},
	}); err != nil {
		t.Fatalf("SetGroupMappings: %v", err)
	}
	if pr, st := sessionReq(); st != AuthOK || pr.Role != RoleViewer {
		t.Fatalf("after mapping reload, session = (%+v, %v), want RoleViewer", pr, st)
	}
	if pr, st := bearerReq(); st != AuthOK || pr.Role != RoleViewer {
		t.Fatalf("after mapping reload, bearer = (%+v, %v), want RoleViewer", pr, st)
	}

	// Step 3: Remove user-123 from the group at the provider.
	// Within GroupCacheTTL (after 1 minute < 2m TTL), cached resolution still holds.
	mu.Lock()
	providerGroups["user-123"] = nil
	mu.Unlock()

	now = now.Add(time.Minute)
	if _, st := sessionReq(); st != AuthOK {
		t.Fatalf("before cache TTL expiry, session status = %v, want AuthOK", st)
	}
	if _, st := bearerReq(); st != AuthOK {
		t.Fatalf("before cache TTL expiry, bearer status = %v, want AuthOK", st)
	}

	// Once GroupCacheTTL (2m) elapses, access is revoked on both session and bearer paths!
	now = now.Add(time.Minute + time.Second)
	if pr, st := sessionReq(); st != AuthForbidden || pr != nil {
		t.Fatalf("after cache TTL expiry, session = (%+v, %v), want (nil, AuthForbidden)", pr, st)
	}
	if pr, st := bearerReq(); st != AuthForbidden || pr != nil {
		t.Fatalf("after cache TTL expiry, bearer = (%+v, %v), want (nil, AuthForbidden)", pr, st)
	}
}
