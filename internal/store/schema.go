package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// SchemaStatus describes how far the database's schema has been migrated
// relative to the migrations this binary carries.
type SchemaStatus struct {
	// Current is the migration version the database records. Zero means the
	// migrations have never run against it.
	Current uint
	// Expected is the highest migration version this binary carries.
	Expected uint
	// Dirty is true when a migration failed part-way: the schema is then in
	// a state neither the old nor the new version describes, and only a
	// human can decide what to do about it.
	Dirty bool
	// Migrated is true when the database is at the expected version and
	// clean.
	Migrated bool
}

// CheckSchema reports the migration version the database is at and the one
// this binary expects.
//
// It opens a short-lived connection of its own and only ever reads: a
// configuration check must not change the database it inspects. A database
// that has never been migrated has no schema_migrations table, which is
// reported as version 0 rather than as an error — "the migrations have not
// run" is a fact about the deployment, not a failure to read the database.
//
// The connection error is returned as-is so the caller can tell "the
// database is unreachable" from "the schema is behind"; the two need
// different answers.
func CheckSchema(ctx context.Context, databaseURL string) (SchemaStatus, error) {
	scheme, err := Scheme(databaseURL)
	if err != nil {
		return SchemaStatus{}, err
	}
	status := SchemaStatus{Expected: expectedMigrationVersion(scheme)}
	switch scheme {
	case "postgres", "postgresql":
		return checkPostgresSchema(ctx, databaseURL, status)
	case "sqlite":
		return checkSQLiteSchema(ctx, databaseURL, status)
	default:
		return SchemaStatus{}, fmt.Errorf("unsupported DATABASE_URL scheme %q (supported schemes: postgres, postgresql, sqlite)", scheme)
	}
}

// checkPostgresSchema pings Postgres and reads the migration version.
func checkPostgresSchema(ctx context.Context, databaseURL string, status SchemaStatus) (SchemaStatus, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return status, fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.PingContext(ctx); err != nil {
		return status, fmt.Errorf("database is unreachable: %w", err)
	}
	version, dirty, err := readMigrationVersion(ctx, db)
	if err != nil {
		return status, err
	}
	status.Current, status.Dirty = version, dirty
	status.Migrated = !dirty && version == status.Expected
	return status, nil
}

// checkSQLiteSchema checks the database file and reads the migration
// version. The file's existence is checked first: opening a SQLite
// database creates it, and a configuration check that creates an empty
// database file is a side effect nobody asked for.
func checkSQLiteSchema(ctx context.Context, databaseURL string, status SchemaStatus) (SchemaStatus, error) {
	path, err := sqliteFilePath(databaseURL)
	if err != nil {
		return status, err
	}
	if !strings.HasPrefix(path, ":memory:") {
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return status, fmt.Errorf("database file %s does not exist", path)
			}
			return status, fmt.Errorf("stat database file: %w", err)
		}
	}
	dsn, err := sqliteDSN(databaseURL)
	if err != nil {
		return status, err
	}
	db, err := sql.Open(sqliteDriver, dsn)
	if err != nil {
		return status, fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.PingContext(ctx); err != nil {
		return status, fmt.Errorf("database is unreachable: %w", err)
	}
	version, dirty, err := readMigrationVersion(ctx, db)
	if err != nil {
		return status, err
	}
	status.Current, status.Dirty = version, dirty
	status.Migrated = !dirty && version == status.Expected
	return status, nil
}

// readMigrationVersion reads the version golang-migrate recorded. A missing
// schema_migrations table means the migrations have never run, which is
// reported as version 0 rather than as an error.
func readMigrationVersion(ctx context.Context, db *sql.DB) (uint, bool, error) {
	var version int64
	var dirty bool
	err := db.QueryRowContext(ctx, "SELECT version, dirty FROM schema_migrations LIMIT 1").Scan(&version, &dirty)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		if isUndefinedTable(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("read schema version: %w", err)
	}
	if version < 0 {
		// golang-migrate writes -1 for "no version applied yet".
		return 0, false, nil
	}
	return uint(version), dirty, nil
}

// isUndefinedTable reports whether err says the table does not exist. Both
// backends spell it differently: Postgres has a SQLSTATE for it, SQLite
// puts it in the message.
func isUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "42P01"
	}
	return strings.Contains(err.Error(), "no such table")
}

// expectedMigrationVersion returns the highest migration version the
// embedded migration set carries for a backend. The two sets are parallel
// and share their numbering, so the answer is the same for both.
func expectedMigrationVersion(scheme string) uint {
	dir := "migrations"
	if scheme == "sqlite" {
		dir = "migrations/sqlite"
	}
	entries, err := fs.ReadDir(migrationsFS, dir)
	if err != nil {
		return 0
	}
	var highest uint
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(prefix, 10, 32)
		if err != nil {
			continue
		}
		if uint(n) > highest {
			highest = uint(n)
		}
	}
	return highest
}
