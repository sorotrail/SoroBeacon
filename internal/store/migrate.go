package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"os"

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

// MigrationState reports the current and latest embedded migration versions
// without applying migrations or creating a database. It is intended for
// deployment preflight checks, where inspecting the target must be read-only.
func MigrationState(ctx context.Context, databaseURL string) (current, latest uint, dirty bool, err error) {
	scheme, err := Scheme(databaseURL)
	if err != nil {
		return 0, 0, false, err
	}
	sourcePath := "migrations"
	driverName := "pgx"
	if scheme == "sqlite" {
		sourcePath = "migrations/sqlite"
		driverName = sqliteDriver
	} else if scheme != "postgres" && scheme != "postgresql" {
		return 0, 0, false, fmt.Errorf("unsupported DATABASE_URL scheme %q", scheme)
	}
	src, err := iofs.New(migrationsFS, sourcePath)
	if err != nil {
		return 0, 0, false, fmt.Errorf("load migrations: %w", err)
	}
	latest, err = src.First()
	if err != nil {
		return 0, 0, false, fmt.Errorf("find first migration: %w", err)
	}
	for {
		next, nextErr := src.Next(latest)
		if errors.Is(nextErr, os.ErrNotExist) {
			break
		}
		if nextErr != nil {
			return 0, 0, false, fmt.Errorf("find latest migration: %w", nextErr)
		}
		latest = next
	}

	dsn := databaseURL
	if scheme == "sqlite" {
		path, pathErr := sqliteFilePath(databaseURL)
		if pathErr != nil {
			return 0, latest, false, pathErr
		}
		query := url.Values{}
		query.Set("mode", "ro")
		query.Add("_pragma", "busy_timeout(5000)")
		query.Add("_pragma", "query_only(1)")
		dsn = "file:" + path + "?" + query.Encode()
	}
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return 0, latest, false, fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.PingContext(ctx); err != nil {
		return 0, latest, false, fmt.Errorf("ping database: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&current, &dirty); err != nil {
		return 0, latest, false, fmt.Errorf("read migration state: %w", err)
	}
	return current, latest, dirty, nil
}

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
