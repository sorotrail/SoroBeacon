package api

import (
	"log/slog"
	"net/http"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/reqid"
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
			principal, status := a.AuthenticateRequest(r)
			if status == auth.AuthForbidden {
				log.Warn("oidc claim mapping denied",
					"request_id", reqid.From(r),
					"method", r.Method,
					"path", routePath(r))
				writeErr(w, r, http.StatusForbidden, "forbidden")
				return
			}
			if status != auth.AuthOK || principal == nil {
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
			// The role is resolved from the credential that was just resolved,
			// so the role check downstream does not read the token row again.
			ctx = auth.WithRole(ctx, a.RoleForPrincipal(r, principal))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// denyRole answers a request whose credential is not allowed to call the
// route it asked for.
//
// The message says what the caller needs and nothing about the credentials
// that exist: the required role is not a secret, and a caller holding the
// wrong one needs to know which one to ask for.
func (s *Server) denyRole(w http.ResponseWriter, r *http.Request, required auth.Role) {
	s.log.Warn("role denied",
		"request_id", reqid.From(r),
		"method", r.Method,
		"path", routePath(r),
		"required_role", required.String())
	writeErr(w, r, http.StatusForbidden, "forbidden")
}
