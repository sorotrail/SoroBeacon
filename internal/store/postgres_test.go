package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestPostgresConformance runs the shared store suite against Postgres. It
// skips when TEST_DATABASE_URL is unset so `go test ./...` works without a
// database; CI's test-db job sets it (make test-db locally).
func TestPostgresConformance(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres store conformance tests")
	}
	runStoreConformance(t, newTestPostgres)
}

// newTestPostgres connects to TEST_DATABASE_URL, applies migrations and
// empties every table so each conformance subtest starts from a known state.
func newTestPostgres(t *testing.T) conformanceStore {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	require.NotEmpty(t, url, "TEST_DATABASE_URL must be set for the Postgres conformance suite")
	require.NoError(t, Migrate(url))

	st, err := NewPostgres(context.Background(), url, PoolSettings{})
	require.NoError(t, err)
	t.Cleanup(st.Close)
	require.NoError(t, st.resetConformance(context.Background()))
	return st
}

// The methods below satisfy conformanceStore. They are the only Postgres-aware
// code in the suite: everything else asserts on the Store interface, so the
// SQLite backend is held to the identical contract.

func (p *Postgres) resetConformance(ctx context.Context) error {
	_, err := p.pool.Exec(ctx,
		`TRUNCATE monitors, rules, channels, monitor_channels, alerts, delivery_attempts,
		         saved_searches, monitor_templates, audit_log, pending_digests RESTART IDENTITY CASCADE;
		 UPDATE ingest_state SET last_ledger = 0, last_cursor = '' WHERE id = 1`)
	return err
}

func (p *Postgres) setAlertCreatedAt(ctx context.Context, id int64, at time.Time) error {
	_, err := p.pool.Exec(ctx, `UPDATE alerts SET created_at = $1 WHERE id = $2`, at, id)
	return err
}

func (p *Postgres) backdateRuleLastAlert(ctx context.Context, id int64, d time.Duration) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE rules SET last_alert_at = now() - make_interval(secs => $2) WHERE id = $1`, id, d.Seconds())
	return err
}

func (p *Postgres) rawChannelConfig(ctx context.Context, id int64) ([]byte, error) {
	var raw []byte
	err := p.pool.QueryRow(ctx, `SELECT config FROM channels WHERE id = $1`, id).Scan(&raw)
	return raw, err
}

func (p *Postgres) setCipher(c ConfigCipher) { p.cipher = c }
