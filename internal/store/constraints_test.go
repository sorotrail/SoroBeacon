package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A constraint violation has to reach the caller as a typed store error, in
// both backends and for every kind of constraint a request can cause. This is
// the translation the HTTP layer's 422 answer is built on, so it is pinned
// here rather than only through the API.
func TestConstraintTranslationIsTyped(t *testing.T) {
	t.Run("postgres", func(t *testing.T) {
		tests := []struct {
			name string
			code string
			kind ConstraintKind
		}{
			{"foreign key", pgForeignKeyViolation, ConstraintForeignKey},
			{"check", pgCheckViolation, ConstraintCheck},
			{"unique", pgUniqueViolation, ConstraintUnique},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				err := mapErr(&pgconn.PgError{Code: tt.code, ConstraintName: "some_constraint"})
				require.Error(t, err)
				assert.ErrorIs(t, err, ErrConstraint)
				kind, ok := ConstraintKindOf(err)
				require.True(t, ok, "the error must name the constraint kind")
				assert.Equal(t, tt.kind, kind)
			})
		}
	})

	t.Run("sqlite", func(t *testing.T) {
		for _, tt := range sqliteConstraintErrors(t) {
			t.Run(tt.name, func(t *testing.T) {
				err := mapSQLiteErr(tt.err)
				require.Error(t, err)
				assert.ErrorIs(t, err, ErrConstraint)
				kind, ok := ConstraintKindOf(err)
				require.True(t, ok, "the error must name the constraint kind")
				assert.Equal(t, tt.kind, kind)
			})
		}
	})
}

// sqliteConstraintErrors produces the errors SQLite itself reports for the
// three constraint kinds a request can cause. They are made by a real
// database rather than by hand: the driver error is what the store sees, and
// its extended result code is what the translation keys on.
func sqliteConstraintErrors(t *testing.T) []struct {
	name string
	err  error
	kind ConstraintKind
} {
	t.Helper()
	db, err := sql.Open(sqliteDriver, "file:"+filepath.Join(t.TempDir(), "constraints.db")+"?_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE TABLE parent (id INTEGER PRIMARY KEY)`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE child (
		id        INTEGER PRIMARY KEY,
		parent_id INTEGER REFERENCES parent (id),
		severity  TEXT NOT NULL CHECK (severity IN ('info', 'warning')),
		name      TEXT NOT NULL UNIQUE
	)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO parent (id) VALUES (1)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO child (id, parent_id, severity, name) VALUES (1, 1, 'info', 'first')`)
	require.NoError(t, err)

	fkErr := execErr(t, db, `INSERT INTO child (id, parent_id, severity, name) VALUES (2, 999, 'info', 'fk')`)
	checkErr := execErr(t, db, `INSERT INTO child (id, parent_id, severity, name) VALUES (3, 1, 'nonsense', 'check')`)
	uniqueErr := execErr(t, db, `INSERT INTO child (id, parent_id, severity, name) VALUES (4, 1, 'info', 'first')`)

	return []struct {
		name string
		err  error
		kind ConstraintKind
	}{
		{"foreign key", fkErr, ConstraintForeignKey},
		{"check", checkErr, ConstraintCheck},
		{"unique", uniqueErr, ConstraintUnique},
	}
}

// execErr runs a statement that is expected to fail and returns the error.
func execErr(t *testing.T, db *sql.DB, stmt string) error {
	t.Helper()
	_, err := db.Exec(stmt)
	require.Error(t, err, "the statement was expected to violate a constraint")
	return err
}

// A foreign-key violation means the row the request named does not exist, so
// it is a missing row as well as a constraint violation. Both answers stay
// available: the callers that only know ErrNotFound keep the answer they have
// always had, and the ones that know ErrConstraint can say the request was at
// fault.
func TestForeignKeyViolationIsAlsoNotFound(t *testing.T) {
	pgErr := mapErr(&pgconn.PgError{Code: pgForeignKeyViolation, ConstraintName: "rules_monitor_id_fkey"})
	assert.ErrorIs(t, pgErr, ErrNotFound)
	assert.ErrorIs(t, pgErr, ErrConstraint)

	for _, tt := range sqliteConstraintErrors(t) {
		if tt.kind != ConstraintForeignKey {
			continue
		}
		sqliteErr := mapSQLiteErr(tt.err)
		assert.ErrorIs(t, sqliteErr, ErrNotFound)
		assert.ErrorIs(t, sqliteErr, ErrConstraint)
	}
}

// A check violation is not a missing row: the value is wrong, not absent, so
// it must not be reported as "not found".
func TestCheckViolationIsNotNotFound(t *testing.T) {
	err := mapErr(&pgconn.PgError{Code: pgCheckViolation, ConstraintName: "alerts_severity_check"})
	assert.NotErrorIs(t, err, ErrNotFound)
	assert.ErrorIs(t, err, ErrConstraint)
}

// The error a constraint violation produces must not quote the statement that
// failed: it travels to an HTTP response and to a log line.
func TestConstraintErrorDoesNotEchoSQL(t *testing.T) {
	err := mapErr(&pgconn.PgError{Code: pgForeignKeyViolation, ConstraintName: "alerts_monitor_id_fkey"})
	assert.NotContains(t, err.Error(), "INSERT")
	assert.NotContains(t, err.Error(), "SELECT")
	assert.NotContains(t, err.Error(), "alerts (")
}

// A write that names a record which does not exist is a constraint violation
// on both backends, and the conformance suite runs it against each of them.
func testConstraintViolationsAreTyped(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	rule := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, rule))

	// An inhibition naming a rule that does not exist violates the foreign
	// key. The API asks for this to be a client error, so the store has to
	// say so in a way the API can recognise.
	err := st.CreateInhibition(ctx, &Inhibition{SourceRuleID: rule.ID + 1000000, TargetRuleID: rule.ID})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrConstraint)
	kind, ok := ConstraintKindOf(err)
	require.True(t, ok)
	assert.Equal(t, ConstraintForeignKey, kind)

	// A name already taken in the same workspace violates the unique index.
	dup := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	err = st.CreateMonitor(ctx, dup)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrConstraint)
	kind, ok = ConstraintKindOf(err)
	require.True(t, ok)
	assert.Equal(t, ConstraintUnique, kind)
}
