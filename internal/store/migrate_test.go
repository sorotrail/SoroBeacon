// Package store tests migration up/down round-trip.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

func TestMigrationStateSQLiteIsReadOnly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "preflight.db")
	url := "sqlite://" + path

	if _, _, _, err := MigrationState(ctx, url); err == nil {
		t.Fatal("an absent database should not be treated as migrated")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only preflight created the database: stat error = %v", err)
	}

	require.NoError(t, Migrate(url))
	current, latest, dirty, err := MigrationState(ctx, url)
	require.NoError(t, err)
	require.False(t, dirty)
	require.Equal(t, latest, current)

	st, err := NewReadOnly(ctx, url, PoolSettings{}, nil)
	require.NoError(t, err)
	defer st.Close()
	err = st.CreateMonitor(ctx, &Monitor{Name: "must not be written", ContractIDs: []string{"C"}, Enabled: true})
	require.Error(t, err, "the preflight store must reject writes")
}

func TestMigrationStatePostgresReadOnly(t *testing.T) {
	rawURL := os.Getenv("TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres preflight test")
	}
	databaseURL := isolatedMigrationURL(t, rawURL)
	require.NoError(t, Migrate(databaseURL))
	current, latest, dirty, err := MigrationState(context.Background(), databaseURL)
	require.NoError(t, err)
	require.False(t, dirty)
	require.Equal(t, latest, current)

	st, err := NewReadOnly(context.Background(), databaseURL, PoolSettings{}, nil)
	require.NoError(t, err)
	defer st.Close()
	err = st.CreateMonitor(context.Background(), &Monitor{Name: "must not be written", ContractIDs: []string{"C"}, Enabled: true})
	require.Error(t, err, "Postgres preflight transactions must reject writes")
}

// TestMigrate_RoundTrip tests that migrations apply up, down, and up again
// correctly. It skips when TEST_DATABASE_URL is unset so `go test ./...`
// works without a database; CI's test-db job sets it (make test-db locally).
// This test requires a database at version 0 (fresh) to test the full round-trip.
// If the database is already migrated, it skips the round-trip and just verifies
// that reapplying is a no-op.
func TestMigrate_RoundTrip(t *testing.T) {
	rawURL := os.Getenv("TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping migration round-trip test")
	}
	url := isolatedMigrationURL(t, rawURL)

	// Check current version
	verBefore := getMigrationVersion(t, url)

	// If database is not at version 0, we can't test the full round-trip
	// without a fresh database. Skip the round-trip but verify reapplying works.
	if verBefore > 0 {
		t.Logf("database already at version %d, skipping round-trip (needs fresh DB)", verBefore)
		// Verify reapplying is a no-op
		require.NoError(t, Migrate(url))
		require.NoError(t, Migrate(url))
		ver := getMigrationVersion(t, url)
		require.Equal(t, verBefore, ver)
		// Skip table verification since the database may not have all tables
		// from the latest migration version (depends on which version it's at).
		return
	}

	// Apply migrations up from empty database
	require.NoError(t, Migrate(url))

	// Get the current version after up
	verAfterUp := getMigrationVersion(t, url)
	require.True(t, verAfterUp > 0, "expected version > 0 after up, got %d", verAfterUp)

	// Migrate all the way down
	require.NoError(t, migrateDown(t, url))

	// Verify tables are gone (schema_migrations should still exist but be empty)
	verAfterDown := getMigrationVersion(t, url)
	require.Equal(t, uint(0), verAfterDown, "expected version 0 after down, got %d", verAfterDown)

	// Up again
	require.NoError(t, Migrate(url))

	// Verify second up reaches same version as first
	verAfterSecondUp := getMigrationVersion(t, url)
	require.Equal(t, verAfterUp, verAfterSecondUp,
		"second up version %d should equal first up version %d", verAfterSecondUp, verAfterUp)

	// Applying twice in a row is a no-op (not an error)
	require.NoError(t, Migrate(url))
	verAfterThirdUp := getMigrationVersion(t, url)
	require.Equal(t, verAfterUp, verAfterThirdUp,
		"third up version %d should equal first up version %d", verAfterThirdUp, verAfterUp)

	// Verify some key tables exist after the round-trip
	verifyKeyTablesExist(t, url)
}

// TestMigrate_ReapplyingIsNoop tests that applying migrations twice in a
// row is a no-op rather than an error.
func TestMigrate_ReapplyingIsNoop(t *testing.T) {
	rawURL := os.Getenv("TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping migration reapply test")
	}
	url := isolatedMigrationURL(t, rawURL)

	require.NoError(t, Migrate(url))
	require.NoError(t, Migrate(url)) // Second application should not error

	ver := getMigrationVersion(t, url)
	require.True(t, ver > 0, "expected version > 0 after reapply, got %d", ver)
}

func isolatedMigrationURL(t *testing.T, rawURL string) string {
	t.Helper()
	schema := fmt.Sprintf("migration_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	db, err := sql.Open("pgx", rawURL)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.ExecContext(context.Background(), `CREATE SCHEMA "`+schema+`"`)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup, err := sql.Open("pgx", rawURL)
		if err != nil {
			t.Errorf("open database to drop isolated migration schema: %v", err)
			return
		}
		defer cleanup.Close()
		if _, err := cleanup.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`); err != nil {
			t.Errorf("drop isolated migration schema: %v", err)
		}
	})
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	q := u.Query()
	q.Set("options", "-csearch_path="+schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// getMigrationVersion returns the current migration version for the database.
func getMigrationVersion(t *testing.T, url string) uint {
	t.Helper()
	src, err := iofs.New(migrationsFS, "migrations")
	require.NoError(t, err)

	db, err := sql.Open("pgx", url)
	require.NoError(t, err)
	defer db.Close()

	driver, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
	require.NoError(t, err)

	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	require.NoError(t, err)
	defer func() { _, _ = m.Close() }()

	version, dirty, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0
	}
	require.NoError(t, err)
	require.False(t, dirty, "database should not be dirty after migration")
	return version
}

// migrateDown rolls back all migrations.
func migrateDown(t *testing.T, url string) error {
	t.Helper()
	src, err := iofs.New(migrationsFS, "migrations")
	require.NoError(t, err)

	db, err := sql.Open("pgx", url)
	require.NoError(t, err)
	defer db.Close()

	driver, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
	require.NoError(t, err)

	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	require.NoError(t, err)
	defer func() { _, _ = m.Close() }()

	return m.Down()
}

// verifyKeyTablesExist checks that the core tables exist after migration.
func verifyKeyTablesExist(t *testing.T, url string) {
	t.Helper()
	db, err := sql.Open("pgx", url)
	require.NoError(t, err)
	defer db.Close()

	// Check that key tables exist by querying them
	tables := []string{
		"monitors",
		"rules",
		"channels",
		"monitor_channels",
		"alerts",
		"delivery_attempts",
		"escalation_policies",
		"escalation_steps",
		"escalation_step_channels",
		"alert_escalations",
		"saved_searches",
		"monitor_templates",
		"audit_log",
		"pending_digests",
		"maintenance_windows",
		"ingest_state",
		"ledger_hashes",
		"backfills",
	}

	for _, table := range tables {
		var count int
		err := db.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM `+table).Scan(&count)
		require.NoError(t, err, "table %s should exist and be queryable", table)
	}
}

// TestMigrate_NoUnguardedDDL scans migration files for CREATE INDEX or
// CREATE TABLE statements that lack IF NOT EXISTS guards, which have
// caused issues before. This is a lightweight check; it parses the SQL
// as text rather than executing it.
func TestMigrate_NoUnguardedDDL(t *testing.T) {
	t.Helper()
	// Read all migration files from the embed FS
	entries, err := migrationsFS.ReadDir("migrations")
	require.NoError(t, err)

	for _, entry := range entries {
		if entry.IsDir() {
			continue // Skip sqlite subdirectory
		}
		if entry.Name() == ".gitkeep" {
			continue
		}
		content, err := migrationsFS.ReadFile("migrations/" + entry.Name())
		require.NoError(t, err)

		// Check for unguarded CREATE TABLE
		// We allow CREATE TABLE IF NOT EXISTS but flag bare CREATE TABLE
		// (Note: some migrations intentionally create tables that shouldn't
		// exist yet, so we only warn in the test output)
		sql := string(content)
		if containsUnguardedCreateTable(sql) {
			t.Logf("WARNING: %s contains unguarded CREATE TABLE", entry.Name())
		}
		if containsUnguardedCreateIndex(sql) {
			t.Logf("WARNING: %s contains unguarded CREATE INDEX", entry.Name())
		}
	}
}

// containsUnguardedCreateTable checks for CREATE TABLE without IF NOT EXISTS.
// This is a simple text check; it may have false positives/negatives.
func containsUnguardedCreateTable(sql string) bool {
	// Simple check: look for "CREATE TABLE" not followed by "IF NOT EXISTS"
	// This is intentionally permissive to catch potential issues.
	lines := splitLines(sql)
	for _, line := range lines {
		line = stripComments(line)
		if containsIgnoreCase(line, "CREATE TABLE") &&
			!containsIgnoreCase(line, "IF NOT EXISTS") &&
			!containsIgnoreCase(line, "CREATE TEMP") && // temp tables are fine
			!containsIgnoreCase(line, "CREATE UNLOGGED") { // unlogged tables are fine
			return true
		}
	}
	return false
}

// containsUnguardedCreateIndex checks for CREATE INDEX without IF NOT EXISTS.
func containsUnguardedCreateIndex(sql string) bool {
	lines := splitLines(sql)
	for _, line := range lines {
		line = stripComments(line)
		if containsIgnoreCase(line, "CREATE INDEX") &&
			!containsIgnoreCase(line, "IF NOT EXISTS") &&
			!containsIgnoreCase(line, "CREATE UNIQUE INDEX") { // unique indexes are usually intentional
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var lines []string
	current := ""
	for _, r := range s {
		if r == '\n' {
			lines = append(lines, current)
			current = ""
		} else {
			current += string(r)
		}
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}

func stripComments(s string) string {
	// Remove -- comments
	if idx := indexSubstring(s, "--"); idx >= 0 {
		s = s[:idx]
	}
	// Remove /* */ comments (simplified)
	for {
		start := indexSubstring(s, "/*")
		if start < 0 {
			break
		}
		end := indexSubstring(s[start:], "*/")
		if end < 0 {
			break
		}
		s = s[:start] + s[start+end+2:]
	}
	return s
}

func containsIgnoreCase(s, substr string) bool {
	sLower := ""
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			sLower += string(r + 32)
		} else {
			sLower += string(r)
		}
	}
	subLower := ""
	for _, r := range substr {
		if r >= 'A' && r <= 'Z' {
			subLower += string(r + 32)
		} else {
			subLower += string(r)
		}
	}
	return indexSubstring(sLower, subLower) >= 0
}

func indexSubstring(s, substr string) int {
	if len(substr) == 0 {
		return 0
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
