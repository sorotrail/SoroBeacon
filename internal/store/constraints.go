package store

import (
	"errors"
	"fmt"
)

// ErrConstraint reports that the database rejected a write because the row
// violated a constraint: a foreign key naming a row that does not exist, a
// CHECK the value fails, or a unique index the value collides with.
//
// It is a client error rather than a server fault. The request asked for
// something the database will not accept, and sending the same request again
// fails the same way, so an HTTP layer must answer 4xx and a sender on the
// other side must not treat it as transient.
var ErrConstraint = errors.New("constraint violation")

// ConstraintKind names the sort of constraint a write violated. The kinds are
// a closed set so a caller can react to the failure without parsing a
// database-specific message.
type ConstraintKind string

const (
	// ConstraintForeignKey is a reference to a row that does not exist.
	ConstraintForeignKey ConstraintKind = "foreign_key"
	// ConstraintCheck is a value the column's CHECK constraint refuses.
	ConstraintCheck ConstraintKind = "check"
	// ConstraintUnique is a value that collides with an existing row.
	ConstraintUnique ConstraintKind = "unique"
)

// ConstraintError is the typed error a constraint violation produces. It
// carries the kind and the constraint's name, which is schema metadata and
// never caller data, so it is safe to log; it never carries the statement
// that failed, so it cannot leak a query or a value into a response.
type ConstraintError struct {
	// Kind is which sort of constraint was violated.
	Kind ConstraintKind
	// Constraint is the name the database reported, such as
	// "alerts_monitor_id_fkey". It is empty when the backend does not report
	// one, which is the case for SQLite.
	Constraint string
}

// Error describes the violation without quoting the statement that caused it.
func (e *ConstraintError) Error() string {
	if e.Constraint == "" {
		return fmt.Sprintf("constraint violation (%s)", e.Kind)
	}
	return fmt.Sprintf("constraint violation (%s): %s", e.Kind, e.Constraint)
}

// Is makes every constraint error match ErrConstraint, so a caller can ask
// the one question it cares about without knowing which backend produced the
// error.
func (e *ConstraintError) Is(target error) bool { return target == ErrConstraint }

// ConstraintKindOf reports which constraint an error violated, and whether it
// was a constraint violation at all. It is the helper an HTTP layer uses to
// turn a store error into a message that says what the caller did wrong
// without echoing SQL.
func ConstraintKindOf(err error) (ConstraintKind, bool) {
	var ce *ConstraintError
	if errors.As(err, &ce) {
		return ce.Kind, true
	}
	return "", false
}
