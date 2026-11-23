// Package store defines SoroBeacon's persistence interfaces and models.
// The Postgres implementation lives in postgres.go; tests and alternative
// backends can implement the narrow per-domain interfaces below.
package store

// ValidAuditSurface reports whether s is a value the store actually
// writes/reads for audit surface validation. The empty string is not valid
// here — callers that mean "no constraint" should check for empty themselves.
func ValidAuditSurface(s string) bool {
	return s == "monitor" || s == "rule" || s == "channel"
}