package web

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

// walkedDashboardRoutes returns every "METHOD /pattern" pair the dashboard
// router registers, in the same key form routeRoles is written in.
func walkedDashboardRoutes(t *testing.T) []dashboardRoute {
	t.Helper()
	s := newTestServer(t)
	var out []dashboardRoute
	walk := func(method, path string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if path == "/*" {
			return nil
		}
		out = append(out, dashboardRoute{Method: method, Path: auth.NormalizeRoute(path)})
		return nil
	}
	if err := chi.Walk(s.Routes(), walk); err != nil {
		t.Fatalf("walk routes: %v", err)
	}
	return out
}

// dashboardRoute is one route the dashboard router serves.
type dashboardRoute struct {
	Method string
	Path   string
}

// The role table is the review record: every page the dashboard serves has
// to have been looked at and given a role.
func TestRoleTableCoversEveryDashboardRoute(t *testing.T) {
	routes := walkedDashboardRoutes(t)
	if len(routes) < 20 {
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
func TestRoleTableEnforcesEveryDashboardRouteForEveryRole(t *testing.T) {
	routes := walkedDashboardRoutes(t)
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
				req := httptest.NewRequest(rt.Method, concreteDashboardPath(rt.Path), nil)
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

// The sign-in page has to be reachable without a session: a visitor who is
// not signed in has to be able to sign in.
func TestSignInPagesStayOpen(t *testing.T) {
	s := newTestServer(t)
	s.WithAuth(auth.New([]string{roleToken("viewer") + ":viewer"}, 0))
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	for _, path := range []string{"/login", "/favicon.ico"} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode == http.StatusForbidden {
			t.Errorf("GET %s = 403, want the sign-in flow to stay open", path)
		}
	}
}

// concreteDashboardPath turns a route pattern into a path a request can use:
// every parameter becomes a number.
func concreteDashboardPath(pattern string) string {
	segs := strings.Split(strings.Trim(pattern, "/"), "/")
	for i, s := range segs {
		if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
			segs[i] = "1"
		}
	}
	return "/" + strings.Join(segs, "/")
}
