package api

import (
	"log/slog"
	"net/http"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// AuthMiddleware requires a credential on every /api/v1 route once a token
// is configured through API_TOKEN, and scopes the request to the workspace
// that credential belongs to. Two credentials are accepted:
//
//   - Authorization: Bearer <token> — scripts, CI and other services.
//   - the dashboard's session cookie — a browser that signed in at /login.
//     The dashboard is same-origin with the API and links straight to
//     endpoints like /api/v1/alerts.csv, and a plain link cannot attach a
//     header, so the cookie has to count here too. It is HttpOnly and
//     SameSite=Lax, so a cross-site page can neither read it nor get a
//     forged POST to carry it.
//
// The workspace comes from the credential and from nothing else (auth.
// Principal): there is deliberately no X-Workspace header, because a client
// that could name its own workspace could read another team's monitors.
// Handlers below this middleware therefore need no per-route tenancy check —
// every store method they call is scoped by the context they hand it.
//
// A credential that carries a scope list (a token minted at POST /tokens) is
// additionally checked against routeScopes before any handler runs: the route
// it needs must be in the table, and the token must hold every scope that entry
// lists. Anything else is a 403. A static token or a dashboard session has no
// scope list and is exempt, because that is what it means to be the operator.
//
// With no token configured the middleware steps aside completely: an
// unauthenticated deployment behaves exactly as it did before this existed
// (the process logs one startup warning instead), and a request context with
// no workspace resolves to the default one in the store.
//
// Probes (/health, /livez, /readyz) stay exempt so an authenticated
// deployment cannot fail its own health checks, and so the docker-compose
// healthcheck keeps working with no token in its environment. This is the
// same exemption the rate limiter uses (isProbePath); the cost is that the
// readiness detail — dependency names and error strings — is readable
// without a token. Do not expose these paths to the public internet.
func AuthMiddleware(a *auth.Authenticator, log *slog.Logger) func(http.Handler) http.Handler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
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
			principal, ok := a.Principal(r)
			if !ok {
				// One message for a missing header, a malformed one and a
				// wrong token: the response must not tell a caller which of
				// those it got. The presented credential is never echoed.
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
