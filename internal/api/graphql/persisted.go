package graphql

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/formatter"
	"github.com/vektah/gqlparser/v2/parser"

	"github.com/sorotrail/sorobeacon/internal/store"
)

const (
	// CodePersistedQueryNotFound indicates the operation identifier was not found in the persisted store.
	CodePersistedQueryNotFound = "PERSISTED_QUERY_NOT_FOUND"

	// CodePersistedQueryNotAllowed indicates an ad-hoc query was sent while the server is in allowlist-only mode.
	CodePersistedQueryNotAllowed = "PERSISTED_QUERY_NOT_ALLOWED"

	// CodeQueryDepthExceeded indicates the query nesting depth exceeds ServerConfig.MaxDepth.
	CodeQueryDepthExceeded = "QUERY_DEPTH_EXCEEDED"

	// CodeQueryComplexityExceeded indicates the query complexity exceeds ServerConfig.MaxComplexity.
	CodeQueryComplexityExceeded = "QUERY_COMPLEXITY_EXCEEDED"
)

// NormalizeDocument parses a GraphQL document into an AST and re-formats it into a canonical string.
// This ensures comments, superficial whitespace, and layout differences produce identical identifiers
// while preserving semantically meaningful selections.
func NormalizeDocument(doc string) (string, error) {
	trimmed := strings.TrimSpace(doc)
	if trimmed == "" {
		return "", fmt.Errorf("empty query document")
	}

	parsed, err := parser.ParseQuery(&ast.Source{Input: trimmed})
	if err != nil {
		return "", fmt.Errorf("parse query: %w", err)
	}

	var buf bytes.Buffer
	f := formatter.NewFormatter(&buf)
	f.FormatQueryDocument(parsed)
	return strings.TrimSpace(buf.String()), nil
}

// HashDocument returns the canonical SHA-256 hex string for a normalized document.
func HashDocument(normalizedDoc string) string {
	sum := sha256.Sum256([]byte(normalizedDoc))
	return hex.EncodeToString(sum[:])
}

// ValidateAndHashDocument validates, normalizes, and hashes a document. If expectedHash is non-empty,
// it asserts the computed hash matches the expected one.
func ValidateAndHashDocument(doc string, expectedHash string) (normalized string, hash string, err error) {
	normalized, err = NormalizeDocument(doc)
	if err != nil {
		return "", "", err
	}
	hash = HashDocument(normalized)
	if expectedHash != "" && !strings.EqualFold(hash, strings.TrimSpace(expectedHash)) {
		return "", "", fmt.Errorf("operation hash mismatch: document produces %s, but expected %s", hash, expectedHash)
	}
	return normalized, hash, nil
}

// CalculateQueryDepth returns the maximum field nesting depth of the document.
func CalculateQueryDepth(doc *ast.QueryDocument) int {
	if doc == nil {
		return 0
	}
	maxDepth := 0
	for _, op := range doc.Operations {
		d := selectionSetDepth(op.SelectionSet)
		if d > maxDepth {
			maxDepth = d
		}
	}
	return maxDepth
}

func selectionSetDepth(selSet ast.SelectionSet) int {
	maxChild := 0
	for _, sel := range selSet {
		switch s := sel.(type) {
		case *ast.Field:
			d := 1 + selectionSetDepth(s.SelectionSet)
			if d > maxChild {
				maxChild = d
			}
		case *ast.InlineFragment:
			d := selectionSetDepth(s.SelectionSet)
			if d > maxChild {
				maxChild = d
			}
		}
	}
	return maxChild
}

// CalculateQueryComplexity computes a complexity score for the document taking variables into account.
// Base fields contribute 1. Fields with a "limit" argument multiply the complexity of nested children.
func CalculateQueryComplexity(doc *ast.QueryDocument, vars map[string]any) int {
	if doc == nil {
		return 0
	}
	total := 0
	for _, op := range doc.Operations {
		total += selectionSetComplexity(op.SelectionSet, vars)
	}
	return total
}

func selectionSetComplexity(selSet ast.SelectionSet, vars map[string]any) int {
	cost := 0
	for _, sel := range selSet {
		switch s := sel.(type) {
		case *ast.Field:
			cost++ // base field cost
			mult := 1
			if arg := s.Arguments.ForName("limit"); arg != nil {
				if v, err := arg.Value.Value(vars); err == nil {
					if n := toInt(v); n > 0 {
						mult = n
					}
				}
			}
			childCost := selectionSetComplexity(s.SelectionSet, vars)
			cost += mult * childCost
		case *ast.InlineFragment:
			cost += selectionSetComplexity(s.SelectionSet, vars)
		}
	}
	return cost
}

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return int(n)
	case float64:
		return int(n)
	case string:
		if parsed, err := strconv.Atoi(n); err == nil {
			return parsed
		}
	}
	return 0
}

// DefaultPersistedOperations returns the suite of canonical first-party dashboard operations.
// These are exported and reviewable at build time so the allowlist can be seeded automatically.
func DefaultPersistedOperations() []store.PersistedOperation {
	queries := []struct {
		name string
		doc  string
	}{
		{
			name: "DashboardStats",
			doc:  `query DashboardStats { stats { monitors rules channels alerts alertsLast24h lastLedger lastPollAt } }`,
		},
		{
			name: "DashboardMonitors",
			doc:  `query DashboardMonitors($limit: Int, $cursor: String, $query: String) { monitors(limit: $limit, cursor: $cursor, query: $query) { edges { cursor node { id name enabled priority createdAt lastMatchedAt } } pageInfo { hasNextPage startCursor endCursor } } }`,
		},
		{
			name: "DashboardAlerts",
			doc:  `query DashboardAlerts($limit: Int, $cursor: String, $severity: Severity) { alerts(limit: $limit, cursor: $cursor, severity: $severity) { edges { cursor node { id monitorId ruleId eventId payload createdAt severity ledger } } pageInfo { hasNextPage startCursor endCursor } } }`,
		},
		{
			name: "DashboardAlertSeries",
			doc:  `query DashboardAlertSeries($days: Int) { alertSeries(days: $days) { day count } }`,
		},
	}

	now := time.Now().UTC()
	ops := make([]store.PersistedOperation, len(queries))
	for i, q := range queries {
		norm, err := NormalizeDocument(q.doc)
		if err != nil {
			norm = q.doc
		}
		ops[i] = store.PersistedOperation{
			ID:        HashDocument(norm),
			Name:      q.name,
			Document:  norm,
			CreatedAt: now,
		}
	}
	return ops
}
