package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSQLiteConformance runs the shared store suite against SQLite. It needs
// no service container and no environment variable, so CI's plain build job
// exercises it; that is the point of the backend — a single file, no server.
func TestSQLiteConformance(t *testing.T) {
	runStoreConformance(t, newTestSQLite)
}

// newTestSQLite creates a fresh database file in the test's temp directory and
// applies the SQLite migrations.
func newTestSQLite(t *testing.T) conformanceStore {
	t.Helper()
	url := "sqlite://" + filepath.Join(t.TempDir(), "sorobeacon.db")
	require.NoError(t, Migrate(url))
	st, err := NewSQLite(context.Background(), url)
	require.NoError(t, err)
	t.Cleanup(st.Close)
	require.NoError(t, st.resetConformance(context.Background()))
	return st
}

// The methods below satisfy conformanceStore for SQLite. Timestamps go through
// the same fixed-format helpers the store itself uses so the tests set exactly
// what a real write would.
func (s *SQLite) resetConformance(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM delivery_attempts;
		 DELETE FROM audit_log;
		 DELETE FROM pending_digests;
		 DELETE FROM alerts;
		 DELETE FROM monitor_channels;
		 DELETE FROM rules;
		 DELETE FROM channels;
		 DELETE FROM monitors;
		 DELETE FROM saved_searches;
		 DELETE FROM monitor_templates;
		 UPDATE ingest_state SET last_ledger = 0, last_cursor = '' WHERE id = 1`); err != nil {
		return err
	}
	// sqlite_sequence only exists once a table with AUTOINCREMENT has been
	// written to; before that its absence is not an error.
	_, _ = s.db.ExecContext(ctx,
		`DELETE FROM sqlite_sequence WHERE name IN ('monitors','rules','channels','alerts','delivery_attempts','saved_searches','monitor_templates')`)
	return nil
}

func (s *SQLite) setAlertCreatedAt(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE alerts SET created_at = ? WHERE id = ?`, sqliteTimeString(at), id)
	return err
}

func (s *SQLite) backdateRuleLastAlert(ctx context.Context, id int64, d time.Duration) error {
	_, err := s.db.ExecContext(ctx, `UPDATE rules SET last_alert_at = ? WHERE id = ?`,
		sqliteTimeString(time.Now().Add(-d)), id)
	return err
}

func (s *SQLite) rawChannelConfig(ctx context.Context, id int64) ([]byte, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT config FROM channels WHERE id = ?`, id).Scan(&raw)
	return raw, err
}

func (s *SQLite) setCipher(c ConfigCipher) { s.cipher = c }

// --- SQLite-specific behaviour ---

func TestSQLiteFilePath(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{name: "absolute", url: "sqlite:///var/lib/sorobeacon/sorobeacon.db", want: "/var/lib/sorobeacon/sorobeacon.db"},
		{name: "relative host form", url: "sqlite://relative/path.db", want: "relative/path.db"},
		{name: "opaque", url: "sqlite:./data/sorobeacon.db", want: "./data/sorobeacon.db"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := sqliteFilePath(tt.url)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	_, err := sqliteFilePath("sqlite://")
	assert.ErrorContains(t, err, "file path")
}

// TestSQLiteDSNEnablesWALAndImmediateTransactions pins the concurrency
// contract: WAL journalling so readers are not blocked, foreign keys so
// cascades fire, and immediate transactions so a write lock is taken up front
// instead of at first write (the SQLite stand-in for SELECT ... FOR UPDATE).
func TestSQLiteDSNEnablesWALAndImmediateTransactions(t *testing.T) {
	dsn, err := sqliteDSN("sqlite:///tmp/sorobeacon.db")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(dsn, "file:/tmp/sorobeacon.db?"), dsn)
	assert.Contains(t, dsn, "_txlock=immediate")
	assert.Contains(t, dsn, "journal_mode")
	assert.Contains(t, dsn, "foreign_keys")
	assert.Contains(t, dsn, "busy_timeout")
}

func TestSQLitePragmasApplied(t *testing.T) {
	url := "sqlite://" + filepath.Join(t.TempDir(), "sorobeacon.db")
	require.NoError(t, Migrate(url))
	st, err := NewSQLite(context.Background(), url)
	require.NoError(t, err)
	t.Cleanup(st.Close)

	var journalMode string
	require.NoError(t, st.db.QueryRowContext(context.Background(), `PRAGMA journal_mode`).Scan(&journalMode))
	assert.Equal(t, "wal", strings.ToLower(journalMode))

	var foreignKeys int64
	require.NoError(t, st.db.QueryRowContext(context.Background(), `PRAGMA foreign_keys`).Scan(&foreignKeys))
	assert.Equal(t, int64(1), foreignKeys, "ON DELETE CASCADE relies on foreign_keys=1")

	var busyTimeout int64
	require.NoError(t, st.db.QueryRowContext(context.Background(), `PRAGMA busy_timeout`).Scan(&busyTimeout))
	assert.Equal(t, int64(5000), busyTimeout)
}

// TestSQLiteMigrateIdempotent proves the embedded migrations are re-runnable,
// which matters because Migrate runs on every startup.
func TestSQLiteMigrateIdempotent(t *testing.T) {
	url := "sqlite://" + filepath.Join(t.TempDir(), "sorobeacon.db")
	require.NoError(t, Migrate(url))
	require.NoError(t, Migrate(url))

	st, err := NewSQLite(context.Background(), url)
	require.NoError(t, err)
	t.Cleanup(st.Close)

	state, err := st.GetIngestState(context.Background())
	require.NoError(t, err)
	assert.Zero(t, state.LastLedger)
}

// TestSQLiteCreatesParentDirectory covers the quickstart promise: a
// DATABASE_URL pointing into a directory that does not exist yet must still
// start, so no manual mkdir is needed on a fresh host.
func TestSQLiteCreatesParentDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "data")
	url := "sqlite://" + filepath.Join(dir, "sorobeacon.db")
	require.NoError(t, Migrate(url))

	st, err := NewSQLite(context.Background(), url)
	require.NoError(t, err)
	t.Cleanup(st.Close)

	_, err = os.Stat(filepath.Join(dir, "sorobeacon.db"))
	require.NoError(t, err, "database file must exist at the configured path")
}

func TestBackendSelection(t *testing.T) {
	assert.Equal(t, "postgres", BackendName("postgres://user:pass@localhost:5432/db"))
	assert.Equal(t, "postgres", BackendName("postgresql://localhost/db"))
	assert.Equal(t, "sqlite", BackendName("sqlite:///var/lib/sorobeacon/sorobeacon.db"))
	assert.Equal(t, "unknown", BackendName("mysql://localhost/db"))

	scheme, err := Scheme("sqlite:///tmp/x.db")
	require.NoError(t, err)
	assert.Equal(t, "sqlite", scheme)

	_, err = Scheme("not a url")
	assert.ErrorContains(t, err, "DATABASE_URL")
}

// TestSQLiteNewRejectsUnsupportedScheme guards the constructor against a URL
// that config.Load would have caught but that a caller could pass directly.
func TestSQLiteNewRejectsUnsupportedScheme(t *testing.T) {
	_, err := New(context.Background(), "mysql://localhost/db", PoolSettings{}, nil)
	assert.ErrorContains(t, err, "unsupported")
}
