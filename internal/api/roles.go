package api

import (
	"github.com/sorotrail/sorobeacon/internal/auth"
)

// routeRoles is the API's role table: the minimum role that may call each
// route, keyed by "METHOD /pattern" as chi registers it.
//
// The table is the review record. A route with no entry is denied, so
// forgetting it is a broken integration a reviewer sees rather than a silent
// hole, and TestRoleTableCoversEveryRoute keeps the table and the router
// from drifting apart in either direction.
//
// The three roles answer "what may this credential do":
//
//   - A viewer reads. Nothing it can reach changes the instance.
//   - An editor changes what the instance is for: monitors, rules, channels,
//     maintenance windows, and the alerts that flow through them.
//   - An admin does the administrative things: minting and revoking
//     credentials, and reading the audit log of who changed what.
//
// The scope table (internal/api/scopes.go) is a second, finer gate that
// applies to database-backed tokens. A request has to pass both: the role
// says how far the credential reaches, the scopes say which resources within
// that reach it may touch.
var routeRoles = auth.NewRouteTable(map[string]auth.Role{
	// Monitoring: reading is a viewer's job, changing is an editor's.
	"GET /monitors":                        auth.RoleViewer,
	"GET /monitors/{id}":                   auth.RoleViewer,
	"GET /monitors/{id}/stats":             auth.RoleViewer,
	"GET /monitors/{id}/rules":             auth.RoleViewer,
	"GET /monitors/{id}/escalation":        auth.RoleViewer,
	"GET /monitors/trash":                  auth.RoleViewer,
	"POST /monitors":                       auth.RoleEditor,
	"POST /monitors/bulk":                  auth.RoleEditor,
	"POST /monitors/import":                auth.RoleEditor,
	"PATCH /monitors/{id}":                 auth.RoleEditor,
	"DELETE /monitors/{id}":                auth.RoleEditor,
	"POST /monitors/{id}/duplicate":        auth.RoleEditor,
	"POST /monitors/{id}/soft-delete":      auth.RoleEditor,
	"POST /monitors/{id}/restore":          auth.RoleEditor,
	"POST /monitors/{id}/rules":            auth.RoleEditor,
	"POST /monitors/{id}/rules/bulk":       auth.RoleEditor,
	"PATCH /monitors/{id}/rules/{ruleID}":  auth.RoleEditor,
	"DELETE /monitors/{id}/rules/{ruleID}": auth.RoleEditor,
	// A dry run evaluates a rule against stored alerts and persists nothing,
	// so it is a read.
	"POST /monitors/{id}/rules/dry-run": auth.RoleViewer,
	// An escalation policy decides which channels a monitor reaches, so it
	// is an editor's decision.
	"PUT /monitors/{id}/escalation":    auth.RoleEditor,
	"DELETE /monitors/{id}/escalation": auth.RoleEditor,

	// Channels: a channel is where alerts go, so changing one is an editor's
	// decision. Testing one sends a real notification, which is a write even
	// though nothing is stored.
	"GET /channels":                 auth.RoleViewer,
	"GET /channels/{id}":            auth.RoleViewer,
	"GET /channels/{id}/stats":      auth.RoleViewer,
	"POST /channels":                auth.RoleEditor,
	"PATCH /channels/{id}":          auth.RoleEditor,
	"DELETE /channels/{id}":         auth.RoleEditor,
	"POST /channels/bulk":           auth.RoleEditor,
	"POST /channels/{id}/duplicate": auth.RoleEditor,
	"POST /channels/{id}/test":      auth.RoleEditor,
	"GET /channels/{id}/keys":       auth.RoleViewer,
	"POST /channels/{id}/keys/rotate": auth.RoleEditor,
	"POST /channels/{id}/keys/{keyID}/retire": auth.RoleEditor,
	"POST /channels/{id}/keys/{keyID}/revoke": auth.RoleEditor,

	// Templates hold monitors and rules, so instantiating one is an editor's
	// decision too.
	"GET /templates":                        auth.RoleViewer,
	"GET /templates/{id}":                   auth.RoleViewer,
	"POST /templates":                       auth.RoleEditor,
	"PATCH /templates/{id}":                 auth.RoleEditor,
	"DELETE /templates/{id}":                auth.RoleEditor,
	"POST /templates/{id}/instantiate":      auth.RoleEditor,
	"POST /templates/{id}/instantiate/bulk": auth.RoleEditor,

	// Alerts and the queues around them: reading is a viewer's job, and
	// everything that changes what gets delivered is an editor's.
	"GET /alerts":                                    auth.RoleViewer,
	"GET /alerts.csv":                                auth.RoleViewer,
	"GET /alerts/export":                             auth.RoleViewer,
	"GET /alerts/stream":                             auth.RoleViewer,
	"GET /alerts/{id}/deliveries":                    auth.RoleViewer,
	"GET /dead-letters":                              auth.RoleViewer,
	"GET /inhibitions":                               auth.RoleViewer,
	"GET /maintenance-windows":                       auth.RoleViewer,
	"GET /maintenance-windows/{id}":                  auth.RoleViewer,
	"POST /alerts/{id}/acknowledge":                  auth.RoleEditor,
	"POST /alerts/{id}/deliveries/{channelID}/retry": auth.RoleEditor,
	"POST /dead-letters/{id}/redrive":                auth.RoleEditor,
	"POST /ingest":                                   auth.RoleEditor,
	"POST /triggers":                                 auth.RoleEditor,
	"POST /inhibitions":                              auth.RoleEditor,
	"DELETE /inhibitions/{sourceID}/{targetID}":      auth.RoleEditor,
	"POST /maintenance-windows":                      auth.RoleEditor,
	"PATCH /maintenance-windows/{id}":                auth.RoleEditor,
	"DELETE /maintenance-windows/{id}":               auth.RoleEditor,

	// Statistics are a view of the instance, not a change to it.
	"GET /stats":              auth.RoleViewer,
	"GET /stats/alerts-daily": auth.RoleViewer,

	// Credentials are administrative: minting one is minting authority, and
	// the audit log is the instance's record of who has been doing what.
	"GET /tokens":              auth.RoleAdmin,
	"POST /tokens":             auth.RoleAdmin,
	"POST /tokens/{id}/revoke": auth.RoleAdmin,
	"GET /audit":               auth.RoleAdmin,

	// Instance facts: the version, the poller's position and the rule
	// vocabulary are not a tenant's data.
	"GET /version":    auth.RoleViewer,
	"GET /poller":     auth.RoleViewer,
	"GET /rule-types": auth.RoleViewer,

	// The probes answer for the instance's health, not for its data, and an
	// orchestrator has no credential to present.
	"GET /health": auth.RoleOpen,
	"GET /livez":  auth.RoleOpen,
	"GET /readyz": auth.RoleOpen,
})
