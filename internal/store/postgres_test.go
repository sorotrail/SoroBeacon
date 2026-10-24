package store

import (
	"context"
	"encoding/json"
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

	_, err = st.pool.Exec(context.Background(),
		`TRUNCATE monitors, rules, channels, monitor_channels, alerts, delivery_attempts, contract_ingest_cursors, api_tokens,
		        escalation_policies, escalation_steps, escalation_step_channels, alert_escalations,
		        retention_gaps, event_archive
		 RESTART IDENTITY CASCADE;
		 UPDATE ingest_state SET last_ledger = 0, last_cursor = '' WHERE id = 1`)
	require.NoError(t, err)
	return st
}

// testStore returns a migrated Postgres store, skipping the test when
// TEST_DATABASE_URL is unset so `go test ./...` still works without a
// database. It returns the concrete *Postgres rather than conformanceStore
// because callers need the methods that are not on that interface, chiefly
// WithConfigCipher.
func testStore(t *testing.T) *Postgres {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres-backed test")
	}
	require.NoError(t, Migrate(url))
	st, err := NewPostgres(context.Background(), url, PoolSettings{})
	require.NoError(t, err)
	t.Cleanup(st.Close)
	require.NoError(t, st.resetConformance(context.Background()))
	return st
}

// Restore passes a backup's created_at through the Create methods. A non-zero
// CreatedAt is data and must not be replaced by now(); only the zero value may
// fall back to the column default.
func TestPostgresCreatePreservesExplicitCreatedAt(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	created := time.Date(2025, 11, 12, 13, 14, 15, 678901000, time.UTC)

	monitor := &Monitor{Name: "timestamp-monitor", Enabled: true, Network: "testnet", CreatedAt: created}
	require.NoError(t, st.CreateMonitor(ctx, monitor))
	gotMonitor, err := st.GetMonitor(ctx, monitor.ID)
	require.NoError(t, err)
	require.True(t, created.Equal(gotMonitor.CreatedAt), "monitor created_at: want %s, got %s", created, gotMonitor.CreatedAt)

	channel := &Channel{
		Name:      "timestamp-channel",
		Type:      "webhook",
		Config:    json.RawMessage(`{"url":"https://example.com/hook"}`),
		Enabled:   true,
		CreatedAt: created,
	}
	require.NoError(t, st.CreateChannel(ctx, channel))
	gotChannel, err := st.GetChannel(ctx, channel.ID)
	require.NoError(t, err)
	require.True(t, created.Equal(gotChannel.CreatedAt), "channel created_at: want %s, got %s", created, gotChannel.CreatedAt)

	search := &SavedSearch{Name: "timestamp-search", CreatedAt: created}
	require.NoError(t, st.CreateSavedSearch(ctx, search))
	gotSearch, err := st.GetSavedSearch(ctx, search.ID)
	require.NoError(t, err)
	require.True(t, created.Equal(gotSearch.CreatedAt), "saved search created_at: want %s, got %s", created, gotSearch.CreatedAt)

	template := &MonitorTemplate{
		Name:       "timestamp-template",
		Rules:      []MonitorTemplateRule{},
		ChannelIDs: []int64{},
		Parameters: []TemplateParameter{},
		CreatedAt:  created,
	}
	require.NoError(t, st.CreateMonitorTemplate(ctx, template))
	gotTemplate, err := st.GetMonitorTemplate(ctx, template.ID)
	require.NoError(t, err)
	require.True(t, created.Equal(gotTemplate.CreatedAt), "template created_at: want %s, got %s", created, gotTemplate.CreatedAt)
}

// rawChannelConfig reads the config column straight out of the table, so a
// test can assert on what actually landed rather than on what the store hands
// back after decryption. It fails the test instead of returning an error,
// which keeps the call sites to one line.
func rawChannelConfig(t *testing.T, st *Postgres, id int64) []byte {
	t.Helper()
	raw, err := st.rawChannelConfig(context.Background(), id)
	require.NoError(t, err)
	return raw
}

// The methods below satisfy conformanceStore. They are the only Postgres-aware
// code in the suite: everything else asserts on the Store interface, so the
// SQLite backend is held to the identical contract.

func (p *Postgres) resetConformance(ctx context.Context) error {
	_, err := p.pool.Exec(ctx,
		`TRUNCATE monitors, rules, channels, monitor_channels, alerts, delivery_attempts, contract_ingest_cursors, api_tokens,
		         saved_searches, monitor_templates, audit_log, pending_digests, maintenance_windows,
		         retention_gaps, event_archive RESTART IDENTITY CASCADE;
		 UPDATE ingest_state SET last_ledger = 0, last_cursor = '' WHERE id = 1`)
	return err
}

func (p *Postgres) setAlertCreatedAt(ctx context.Context, id int64, at time.Time) error {
	_, err := p.pool.Exec(ctx, `UPDATE alerts SET created_at = $1 WHERE id = $2`, at, id)
	return err
}

func (p *Postgres) setDeliveryAttemptAt(ctx context.Context, id int64, at time.Time) error {
	_, err := p.pool.Exec(ctx, `UPDATE delivery_attempts SET attempted_at = $1 WHERE id = $2`, at, id)
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

func (p *Postgres) setEventArchiveIngestedAt(ctx context.Context, eventID string, at time.Time) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE event_archive SET ingested_at = $1 WHERE event_id = $2`, at, eventID)
	return err
}
