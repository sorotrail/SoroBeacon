package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/sorotrail/sorobeacon/internal/auth"
)

// roleToken is a static token carrying a role. The suffix names the role and
// is stripped when the token table is built, so the client presents the bare
// token — which is what the role tests use.
func roleToken(role string) string { return "role-token-" + role }

// The role table is the review record: every route the router serves has to
// have been looked at and given a role, and an entry whose route has gone
// away is a permission the table advertises for a path that no longer exists.
//
// This is what turns "added an endpoint, forgot the role table" into a build
// failure rather than a route nobody decided should be reachable.
func TestRoleTableCoversEveryRoute(t *testing.T) {
	routes := walkedRoutes(t)
	if len(routes) < 25 {
		t.Fatalf("the walk found %d routes; the router is not registering what it used to", len(routes))
	}

	registered := map[string]bool{}
	for _, rt := range routes {
		key := rt.Method + " " + rt.Path
		registered[key] = true
		if _, mapped := routeRoles.Lookup(rt.Method, rt.Path); !mapped {
			t.Errorf("route %s has no entry in routeRoles, so every credential is denied it", key)
		}
	}

	for _, key := range routeRoles.Patterns() {
		if !registered[key] {
			t.Errorf("routeRoles names %q but no such route is registered", key)
		}
	}
}

// Every route, every role: a credential is refused exactly the routes its
// role does not reach, and reaches the rest.
//
// The routes are served by a stand-in handler rather than the real ones, so
// the assertion is about the role decision alone and not about what a
// handler with a fake store behind it happens to answer.
func TestRoleTableEnforcesEveryRouteForEveryRole(t *testing.T) {
	routes := walkedRoutes(t)
	roles := []struct {
		name string
		role auth.Role
	}{
		{"viewer", auth.RoleViewer},
		{"editor", auth.RoleEditor},
		{"admin", auth.RoleAdmin},
	}

	for _, r := range roles {
		t.Run(r.name, func(t *testing.T) {
			mux := chi.NewRouter()
			mux.Use(auth.RoleCheck{
				Authenticator: auth.New([]string{roleToken(r.name) + ":" + r.name}, 0),
				Table:         routeRoles,
				RoutePath:     func(req *http.Request) string { return req.URL.Path },
				RouteExists: func(method, path string) bool {
					return mux.Match(chi.NewRouteContext(), method, path)
				},
			}.Middleware())
			for _, rt := range routes {
				method, pattern := rt.Method, rt.Path
				mux.MethodFunc(method, pattern, func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusOK)
				})
			}

			for _, rt := range routes {
				required, ok := routeRoles.Lookup(rt.Method, rt.Path)
				if !ok {
					continue // the coverage test above reports the omission
				}
				req := httptest.NewRequest(rt.Method, concretePath(rt.Path), nil)
				req.Header.Set("Authorization", "Bearer "+roleToken(r.name))
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, req)

				denied := rec.Code == http.StatusForbidden
				if r.role.AtLeast(required) && denied {
					t.Errorf("%s %s = 403, want it allowed for a %s", rt.Method, rt.Path, r.name)
				}
				if !r.role.AtLeast(required) && !denied {
					t.Errorf("%s %s = %d, want 403 for a %s", rt.Method, rt.Path, rec.Code, r.name)
				}
			}
		})
	}
}

// The end-to-end shape: the router itself refuses a write from a viewer and
// lets an administrator through. The stand-in handler above proves the
// table; this proves the wiring.
func TestRoleCheckIsMountedOnTheRouter(t *testing.T) {
	viewer := authServerOn(&fakeStore{}, auth.New([]string{roleToken("viewer") + ":viewer"}, 0))
	admin := authServerOn(&fakeStore{}, auth.New([]string{roleToken("admin") + ":admin"}, 0))

	post := func(h http.Handler, token string) int {
		req := httptest.NewRequest(http.MethodPost, "/monitors", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := post(viewer, roleToken("viewer")); code != http.StatusForbidden {
		t.Errorf("a viewer POSTing a monitor = %d, want 403", code)
	}
	if code := post(admin, roleToken("admin")); code == http.StatusForbidden {
		t.Errorf("an admin POSTing a monitor = %d, want it allowed", code)
	}
}

// A path the router does not have is the router's answer, not a denial: a
// typo must not look like a permission problem.
func TestRoleCheckLeavesUnknownPathsToTheRouter(t *testing.T) {
	h := authServerOn(&fakeStore{}, auth.New([]string{roleToken("admin") + ":admin"}, 0))
	req := httptest.NewRequest(http.MethodGet, "/no-such-route", nil)
	req.Header.Set("Authorization", "Bearer "+roleToken("admin"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("an unknown path = %d, want 404", rec.Code)
	}
}

// The probes answer for the instance's health, not for its data, and an
// orchestrator has no credential to present.
func TestProbesStayOpen(t *testing.T) {
	for _, path := range []string{"/health", "/livez", "/readyz"} {
		h := authServerOn(&fakeStore{}, auth.New([]string{roleToken("viewer") + ":viewer"}, 0))
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusForbidden {
			t.Errorf("GET %s = 403, want the probe to stay open", path)
		}
	}
}

// concretePath turns a route pattern into a path a request can use: every
// parameter becomes a number.
func concretePath(pattern string) string {
	segs := strings.Split(strings.Trim(pattern, "/"), "/")
	for i, s := range segs {
		if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
			segs[i] = "1"
		}
	}
	return "/" + strings.Join(segs, "/")
}
