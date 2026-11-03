package web

import (
	"github.com/sorotrail/sorobeacon/internal/auth"
)

// routeRoles is the dashboard's role table: the minimum role that may reach
// each page, keyed by "METHOD /pattern" as chi registers it.
//
// It answers the same question the API's table does (internal/api/roles.go),
// for the same three roles, and it is enforced by the same middleware: one
// enforcement path, two tables.
//
// The dashboard's pages are all mutations except the read-only ones, so the
// table is mostly a line between "looking" and "changing". The pages that
// need no credential at all are the sign-in flow and the two that only set
// presentation cookies.
var routeRoles = auth.NewRouteTable(map[string]auth.Role{
	// The sign-in flow, the sign-out and the presentation cookies are
	// reachable without a session: a visitor who is not signed in has to be
	// able to sign in.
	"GET /login":               auth.RoleOpen,
	"POST /login":              auth.RoleOpen,
	"POST /logout":             auth.RoleOpen,
	"GET /login/oidc":          auth.RoleOpen,
	"GET /login/oidc/callback": auth.RoleOpen,
	"GET /favicon.ico":         auth.RoleOpen,
	"POST /theme":              auth.RoleOpen,
	"POST /timezone":           auth.RoleOpen,

	// Reading.
	"GET /":                       auth.RoleViewer,
	"GET /monitors":               auth.RoleViewer,
	"GET /monitors/{id}":          auth.RoleViewer,
	"GET /monitors/trash":         auth.RoleViewer,
	"GET /channels":               auth.RoleViewer,
	"GET /channels/{id}/edit":     auth.RoleViewer,
	"GET /rulebuilder/{type}":     auth.RoleViewer,
	"GET /alerts":                 auth.RoleViewer,
	"GET /alerts/{id}":            auth.RoleViewer,
	"GET /alerts/{id}/deliveries": auth.RoleViewer,
	"GET /dead-letters":           auth.RoleViewer,
	"GET /maintenance":            auth.RoleViewer,
	"GET /searches":               auth.RoleViewer,

	// Changing what the instance watches and where it sends.
	"POST /monitors":                            auth.RoleEditor,
	"POST /monitors/bulk":                       auth.RoleEditor,
	"POST /monitors/import":                     auth.RoleEditor,
	"POST /monitors/{id}/toggle":                auth.RoleEditor,
	"POST /monitors/{id}/delete":                auth.RoleEditor,
	"POST /monitors/{id}/soft-delete":           auth.RoleEditor,
	"POST /monitors/{id}/restore":               auth.RoleEditor,
	"POST /monitors/{id}/duplicate":             auth.RoleEditor,
	"POST /monitors/{id}/rules":                 auth.RoleEditor,
	"POST /monitors/{id}/rules/{ruleID}/toggle": auth.RoleEditor,
	"POST /monitors/{id}/rules/{ruleID}/delete": auth.RoleEditor,
	"POST /monitors/{id}/channels":              auth.RoleEditor,
	"POST /channels":                            auth.RoleEditor,
	"POST /channels/{id}":                       auth.RoleEditor,
	"POST /channels/{id}/delete":                auth.RoleEditor,
	"POST /channels/{id}/test":                  auth.RoleEditor,
	"POST /channels/{id}/toggle":                auth.RoleEditor,
	"POST /channels/bulk":                       auth.RoleEditor,
	"POST /channels/{id}/duplicate":             auth.RoleEditor,

	// What gets delivered, and when. Acknowledging is writing to the alert
	// the same way a retry is: it stops escalation, so it is an editor's
	// action, and the API table asks the same role for its own route to the
	// same store method.
	"POST /maintenance":                              auth.RoleEditor,
	"POST /maintenance/{id}/delete":                  auth.RoleEditor,
	"POST /alerts/{id}/deliveries/{channelID}/retry": auth.RoleEditor,
	"POST /alerts/{id}/acknowledge":                  auth.RoleEditor,
	"POST /dead-letters/{id}/redrive":                auth.RoleEditor,
	"POST /dead-letters/{id}/discard":                auth.RoleEditor,
	"POST /dead-letters/bulk/redrive":                auth.RoleEditor,
	"POST /dead-letters/bulk/discard":                auth.RoleEditor,

	// Saved searches are one operator's own view of the alert history, so
	// they are an editor's to change.
	"POST /searches":                auth.RoleEditor,
	"POST /searches/{id}/delete":    auth.RoleEditor,
	"POST /searches/{id}/default":   auth.RoleEditor,
	"POST /searches/{id}/undefault": auth.RoleEditor,

	// Credentials are administrative: minting one is minting authority.
	"GET /tokens":              auth.RoleAdmin,
	"POST /tokens":             auth.RoleAdmin,
	"POST /tokens/{id}/revoke": auth.RoleAdmin,

	// The audit log names the credential behind every change, so reading it
	// is administrative the way the API's own route makes it: the role table
	// in internal/api/roles.go asks admin for GET /api/v1/audit too.
	"GET /audit": auth.RoleAdmin,
})
