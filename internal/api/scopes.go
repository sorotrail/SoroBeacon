package api

import (
	"net/http"
	"strings"

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
var routeScopes = map[string][]auth.Scope{
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

	"POST /channels":        {auth.ScopeChannelsWrite},
	"GET /channels":         {auth.ScopeChannelsRead},
	"GET /channels/{id}":    {auth.ScopeChannelsRead},
	"PATCH /channels/{id}":  {auth.ScopeChannelsWrite},
	"DELETE /channels/{id}": {auth.ScopeChannelsWrite},
	// Testing a channel sends a real notification through it, which is a write
	// even though nothing is stored.
	"POST /channels/{id}/test": {auth.ScopeChannelsWrite},

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

	"POST /tokens":             {auth.ScopeTokensWrite},
	"GET /tokens":              {auth.ScopeTokensRead},
	"POST /tokens/{id}/revoke": {auth.ScopeTokensWrite},
}

// requiredScopes resolves the permissions a request needs. ok false means the
// route is not in the table, which the caller treats as a denial.
func requiredScopes(method, path string) (scopes []auth.Scope, ok bool) {
	// Exact key first: it covers every literal pattern and costs nothing.
	if s, hit := routeScopes[method+" "+normalizeRoute(path)]; hit {
		return s, true
	}
	// Parameterised patterns need segment matching, so the table stays the
	// single list a reviewer reads rather than becoming a set of regexes.
	for key, s := range routeScopes {
		m, pattern, _ := strings.Cut(key, " ")
		if m != method || !patternMatches(pattern, path) {
			continue
		}
		return s, true
	}
	return nil, false
}

// patternMatches reports whether a chi route pattern ("monitors/{id}/rules")
// selects the request path ("/monitors/7/rules"). A {param} segment matches any
// single segment: patterns are matched segment by segment rather than by prefix
// so "/monitors/{id}" cannot also select "/monitors/7/rules", which is a
// different route with (here) a different write scope.
func patternMatches(pattern, path string) bool {
	p := splitSegments(pattern)
	q := splitSegments(path)
	if len(p) != len(q) {
		return false
	}
	for i := range p {
		if strings.HasPrefix(p[i], "{") && strings.HasSuffix(p[i], "}") {
			continue
		}
		if p[i] != q[i] {
			return false
		}
	}
	return true
}

func splitSegments(s string) []string {
	return strings.Split(strings.Trim(normalizeRoute(s), "/"), "/")
}

// normalizeRoute makes "/monitors", "/monitors/" and "monitors" one key, so a
// pattern as chi reports it (which carries the trailing slash a Mount leaves)
// and a path as the router received it agree. The canonical form is the one
// routeScopes is written in: a leading slash, no trailing one, "/" for the root.
func normalizeRoute(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || s == "/" {
		return "/"
	}
	if !strings.HasPrefix(s, "/") {
		s = "/" + s
	}
	return strings.TrimSuffix(s, "/")
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
