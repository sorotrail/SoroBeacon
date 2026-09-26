package api

import (
	"net/http"

	"github.com/sorotrail/sorobeacon/internal/auth"
)

// AuthMiddleware requires a credential on every /api/v1 route once a token
// is configured through API_TOKEN.
func AuthMiddleware(a *auth.Authenticator) func(http.Handler) http.Handler {
	if !a.Enabled() {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isProbePath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			if !a.Authenticated(r) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="sorobeacon"`)
				writeErr(w, r, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, r)
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
