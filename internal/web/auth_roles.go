package web

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/sorotrail/sorobeacon/internal/auth"
)

// routePath is the path to match against the role table: what the mounted
// router still has to route. Chi sets it while routing the request, and the
// URL is the fallback for a router mounted at the root, where the two are the
// same string anyway.
func routePath(r *http.Request) string {
	if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePath != "" {
		return rc.RoutePath
	}
	return r.URL.Path
}

// denyRole answers a request whose session is not allowed to reach the page
// it asked for.
//
// The page says what is needed and nothing about the credentials that exist:
// the required role is not a secret, and a signed-in operator whose role is
// too narrow needs to know which one to ask for.
func (s *Server) denyRole(w http.ResponseWriter, r *http.Request, required auth.Role) {
	s.log.Warn("role denied",
		"method", r.Method,
		"path", routePath(r),
		"required_role", required.String())
	s.renderStatus(w, r, http.StatusForbidden, "error", map[string]any{
		"Title":   "Not allowed",
		"Heading": "Not allowed",
		"Message": "Your role does not allow this page. It needs the " + required.String() + " role.",
	})
}
