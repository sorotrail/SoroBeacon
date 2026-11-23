package auth

import (
	"net/http"
	"sort"
	"strings"
)

// RouteTable is a lookup from a route to the value that route requires.
//
// The keys are the routes as the routers register them: "METHOD /pattern",
// with {param} standing for one path segment. Two tables use it — the API's
// scope table and the role tables — and they have to resolve a path the same
// way, which is why the matching lives here rather than twice.
//
// The table is deny-by-default: a route with no entry is not found, and every
// caller treats that as "deny". A route nobody looked at is a route nobody
// decided should be reachable, and the alternative — allowing what is
// unknown — is how an added endpoint becomes an unlisted permission.
type RouteTable[T any] struct {
	entries map[string]T
	// patterns is the entry keys in a stable order, so a path that matches
	// more than one pattern is always resolved the same way.
	patterns []string
}

// NewRouteTable builds a table over the given entries. The map is copied, so
// the caller's map cannot change the table's answers afterwards.
func NewRouteTable[T any](entries map[string]T) *RouteTable[T] {
	t := &RouteTable[T]{entries: make(map[string]T, len(entries))}
	for k, v := range entries {
		t.entries[k] = v
		t.patterns = append(t.patterns, k)
	}
	sort.Strings(t.patterns)
	return t
}

// Lookup returns the value the route requires, and whether the route is in
// the table at all.
//
// An exact match wins: the registered pattern is tried first, so a literal
// route is never shadowed by a parameterised one. Failing that, the patterns
// are matched segment by segment, and the most specific match — the one with
// the most literal segments — is the answer.
func (t *RouteTable[T]) Lookup(method, path string) (T, bool) {
	var zero T
	if t == nil {
		return zero, false
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	key := method + " " + NormalizeRoute(path)
	if v, ok := t.entries[key]; ok {
		return v, true
	}
	best := ""
	bestLiterals := -1
	for _, pattern := range t.patterns {
		m, pat, ok := strings.Cut(pattern, " ")
		if !ok || !strings.EqualFold(m, method) {
			continue
		}
		if !PatternMatches(pat, path) {
			continue
		}
		if literals := literalSegments(pat); literals > bestLiterals {
			best, bestLiterals = pattern, literals
		}
	}
	if best == "" {
		return zero, false
	}
	return t.entries[best], true
}

// Patterns returns the table's keys, sorted. A test uses it to compare the
// table with the routes a router actually serves.
func (t *RouteTable[T]) Patterns() []string {
	if t == nil {
		return nil
	}
	out := make([]string, len(t.patterns))
	copy(out, t.patterns)
	return out
}

// NormalizeRoute puts a path in the form the tables are keyed by: a leading
// slash, no trailing slash, and "/" for the root.
func NormalizeRoute(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "/"
	}
	if !strings.HasPrefix(s, "/") {
		s = "/" + s
	}
	if len(s) > 1 {
		s = strings.TrimRight(s, "/")
	}
	if s == "" {
		return "/"
	}
	return s
}

// PatternMatches reports whether a request path matches a route pattern. A
// {param} segment matches any one segment, and the segment counts have to
// agree: /monitors/{id} does not match /monitors/7/rules.
func PatternMatches(pattern, path string) bool {
	pat := splitRouteSegments(pattern)
	seg := splitRouteSegments(path)
	if len(pat) != len(seg) {
		return false
	}
	for i, p := range pat {
		if strings.HasPrefix(p, "{") && strings.HasSuffix(p, "}") {
			continue
		}
		if p != seg[i] {
			return false
		}
	}
	return true
}

// splitRouteSegments splits a path into its segments.
func splitRouteSegments(s string) []string {
	s = NormalizeRoute(s)
	if s == "/" {
		return nil
	}
	return strings.Split(strings.Trim(s, "/"), "/")
}

// literalSegments counts the segments of a pattern that are not parameters.
// It is how the most specific of several matching patterns is chosen.
func literalSegments(pattern string) int {
	n := 0
	for _, seg := range splitRouteSegments(pattern) {
		if !strings.HasPrefix(seg, "{") || !strings.HasSuffix(seg, "}") {
			n++
		}
	}
	return n
}

// RoutePathFunc resolves the path of a request within the router that served
// it. It exists because a router mounted under a prefix sees the whole path
// while its table is keyed by the route inside it: /api/v1/monitors/7 is
// /monitors/{id} to the API's table.
type RoutePathFunc func(r *http.Request) string
