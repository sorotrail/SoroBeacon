package store

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	sqlitemigrate "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for migrations
)

// Both migration sets are embedded. The SQLite set lives in a subdirectory
// because Postgres DDL (JSONB, TIMESTAMPTZ, BIGSERIAL) does not run unmodified
// on SQLite; keeping them parallel lets each backend evolve independently.
//
//go:embed migrations/*.sql migrations/sqlite/*.sql
var migrationsFS embed.FS

// Migrate applies all pending migrations for the backend selected by the
// DATABASE_URL scheme. It is embedded in the binary and safe to call on every
// start: an up-to-date database is a no-op.
func Migrate(databaseURL string) error {
	scheme, err := Scheme(databaseURL)
	if err != nil {
		return err
	}
	switch scheme {
	case "postgres", "postgresql":
		return migratePostgres(databaseURL)
	case "sqlite":
		return migrateSQLite(databaseURL)
	default:
		return fmt.Errorf("unsupported DATABASE_URL scheme %q (supported schemes: postgres, postgresql, sqlite)", scheme)
	}
}

func migratePostgres(databaseURL string) error {
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}

	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open db for migrations: %w", err)
	}
	driver, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("init migration driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("init migrations: %w", err)
	}
	defer func() { _, _ = m.Close() }()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

func migrateSQLite(databaseURL string) error {
	src, err := iofs.New(migrationsFS, "migrations/sqlite")
	if err != nil {
		return fmt.Errorf("load sqlite migrations: %w", err)
	}

	// Create the directory first: Migrate runs before NewSQLite, so a fresh
	// host would otherwise fail on the migration open, not the store open.
	if err := ensureSQLiteDir(databaseURL); err != nil {
		return err
	}
	dsn, err := sqliteDSN(databaseURL)
	if err != nil {
		return err
	}
	db, err := sql.Open(sqliteDriver, dsn)
	if err != nil {
		return fmt.Errorf("open sqlite for migrations: %w", err)
	}
	driver, err := sqlitemigrate.WithInstance(db, &sqlitemigrate.Config{})
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("init sqlite migration driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, "sqlite", driver)
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("init sqlite migrations: %w", err)
	}
	defer func() { _, _ = m.Close() }()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply sqlite migrations: %w", err)
	}
	return nil
}
