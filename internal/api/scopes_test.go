package api

import (
	"net/http"
	"slices"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
)

// walkedRoutes returns every "METHOD /pattern" pair the API router registers, in
// the same key form routeScopes is written in.
func walkedRoutes(t *testing.T) []route {
	t.Helper()
	s := New(&fakeStore{}, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger())
	var out []route
	walk := func(method, path string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if path == "/*" {
			return nil
		}
		out = append(out, route{Method: method, Path: normalizeRoute(path)})
		return nil
	}
	if err := chi.Walk(s.Routes(), walk); err != nil {
		t.Fatalf("walk routes: %v", err)
	}
	return out
}

// TestScopeTableCoversEveryRoute holds both halves of the
// scope contract in one place.
//
// Forward: a registered route that is missing from the table denies every scoped
// token, because the check fails closed — so the omission has to be a decision,
// and the only routes allowed to be missing are the probes that authenticate
// nobody. Backward: an entry whose route has been renamed or deleted is a
// permission the table advertises for a path that no longer exists, which reads
// as coverage and is drift.
//
// This is what turns "added an endpoint, forgot the scope table" into a build
// failure rather than a silent hole.
func TestScopeTableCoversEveryRoute(t *testing.T) {
	routes := walkedRoutes(t)
	if len(routes) < 25 {
		t.Fatalf("the walk found %d routes; the router is not registering what it used to", len(routes))
	}

	registered := map[string]bool{}
	for _, rt := range routes {
		key := rt.Method + " " + rt.Path
		registered[key] = true

		if isProbePath(rt.Path) {
			if _, mapped := requiredScopes(rt.Method, rt.Path); mapped {
				t.Errorf("probe %s is in routeScopes but is meant to be exempt from authentication", key)
			}
			continue
		}
		if _, mapped := requiredScopes(rt.Method, rt.Path); !mapped {
			t.Errorf("route %s has no entry in routeScopes, so every scoped token is denied it", key)
		}
	}

	for key := range routeScopes {
		if !registered[key] {
			t.Errorf("routeScopes names %q but no such route is registered", key)
		}
	}
}

// TestRouteScopesOnlyNameMintableScopes keeps the table and the mint vocabulary
// from drifting apart the third way: a route requiring a scope nobody can hold is
// unreachable by design, and that reads as a grant rather than a wall.
func TestRouteScopesOnlyNameMintableScopes(t *testing.T) {
	mintable := map[auth.Scope]bool{}
	for _, s := range auth.AllScopes {
		mintable[s] = true
	}
	for key, scopes := range routeScopes {
		if len(scopes) == 0 {
			t.Errorf("%q maps to no scopes; use auth.ScopeNone to say \"any credential, no permission\"", key)
		}
		for _, s := range scopes {
			if s != auth.ScopeNone && !mintable[s] {
				t.Errorf("route %q requires %q, which cannot be minted", key, s)
			}
		}
	}
}

// TestRequiredScopesMatchesPaths covers the matching itself: a pattern has to
// answer for the concrete request paths it names, and only for those.
func TestRequiredScopesMatchesPaths(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		path   string
		want   []auth.Scope
		ok     bool
	}{
		{"literal", http.MethodGet, "/stats", []auth.Scope{auth.ScopeStatsRead}, true},
		{"one param", http.MethodGet, "/monitors/7", []auth.Scope{auth.ScopeMonitorsRead}, true},
		{"two params", http.MethodDelete, "/monitors/7/rules/9", []auth.Scope{auth.ScopeMonitorsWrite}, true},
		{"a route with two requirements", http.MethodPost, "/templates/3/instantiate",
			[]auth.Scope{auth.ScopeTemplatesWrite, auth.ScopeMonitorsWrite}, true},
		{"trailing slash is the same route", http.MethodGet, "/monitors/", []auth.Scope{auth.ScopeMonitorsRead}, true},
		{"any valid credential", http.MethodGet, "/version", []auth.Scope{auth.ScopeNone}, true},
		{"unmapped path", http.MethodGet, "/never-heard-of-it", nil, false},
		{"unmapped method", http.MethodPut, "/monitors", nil, false},
		{"probes are not mapped", http.MethodGet, "/readyz", nil, false},
	} {
		got, ok := requiredScopes(tc.method, tc.path)
		if ok != tc.ok {
			t.Errorf("%s: ok = %v, want %v", tc.name, ok, tc.ok)
			continue
		}
		if tc.ok && !slices.Equal(got, tc.want) {
			t.Errorf("%s: scopes = %v, want %v", tc.name, got, tc.want)
		}
	}

	// "/monitors/{id}" must not select a deeper path: the sub-resource is another
	// route, and here it is another write.
	if got, ok := requiredScopes(http.MethodPost, "/monitors/7/rules"); !ok ||
		!slices.Equal(got, []auth.Scope{auth.ScopeMonitorsWrite}) {
		t.Errorf("POST /monitors/7/rules = %v, %v, want monitors:write", got, ok)
	}
	if got, ok := requiredScopes(http.MethodDelete, "/monitors/7"); !ok ||
		!slices.Equal(got, []auth.Scope{auth.ScopeMonitorsWrite}) {
		t.Errorf("DELETE /monitors/7 = %v, %v, want monitors:write", got, ok)
	}
}
