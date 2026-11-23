package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRole(t *testing.T) {
	tests := []struct {
		input    string
		expected Role
		ok       bool
	}{
		{"viewer", RoleViewer, true},
		{"VIEWER", RoleViewer, true},
		{"  viewer  ", RoleViewer, true},
		{"editor", RoleEditor, true},
		{"ADMIN", RoleAdmin, true},
		{"unknown", RoleUnknown, false},
		{"", RoleUnknown, false},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			role, ok := ParseRole(tc.input)
			assert.Equal(t, tc.expected, role)
			assert.Equal(t, tc.ok, ok)
		})
	}
}

func TestRoleHierarchy(t *testing.T) {
	assert.True(t, RoleViewer.HasPermission(RoleViewer))
	assert.False(t, RoleViewer.HasPermission(RoleEditor))
	assert.False(t, RoleViewer.HasPermission(RoleAdmin))

	assert.True(t, RoleEditor.HasPermission(RoleViewer))
	assert.True(t, RoleEditor.HasPermission(RoleEditor))
	assert.False(t, RoleEditor.HasPermission(RoleAdmin))

	assert.True(t, RoleAdmin.HasPermission(RoleViewer))
	assert.True(t, RoleAdmin.HasPermission(RoleEditor))
	assert.True(t, RoleAdmin.HasPermission(RoleAdmin))

	assert.False(t, RoleUnknown.HasPermission(RoleViewer))

	// A route that asks for no credential is satisfied by anyone, including
	// a caller with no role at all.
	assert.True(t, RoleUnknown.HasPermission(RoleOpen))
	assert.True(t, RoleViewer.HasPermission(RoleOpen))
}

// The role table is the review record: a route with no entry is denied, and
// a route that asks for no credential is reachable by anyone.
func TestRoleMiddlewareEnforcesTheTable(t *testing.T) {
	a := New([]string{"token-admin:admin", "token-editor:editor", "token-viewer:viewer"}, 0)
	table := NewRouteTable(map[string]Role{
		"GET /safe":     RoleViewer,
		"POST /changes": RoleEditor,
		"POST /admin":   RoleAdmin,
		"GET /open":     RoleOpen,
	})

	handler := RoleCheck{
		Authenticator: a,
		Table:         table,
		RoutePath:     func(r *http.Request) string { return r.URL.Path },
		RouteExists: func(method, path string) bool {
			return path == "/safe" || path == "/changes" || path == "/admin" || path == "/open"
		},
	}.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	call := func(token, method, path string) int {
		req := httptest.NewRequest(method, path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	assert.Equal(t, http.StatusOK, call("token-viewer", "GET", "/safe"),
		"a viewer may read")
	assert.Equal(t, http.StatusForbidden, call("token-viewer", "POST", "/changes"),
		"a viewer may not change")
	assert.Equal(t, http.StatusOK, call("token-editor", "POST", "/changes"),
		"an editor may change")
	assert.Equal(t, http.StatusForbidden, call("token-editor", "POST", "/admin"),
		"an editor is not an administrator")
	assert.Equal(t, http.StatusOK, call("token-admin", "POST", "/admin"),
		"an administrator may do the administrative things")
	assert.Equal(t, http.StatusOK, call("", "GET", "/open"),
		"a route that asks for no credential is reachable without one")
}

// A route the router does not have is left to the router: it answers 404 (or
// 405 for a method the route does not take). A route the router has but the
// table does not is denied, because nobody decided what role it takes.
func TestRoleMiddlewareLeavesUnknownPathsToTheRouter(t *testing.T) {
	a := New([]string{"token-admin:admin"}, 0)
	table := NewRouteTable(map[string]Role{"GET /known": RoleViewer})

	handler := RoleCheck{
		Authenticator: a,
		Table:         table,
		RoutePath:     func(r *http.Request) string { return r.URL.Path },
		RouteExists:   func(method, path string) bool { return path == "/known" },
	}.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/no-such-route", nil)
	req.Header.Set("Authorization", "Bearer token-admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code,
		"a path the router does not have is the router's answer, not a denial")
}

// An instance with no credential source is open, and an open instance is an
// admin one: the role check must not close it.
func TestRoleMiddlewareWithAuthenticationDisabledIsAdmin(t *testing.T) {
	table := NewRouteTable(map[string]Role{"POST /changes": RoleAdmin})
	handler := RoleCheck{
		Authenticator: New(nil, 0),
		Table:         table,
		RoutePath:     func(r *http.Request) string { return r.URL.Path },
	}.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("POST", "/changes", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// The role a scoped token carries is the coarsest one its scopes allow. It
// can never be wider than the grant: a read-only token is a viewer.
func TestRoleForScopes(t *testing.T) {
	tests := []struct {
		name   string
		scopes []Scope
		want   Role
	}{
		{"no scopes is a viewer", nil, RoleViewer},
		{"read scopes are a viewer", []Scope{ScopeMonitorsRead, ScopeAlertsRead}, RoleViewer},
		{"a write scope is an editor", []Scope{ScopeMonitorsRead, ScopeMonitorsWrite}, RoleEditor},
		{"the credential scope is administrative", []Scope{ScopeTokensWrite}, RoleAdmin},
		{"the wildcard is administrative", []Scope{ScopeAll}, RoleAdmin},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, RoleForScopes(tt.scopes))
		})
	}
}

// The route table resolves a path the way the routers register it: an exact
// match first, then the most specific pattern.
func TestRouteTableLookup(t *testing.T) {
	table := NewRouteTable(map[string]Role{
		"GET /monitors":        RoleViewer,
		"GET /monitors/trash":  RoleEditor,
		"GET /monitors/{id}":   RoleViewer,
		"POST /monitors/{id}":  RoleEditor,
		"GET /monitors/{id}/x": RoleAdmin,
	})

	got, ok := table.Lookup("GET", "/monitors")
	require.True(t, ok)
	assert.Equal(t, RoleViewer, got)

	got, ok = table.Lookup("GET", "/monitors/trash")
	require.True(t, ok)
	assert.Equal(t, RoleEditor, got, "a literal route beats a parameterised one")

	got, ok = table.Lookup("GET", "/monitors/7")
	require.True(t, ok)
	assert.Equal(t, RoleViewer, got)

	got, ok = table.Lookup("POST", "/monitors/7")
	require.True(t, ok)
	assert.Equal(t, RoleEditor, got)

	// A parameterised pattern does not reach a deeper path.
	_, ok = table.Lookup("GET", "/monitors/7/rules")
	assert.False(t, ok)

	_, ok = table.Lookup("PUT", "/monitors")
	assert.False(t, ok, "a method with no entry is not in the table")

	// A trailing slash is the same route.
	got, ok = table.Lookup("GET", "/monitors/")
	require.True(t, ok)
	assert.Equal(t, RoleViewer, got)
}
