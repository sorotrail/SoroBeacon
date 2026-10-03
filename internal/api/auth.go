package api

import (
	"log/slog"
	"net/http"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// AuthMiddleware requires a credential on every /api/v1 route once a token
// is configured through API_TOKEN.
func AuthMiddleware(a *auth.Authenticator, log *slog.Logger) func(http.Handler) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	if !a.Enabled() {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isProbePath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			// One resolution serves both questions: whether the request is
			// authenticated at all, and which tenant and scopes it carries.
			// Asking twice would let the two answers disagree.
			principal, ok := a.Principal(r)
			if !ok {
				w.Header().Set("WWW-Authenticate", `Bearer realm="sorobeacon"`)
				writeErr(w, r, http.StatusUnauthorized, "unauthorized")
				return
			}
			// Fail closed. A scoped credential is checked against the route
			// table; a static token or a dashboard session carries no scope
			// list and is the operator's own reach, so it passes through as it
			// always did.
			if !principal.Unrestricted() {
				scopes, mapped := requiredScopes(r.Method, routePath(r))
				if !mapped {
					// A route nobody mapped is a route no scoped token has ever
					// been reviewed for, and the alternative — allowing what is
					// unknown — is how an added endpoint becomes an unlisted
					// permission.
					log.Warn("route has no scope mapping", "method", r.Method, "path", routePath(r))
					writeErr(w, r, http.StatusForbidden, "this route accepts no scoped token")
					return
				}
				for _, need := range scopes {
					if !principal.Allowed(need) {
						// The missing scope is named: it is not a secret, and a
						// caller holding a token with the wrong grant needs to
						// know which one to ask for.
						writeErr(w, r, http.StatusForbidden, "token lacks scope "+string(need))
						return
					}
				}
			}
			ctx := auth.WithPrincipal(workspace.With(r.Context(), principal.Workspace), principal)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RoleMiddleware returns middleware that enforces role-based access control based on endpoint methods/paths.
func RoleMiddleware(a *auth.Authenticator) func(http.Handler) http.Handler {
	if !a.Enabled() {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isProbePath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			required := auth.RoleViewer
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
				required = auth.RoleEditor
			}
			role, _ := a.RoleForRequest(r)
			if !role.HasPermission(required) {
				writeErr(w, r, http.StatusForbidden, "forbidden")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireRole returns middleware that enforces minimum role permissions (fail-closed).
func RequireRole(required auth.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authObj, ok := r.Context().Value(authContextKey).(*auth.Authenticator)
			// If auth is not in context, fall back to checking if global auth permits or fail-closed if unauthenticated
			var role auth.Role
			if ok && authObj != nil {
				role, _ = authObj.RoleForRequest(r)
			} else {
				role = auth.RoleUnknown
			}
			if !role.HasPermission(required) {
				writeErr(w, r, http.StatusForbidden, "forbidden")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

var authContextKey = &contextKey{"auth"}

type contextKey struct {
	name string
}
