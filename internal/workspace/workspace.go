// Package workspace defines SoroBeacon's tenancy boundary: the named scope
// that every monitor, channel, rule, alert, saved search and template belongs
// to, and the context plumbing that carries it from the authenticated
// principal down into the store.
//
// It is a leaf package on purpose. The store imports it to scope queries, the
// API and dashboard import it to resolve the caller, and nothing here imports
// either, so adding a tenancy concern cannot create an import cycle.
package workspace

import (
	"context"
	"errors"
)

// ErrInvalidID reports a workspace id outside the grammar Valid accepts. The
// value is never included in the error: an id can appear in a log line, and a
// rejected value is exactly the kind of string that should not be echoed.
var ErrInvalidID = errors.New("invalid workspace id")

// Default is the workspace every pre-existing row belongs to and the one an
// unconfigured single-tenant instance uses. Its id is fixed so a migration can
// insert it and every column default can name it without generating one.
const Default ID = "default"

// idMaxLen bounds an id so it stays usable as a slug in URLs and log lines.
const idMaxLen = 40

// ID names one workspace. The zero value is not a valid workspace: build one
// with Parse, or use Default.
//
// The type exists so a scoped query can only be given a value that has been
// through Parse. IDs are stored and compared as opaque strings, never
// interpreted, so the only thing that can ever reach SQL as a workspace is a
// string from this set.
type ID string

// String implements fmt.Stringer.
func (id ID) String() string { return string(id) }

// Valid reports whether s is a workspace id: lowercase letters, digits, hyphen
// and underscore, 1 to idMaxLen characters, starting with a letter or digit.
//
// The grammar is deliberately narrow. It is checked by Parse, and a
// workspace-only predicate is built from the validated value, so the character
// set is the injection guard rather than any quoting done downstream.
func Valid(s string) bool {
	if len(s) == 0 || len(s) > idMaxLen {
		return false
	}
	if !isAlnum(s[0]) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isAlnum(c) && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

// Parse validates s as a workspace id. The empty string maps to Default so a
// deployment that never configured tenancy keeps working; any other invalid
// value is rejected rather than silently downgraded, because a typo'd
// workspace name would otherwise read and write another tenant's data.
func Parse(s string) (ID, error) {
	if s == "" {
		return Default, nil
	}
	if !Valid(s) {
		return "", ErrInvalidID
	}
	return ID(s), nil
}

// ctxKey is private so no other package can set or read the value by accident;
// the only supported route through a context is With/From.
type ctxKey struct{}

// systemKey marks a context as belonging to the whole instance rather than one
// workspace. It is a separate key from ctxKey so "no workspace" (a bug, and
// rejected by scoped store methods) cannot be confused with "every workspace"
// (a deliberate choice, available only to the few callers below).
type systemKey struct{}

// With returns ctx scoped to id.
func With(ctx context.Context, id ID) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// From returns the workspace id ctx is scoped to. ok is false when ctx carries
// no workspace at all, and also when it carries the system scope: callers that
// accept either must ask System separately.
func From(ctx context.Context) (id ID, ok bool) {
	v, ok := ctx.Value(ctxKey{}).(ID)
	if !ok {
		return "", false
	}
	return v, true
}

// WithSystem marks ctx as cross-tenant: the instance-level work that owns every
// workspace's rows at once — the retention pruner, the poller's ingest loop,
// reorg handling. Store methods that can serve it say so in their own comments;
// the rest resolve such a context to the default workspace, which is wrong but
// bounded, never a cross-workspace read.
func WithSystem(ctx context.Context) context.Context {
	return context.WithValue(ctx, systemKey{}, true)
}

// System reports whether ctx was marked cross-tenant.
func System(ctx context.Context) bool {
	v, _ := ctx.Value(systemKey{}).(bool)
	return v
}
