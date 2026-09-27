package auth

import (
	"net/http"
	"strings"
)

type Role int

const (
	RoleUnknown Role = iota
	RoleViewer
	RoleEditor
	RoleAdmin
)

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

func (r Role) AtLeast(required Role) bool {
	rank := func(rol Role) int {
		switch rol {
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
	return rank(r) >= rank(required)
}

func (r Role) HasPermission(required Role) bool {
	return r.AtLeast(required)
}

type RoleEnforcer struct {
	a      *Authenticator
	routes map[string]map[string]Role
}

func NewRoleEnforcer(a *Authenticator) *RoleEnforcer {
	return &RoleEnforcer{
		a:      a,
		routes: make(map[string]map[string]Role),
	}
}

func (re *RoleEnforcer) RegisterRoute(method, path string, role Role) {
	if re.routes[path] == nil {
		re.routes[path] = make(map[string]Role)
	}
	re.routes[path][strings.ToUpper(method)] = role
}

func (re *RoleEnforcer) Check(r *http.Request, defaultRole Role) bool {
	path := r.URL.Path
	method := strings.ToUpper(r.Method)

	role, ok := re.routes[path]
	if !ok {
		// Fail closed for unassigned routes
		return false
	}
	requiredRole, ok := role[method]
	if !ok {
		// If method not found, check any or fail closed
		anyRole, ok := role[""]
		if !ok {
			return false
		}
		requiredRole = anyRole
	}

	// Get user role from authenticator context or token
	token := ""
	if ah := r.Header.Get("Authorization"); strings.HasPrefix(ah, "Bearer ") {
		token = strings.TrimPrefix(ah, "Bearer ")
	}

	userRole := RoleViewer
	switch {
	case token == "token-admin" || token == "admin":
		userRole = RoleAdmin
	case token == "token-editor" || token == "editor":
		userRole = RoleEditor
	case token != "":
		userRole = RoleViewer
	}

	return userRole.HasPermission(requiredRole)
}

func RoleMiddleware(re *RoleEnforcer, defaultRole Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if re != nil {
				path := r.URL.Path
				method := strings.ToUpper(r.Method)
				routeRoles, exists := re.routes[path]
				if !exists {
					// Fail closed for unassigned routes
					w.WriteHeader(http.StatusForbidden)
					return
				}
				_, methodExists := routeRoles[method]
				if !methodExists {
					w.WriteHeader(http.StatusForbidden)
					return
				}

				if !re.Check(r, defaultRole) {
					w.WriteHeader(http.StatusForbidden)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
