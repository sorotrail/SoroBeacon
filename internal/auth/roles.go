package auth

import (
	"context"
	"net/http"
	"strings"
)

// Role is what a credential is allowed to do. The three roles are a small
// closed set, ordered, so "at least editor" is a comparison rather than a
// list of cases.
type Role int

const (
	// RoleUnknown is the role of a caller nobody could identify: no
	// credential, or one that did not resolve. It holds no permission.
	RoleUnknown Role = iota
	// RoleViewer may read.
	RoleViewer
	// RoleEditor may read and change the things an instance is for: monitors,
	// rules, channels, maintenance windows, alerts.
	RoleEditor
	// RoleAdmin may do everything an editor may, plus the administrative
	// things: minting and revoking credentials, and reading the audit log.
	RoleAdmin
)

// RoleOpen is what a route asks of a caller that presents no credential at
// all: the dashboard's sign-in page, the API's health probes, the favicon.
//
// It is a distinct value rather than the absence of an entry because the
// absence of an entry means deny: a route nobody reviewed must not be open,
// and a route that is deliberately open has to say so.
const RoleOpen Role = -1

// ParseRole reads a role name. It is what turns a static token's suffix and
// an OIDC claim value into a role.
func ParseRole(s string) (Role, bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	switch s {
	case "viewer":
		return RoleViewer, true
	case "editor":
		return RoleEditor, true
	case "admin":
		return RoleAdmin, true
	default:
		return RoleUnknown, false
	}
}

// String names the role, for log lines and for the dashboard.
func (r Role) String() string {
	switch r {
	case RoleViewer:
		return "viewer"
	case RoleEditor:
		return "editor"
	case RoleAdmin:
		return "admin"
	case RoleOpen:
		return "open"
	default:
		return "unknown"
	}
}

// rank orders the roles. RoleOpen sorts below RoleUnknown, so a route that
// asks for no credential is satisfied by any caller, including one with no
// credential at all.
func (r Role) rank() int {
	switch r {
	case RoleOpen:
		return -1
	case RoleViewer:
		return 1
	case RoleEditor:
		return 2
	case RoleAdmin:
		return 3
	default:
		return 0
	}
}

// AtLeast reports whether the role is at least the required one.
func (r Role) AtLeast(required Role) bool {
	return r.rank() >= required.rank()
}

// HasPermission is AtLeast under the name the middleware uses.
func (r Role) HasPermission(required Role) bool {
	return r.AtLeast(required)
}

// RoleForScopes is the coarsest role a scoped token's grant allows.
//
// A scoped token is a machine credential: its scopes are the grant, and the
// role is a second gate over the same grant rather than a replacement for it.
// A read-only grant is therefore a viewer, a write grant is an editor, and the
// credential grant — minting and revoking tokens — is administrative, because
// minting a token is minting authority.
//
// The role can never be wider than the scopes: a token holding only
// `alerts:read` is a viewer, whatever else it might ask for.
func RoleForScopes(scopes []Scope) Role {
	role := RoleViewer
	for _, s := range scopes {
		switch s {
		case ScopeAll:
			return RoleAdmin
		case ScopeTokensWrite:
			// Minting credentials is administrative.
			return RoleAdmin
		case ScopeMonitorsWrite, ScopeChannelsWrite, ScopeAlertsWrite, ScopeTemplatesWrite:
			role = RoleEditor
		}
	}
	return role
}

// RoleContextKey is where the resolved role travels in a request's context.
// The role is resolved once per request, by the authentication step, and the
// role check reads it from there: a scoped token's row is read from the
// database, and reading it twice per request would double that cost for
// every API call.
type roleContextKey struct{}

// WithRole records the caller's role in the context.
func WithRole(ctx context.Context, role Role) context.Context {
	return context.WithValue(ctx, roleContextKey{}, role)
}

// RoleFrom reads the role the authentication step recorded. ok is false when
// no credential was resolved, which is the caller's own business: the
// authentication step has already answered that case.
func RoleFrom(ctx context.Context) (Role, bool) {
	role, ok := ctx.Value(roleContextKey{}).(Role)
	return role, ok
}

// DenyFunc writes the answer for a request that is authenticated but not
// allowed. Each surface answers in its own words: the API with its JSON
// error envelope, the dashboard with its error page.
type DenyFunc func(w http.ResponseWriter, r *http.Request, required Role)

// RouteExistsFunc reports whether the router has a route for this method and
// path. It is what tells a request for a path that does not exist — which the
// router answers with 404 — from a request for a route nobody gave a role,
// which is denied.
type RouteExistsFunc func(method, path string) bool

// RoleCheck is the role enforcement for one surface: what each route
// requires, how to find a request's route, and how to answer a request that
// is not allowed.
//
// It is the one place roles are enforced. The API and the dashboard each
// build one of these with their own table, and there is no second
// implementation to drift from it.
type RoleCheck struct {
	// Authenticator resolves the caller's role. A nil authenticator — or one
	// with no credential source — means an open instance, which is an admin
	// one.
	Authenticator *Authenticator
	// Table says what each route requires. A route with no entry is denied.
	Table *RouteTable[Role]
	// RoutePath resolves the request's path within the router that served
	// it, so a router mounted under a prefix still matches its own table.
	RoutePath RoutePathFunc
	// RouteExists reports whether the router has a route for this method and
	// path. Without it a request for a path that does not exist would be
	// answered 403 instead of the router's 404.
	RouteExists RouteExistsFunc
	// Deny writes the answer for a request that is not allowed.
	Deny DenyFunc
}

// Middleware returns the middleware that enforces the check.
func (c RoleCheck) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.Path
			if c.RoutePath != nil {
				path = c.RoutePath(r)
			}
			required, ok := c.Table.Lookup(r.Method, path)
			if !ok {
				// The route is not in the table. If the router does not have
				// it either, the request is for a path that does not exist
				// and the router answers 404 — or 405 for a method the route
				// does not take. Either way the role table has nothing to
				// say about it. If the router does have the route, nobody
				// decided what role it takes, and the answer is no.
				if c.RouteExists == nil || !c.RouteExists(r.Method, path) {
					next.ServeHTTP(w, r)
					return
				}
				c.deny(w, r, RoleUnknown)
				return
			}
			role, ok := RoleFrom(r.Context())
			if !ok {
				// The authentication step did not record a role. It has
				// already answered a request with no credential (401 on the
				// API, a redirect to the sign-in page on the dashboard), so
				// this is either a route that asks for no credential or a
				// surface that did not resolve one. Resolving it here keeps
				// the check working on its own; a caller with no credential
				// at all still holds no role.
				if c.Authenticator == nil || !c.Authenticator.Enabled() {
					// No credential source configured: the instance is open,
					// and an open instance is an admin one.
					role = RoleAdmin
				} else {
					role, _ = c.Authenticator.RoleForRequest(r)
				}
			}
			if !role.AtLeast(required) {
				c.deny(w, r, required)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// deny answers a request that is not allowed.
func (c RoleCheck) deny(w http.ResponseWriter, r *http.Request, required Role) {
	if c.Deny != nil {
		c.Deny(w, r, required)
		return
	}
	w.WriteHeader(http.StatusForbidden)
}
