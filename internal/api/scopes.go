package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/sorotrail/sorobeacon/internal/auth"
)

// routeScopes is the API's authorization table: which permission each route
// asks a scoped token for, keyed by "METHOD /pattern" as chi registers it.
//
// Resource-level scopes (monitors:read) rather than one scope per route: the
// per-route alternative reads as less arbitrary but has to be edited every
// time an endpoint is added, and it makes "can this token see monitors?" a
// question with no answer a human can review. Every entry here is AND-ed —
// a route that creates monitors through a template asks for both writes — so
// a scope means what it says regardless of which door was used.
//
// An entry's absence is a denial, not an allowance: the scope check fails
// closed on anything unmapped, so forgetting this table is a broken integration
// a reviewer sees, not a silent hole. TestScopeTableCoversEveryRoute
// is what keeps the table and the router from drifting apart in either direction.
var routeScopes = auth.NewRouteTable(map[string][]auth.Scope{
	"POST /monitors":                       {auth.ScopeMonitorsWrite},
	"GET /monitors":                        {auth.ScopeMonitorsRead},
	"POST /monitors/bulk":                  {auth.ScopeMonitorsWrite},
	"GET /monitors/{id}":                   {auth.ScopeMonitorsRead},
	"PATCH /monitors/{id}":                 {auth.ScopeMonitorsWrite},
	"DELETE /monitors/{id}":                {auth.ScopeMonitorsWrite},
	"POST /monitors/{id}/duplicate":        {auth.ScopeMonitorsWrite},
	"POST /monitors/{id}/rules":            {auth.ScopeMonitorsWrite},
	"GET /monitors/{id}/rules":             {auth.ScopeMonitorsRead},
	"POST /monitors/{id}/rules/bulk":       {auth.ScopeMonitorsWrite},
	"PATCH /monitors/{id}/rules/{ruleID}":  {auth.ScopeMonitorsWrite},
	"DELETE /monitors/{id}/rules/{ruleID}": {auth.ScopeMonitorsWrite},
	"POST /monitors/import":                {auth.ScopeMonitorsWrite},
	"GET /monitors/trash":                  {auth.ScopeMonitorsRead},
	"POST /monitors/{id}/soft-delete":      {auth.ScopeMonitorsWrite},
	"POST /monitors/{id}/restore":          {auth.ScopeMonitorsWrite},

	"POST /channels":     {auth.ScopeChannelsWrite},
	"GET /channels":      {auth.ScopeChannelsRead},
	"GET /channels/{id}": {auth.ScopeChannelsRead},
	// Delivery statistics read the same history GET /monitors/{id}/stats
	// reads, so they ask for the same stats scope: a token that may see the
	// instance's numbers may see one channel's.
	"GET /channels/{id}/stats": {auth.ScopeStatsRead},
	"PATCH /channels/{id}":     {auth.ScopeChannelsWrite},
	"DELETE /channels/{id}":    {auth.ScopeChannelsWrite},
	// Testing a channel sends a real notification through it, which is a write
	// even though nothing is stored.
	"POST /channels/{id}/test":                {auth.ScopeChannelsWrite},
	"POST /channels/bulk":                     {auth.ScopeChannelsWrite},
	"POST /channels/{id}/duplicate":           {auth.ScopeChannelsWrite},
	"GET /channels/{id}/keys":                 {auth.ScopeChannelsRead},
	"POST /channels/{id}/keys/rotate":         {auth.ScopeChannelsWrite},
	"POST /channels/{id}/keys/{keyID}/retire": {auth.ScopeChannelsWrite},
	"POST /channels/{id}/keys/{keyID}/revoke": {auth.ScopeChannelsWrite},

	"POST /templates":        {auth.ScopeTemplatesWrite},
	"GET /templates":         {auth.ScopeTemplatesRead},
	"GET /templates/{id}":    {auth.ScopeTemplatesRead},
	"PATCH /templates/{id}":  {auth.ScopeTemplatesWrite},
	"DELETE /templates/{id}": {auth.ScopeTemplatesWrite},
	// Instantiating creates a monitor, so a token that can only edit templates
	// cannot use one as a way to add monitors.
	"POST /templates/{id}/instantiate":      {auth.ScopeTemplatesWrite, auth.ScopeMonitorsWrite},
	"POST /templates/{id}/instantiate/bulk": {auth.ScopeTemplatesWrite, auth.ScopeMonitorsWrite},

	"GET /alerts":                 {auth.ScopeAlertsRead},
	"GET /alerts.csv":             {auth.ScopeAlertsRead},
	"GET /alerts/{id}/deliveries": {auth.ScopeAlertsRead},
	"POST /alerts/{id}/deliveries/{channelID}/retry": {auth.ScopeAlertsWrite},

	"GET /version": {auth.ScopeNone},
	// /health, /livez and /readyz are absent on purpose: they are exempt from
	// authentication altogether (isProbePath), so there is no credential to
	// scope. The coverage test skips them for the same reason.

	"GET /stats":              {auth.ScopeStatsRead},
	"GET /stats/alerts-daily": {auth.ScopeAlertsRead},
	"GET /usage":              {auth.ScopeStatsRead},
	"GET /quotas":             {auth.ScopeStatsRead},
	"PUT /quotas":             {auth.ScopeMonitorsWrite},
	"GET /retention":          {auth.ScopeStatsRead},
	"PUT /retention":          {auth.ScopeMonitorsWrite},
	"DELETE /retention":       {auth.ScopeMonitorsWrite},

	"DELETE /workspaces/{id}": {
		auth.ScopeMonitorsWrite,
		auth.ScopeChannelsWrite,
		auth.ScopeAlertsWrite,
		auth.ScopeTemplatesWrite,
		auth.ScopeTokensWrite,
	},
	"POST /workspaces/{id}/delete": {
		auth.ScopeMonitorsWrite,
		auth.ScopeChannelsWrite,
		auth.ScopeAlertsWrite,
		auth.ScopeTemplatesWrite,
		auth.ScopeTokensWrite,
	},
	"GET /workspaces/{id}/delete": {
		auth.ScopeMonitorsRead,
		auth.ScopeChannelsRead,
		auth.ScopeAlertsRead,
		auth.ScopeTemplatesRead,
		auth.ScopeTokensRead,
	},

	"POST /tokens":              {auth.ScopeTokensWrite},
	"GET /tokens":               {auth.ScopeTokensRead},
	"GET /tokens/{id}":          {auth.ScopeTokensRead},
	"POST /tokens/{id}/rotate":  {auth.ScopeTokensWrite},
	"POST /tokens/{id}/abandon": {auth.ScopeTokensWrite},
	"POST /tokens/{id}/revoke":  {auth.ScopeTokensWrite},

	// Persisted GraphQL operations:
	"GET /graphql/operations":         {auth.ScopeNone},
	"GET /graphql/operations/{id}":    {auth.ScopeNone},
	"POST /graphql/operations":        {auth.ScopeMonitorsWrite},
	"DELETE /graphql/operations/{id}": {auth.ScopeMonitorsWrite},

	// Routes main added after the scope table was first written. Each one is
	// mapped to the resource it reads or writes, never to a new scope: the
	// vocabulary an operator grants from stays the short, reviewable list.
	"GET /alerts/export": {auth.ScopeAlertsRead},
	"GET /alerts/stream": {auth.ScopeAlertsRead},
	// Acknowledging stops an escalation, which changes what gets delivered.
	"POST /alerts/{id}/acknowledge": {auth.ScopeAlertsWrite},
	// The audit log records monitor, rule and channel edits, so reading it is
	// reading those resources' history.
	"GET /audit": {auth.ScopeMonitorsRead},
	// Ingest writes alerts: it is the push counterpart of the poller.
	"POST /ingest": {auth.ScopeAlertsWrite},
	// A trigger asks the instance to evaluate an external event into an
	// alert, so it writes alerts too.
	"POST /triggers": {auth.ScopeAlertsWrite},
	// Inhibitions and maintenance windows decide whether an alert is
	// delivered, so they are alert writes rather than monitor edits.
	"POST /inhibitions":                         {auth.ScopeAlertsWrite},
	"GET /inhibitions":                          {auth.ScopeAlertsRead},
	"DELETE /inhibitions/{sourceID}/{targetID}": {auth.ScopeAlertsWrite},
	"GET /maintenance-windows":                  {auth.ScopeAlertsRead},
	"POST /maintenance-windows":                 {auth.ScopeAlertsWrite},
	"GET /maintenance-windows/{id}":             {auth.ScopeAlertsRead},
	"PATCH /maintenance-windows/{id}":           {auth.ScopeAlertsWrite},
	"DELETE /maintenance-windows/{id}":          {auth.ScopeAlertsWrite},
	// An escalation policy belongs to a monitor and decides which channels it
	// reaches, so editing one asks for both writes.
	"GET /monitors/{id}/escalation":    {auth.ScopeMonitorsRead},
	"PUT /monitors/{id}/escalation":    {auth.ScopeMonitorsWrite, auth.ScopeChannelsWrite},
	"DELETE /monitors/{id}/escalation": {auth.ScopeMonitorsWrite},
	// A dry run evaluates a candidate rule against stored alerts and persists
	// nothing, so it reads both and writes neither.
	"POST /monitors/{id}/rules/dry-run": {auth.ScopeMonitorsRead, auth.ScopeAlertsRead},
	"GET /monitors/{id}/stats":          {auth.ScopeStatsRead},
	// The poller's position and the rule-type list are instance facts, not a
	// tenant's data: any valid credential may read them.
	"GET /poller":     {auth.ScopeNone},
	"GET /rule-types": {auth.ScopeNone},
	// A dead letter is a delivery that exhausted its retries, so reading the
	// queue reads alerts, and redriving one sends a notification: that is an
	// alert write, not a channel edit.
	"GET /dead-letters":               {auth.ScopeAlertsRead},
	"GET /dead-letters/groups":        {auth.ScopeAlertsRead},
	"POST /dead-letters/{id}/redrive": {auth.ScopeAlertsWrite},
	"POST /dead-letters/{id}/discard": {auth.ScopeAlertsWrite},
	"POST /dead-letters/bulk/redrive": {auth.ScopeAlertsWrite},
	"POST /dead-letters/bulk/discard": {auth.ScopeAlertsWrite},
})

// requiredScopes resolves the permissions a request needs. ok false means the
// route is not in the table, which the caller treats as a denial.
//
// The matching lives in the auth package, because the role table resolves
// paths the same way and the two must not disagree about what a route is.
func requiredScopes(method, path string) (scopes []auth.Scope, ok bool) {
	return routeScopes.Lookup(method, path)
}

// routePath is the path to match against the table: what the mounted router
// still has to route, which excludes the /api/v1 prefix the pattern list is
// written without. Chi sets it while routing the request; the URL fallback is
// for a router mounted at the root, where the two are the same string anyway.
func routePath(r *http.Request) string {
	if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePath != "" {
		return rc.RoutePath
	}
	return r.URL.Path
}
