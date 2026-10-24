package store

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file run without Postgres: both sides of the copy are
// SQLite databases in a temp dir. The Postgres-specific expectations —
// partitions, sequence resets, the alert_created_at gap fill — live in
// transfer_pg_test.go and run against TEST_DATABASE_URL.

// transferFixture is a populated source database plus the handles a test
// needs to compare against whatever the copy produced.
type transferFixture struct {
	src     *SQLite
	srcURL  string
	monitor int64
	rule    int64
	channel int64
	alerts  int
}

const (
	transferFixtureAlerts = 250
	transferFixtureBase   = "2026-01-02T10:00:00.000Z"
)

// seedTransferFixture opens (creating and migrating) a SQLite database and
// fills every interesting shape of row: JSON with HTML-significant
// characters, booleans, timestamps, NULLs, join rows, a default saved search
// and enough alerts to force several batches.
func seedTransferFixture(t *testing.T, url string, alertCount int) *transferFixture {
	t.Helper()
	require.NoError(t, Migrate(url))
	st, err := NewSQLite(context.Background(), url)
	require.NoError(t, err)
	t.Cleanup(st.Close)
	ctx := context.Background()

	m := &Monitor{Name: "alpha", ContractIDs: []string{"CA", "CB"}, Enabled: true, Priority: "normal"}
	require.NoError(t, st.CreateMonitor(ctx, m))

	c := &Channel{Name: "hook", Type: "webhook", Enabled: true,
		Config: json.RawMessage(`{"url":"https://example.com/x?a=1&t=2","note":"<escaped> & raw"}`)}
	require.NoError(t, st.CreateChannel(ctx, c))
	require.NoError(t, st.SetMonitorChannels(ctx, m.ID, []int64{c.ID}))

	r := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{"topic":"transfer"}`), Enabled: true, Severity: "critical"}
	require.NoError(t, st.CreateRule(ctx, r))

	require.NoError(t, st.CreateSavedSearch(ctx, &SavedSearch{Name: "all", Filter: SavedSearchFilter{Sort: "created_at_desc"}}))
	require.NoError(t, st.CreateSavedSearch(ctx, &SavedSearch{Name: "default-view", Filter: SavedSearchFilter{Sort: "created_at_desc"}, IsDefault: true}))
	require.NoError(t, st.CreateMonitorTemplate(ctx, &MonitorTemplate{Name: "token-watch", Description: "watch a token"}))

	base, err := parseSQLiteTime(transferFixtureBase)
	require.NoError(t, err)
	for i := 0; i < alertCount; i++ {
		_, err := st.db.ExecContext(ctx, `
			INSERT INTO alerts (monitor_id, rule_id, event_id, payload, created_at, ledger, severity)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			m.ID, r.ID, fmt.Sprintf("ev-%d", i),
			fmt.Sprintf(`{"contract_id":"CA","n":%d}`, i),
			sqliteTimeString(base.Add(time.Duration(i)*time.Minute)),
			5000+i, "critical")
		require.NoError(t, err)
	}
	var firstAlert int64
	require.NoError(t, st.db.QueryRowContext(ctx, `SELECT MIN(id) FROM alerts`).Scan(&firstAlert))
	for i := 0; i < 30; i++ {
		_, err := st.db.ExecContext(ctx, `
			INSERT INTO delivery_attempts (alert_id, channel_id, status, attempted_at)
			VALUES (?, ?, 'delivered', ?)`,
			firstAlert+int64(i%alertCount), c.ID, transferFixtureBase)
		require.NoError(t, err)
	}

	return &transferFixture{src: st, srcURL: url, monitor: m.ID, rule: r.ID, channel: c.ID, alerts: alertCount}
}

func countsByTable(t *testing.T, st Store) map[string]int64 {
	t.Helper()
	counts, err := TransferCounts(context.Background(), st)
	require.NoError(t, err)
	out := make(map[string]int64, len(counts))
	for _, c := range counts {
		out[c.Table] = c.Rows
	}
	return out
}

func TestTransferCopiesEveryTableSQLiteToSQLite(t *testing.T) {
	dir := t.TempDir()
	fx := seedTransferFixture(t, "sqlite://"+filepath.Join(dir, "src.db"), transferFixtureAlerts)

	require.NoError(t, Migrate("sqlite://"+filepath.Join(dir, "dst.db")))
	dst, err := NewSQLite(context.Background(), "sqlite://"+filepath.Join(dir, "dst.db"))
	require.NoError(t, err)
	t.Cleanup(dst.Close)

	var progressCalls int
	var lastProgress struct {
		table         string
		copied, total int64
	}
	report, err := TransferData(context.Background(), fx.src, dst, TransferOptions{
		BatchSize: 25,
		Progress: func(table string, copied, total int64) {
			progressCalls++
			lastProgress = struct {
				table         string
				copied, total int64
			}{table, copied, total}
		},
	})
	require.NoError(t, err)

	require.Len(t, report, len(transferTables))
	for _, rep := range report {
		assert.Equal(t, rep.Source, rep.Inserted+rep.Skipped, "table %s must account for every source row", rep.Table)
	}

	alertRep := report[len(report)-1]
	for _, rep := range report {
		if rep.Table == "alerts" {
			alertRep = rep
		}
	}
	assert.Equal(t, int64(transferFixtureAlerts), alertRep.Inserted)
	assert.GreaterOrEqual(t, progressCalls, transferFixtureAlerts/25, "every 25-row batch reports progress")
	assert.Equal(t, "alerts", lastProgress.table)
	assert.Equal(t, int64(transferFixtureAlerts), lastProgress.total)

	// Every table's row count matches the source exactly.
	srcCounts := countsByTable(t, fx.src)
	dstCounts := countsByTable(t, dst)
	for _, table := range transferTables {
		assert.Equal(t, srcCounts[table.name], dstCounts[table.name], "row count of %s", table.name)
	}

	ctx := context.Background()
	// IDs are preserved, so references resolve without remapping.
	var monitorID, ruleID, channelID int64
	require.NoError(t, dst.db.QueryRowContext(ctx,
		`SELECT m.id, r.id, c.id FROM monitors m
		   JOIN rules r ON r.monitor_id = m.id
		   JOIN monitor_channels mc ON mc.monitor_id = m.id
		   JOIN channels c ON c.id = mc.channel_id`).Scan(&monitorID, &ruleID, &channelID))
	assert.Equal(t, fx.monitor, monitorID)
	assert.Equal(t, fx.rule, ruleID)
	assert.Equal(t, fx.channel, channelID)

	var joined int
	require.NoError(t, dst.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM alerts a
		  JOIN monitors m ON m.id = a.monitor_id
		  JOIN rules r ON r.id = a.rule_id
		 WHERE a.payload LIKE '%"contract_id":"CA"%'`).Scan(&joined))
	assert.Equal(t, transferFixtureAlerts, joined)

	// Values survive byte for byte: the timestamp text, the JSON payload
	// with its HTML-significant characters, and a NULL stay as they were.
	var srcCreated, srcPayload string
	var srcRetracted any
	require.NoError(t, fx.src.db.QueryRowContext(ctx,
		`SELECT created_at, payload, retracted_at FROM alerts ORDER BY id LIMIT 1`).
		Scan(&srcCreated, &srcPayload, &srcRetracted))
	var dstCreated, dstPayload string
	var dstRetracted any
	require.NoError(t, dst.db.QueryRowContext(ctx,
		`SELECT created_at, payload, retracted_at FROM alerts ORDER BY id LIMIT 1`).
		Scan(&dstCreated, &dstPayload, &dstRetracted))
	assert.Equal(t, transferFixtureBase, srcCreated)
	assert.Equal(t, srcCreated, dstCreated)
	assert.Equal(t, srcPayload, dstPayload)
	assert.Contains(t, dstPayload, `<escaped> & raw`)
	assert.Nil(t, dstRetracted)

	// The delivery attempts carried their foreign keys across.
	var attempts int
	require.NoError(t, dst.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM delivery_attempts da JOIN alerts a ON a.id = da.alert_id`).Scan(&attempts))
	assert.Equal(t, 30, attempts)

	// A default saved search is still the default — the partial unique
	// index over (workspace_id) would have refused the copy otherwise.
	var defaults int
	require.NoError(t, dst.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM saved_searches WHERE is_default = 1`).Scan(&defaults))
	assert.Equal(t, 1, defaults)

	// The sequence was advanced: the next monitor created on the target
	// cannot collide with a copied ID.
	next := &Monitor{Name: "post-copy", Enabled: true, Priority: "normal"}
	require.NoError(t, dst.CreateMonitor(ctx, next))
	assert.Greater(t, next.ID, fx.monitor)
	var srcMaxMonitor int64
	require.NoError(t, fx.src.db.QueryRowContext(ctx, `SELECT MAX(id) FROM monitors`).Scan(&srcMaxMonitor))
	assert.Equal(t, srcMaxMonitor+1, next.ID)
}

func TestTransferResumesAfterInterrupt(t *testing.T) {
	dir := t.TempDir()
	fx := seedTransferFixture(t, "sqlite://"+filepath.Join(dir, "src.db"), 120)

	dstURL := "sqlite://"
	dstURL += filepath.Join(dir, "dst.db")
	require.NoError(t, Migrate(dstURL))
	dst, err := NewSQLite(context.Background(), dstURL)
	require.NoError(t, err)
	t.Cleanup(dst.Close)

	// Interrupt the run once 40 alerts have landed, as a crash or Ctrl-C
	// would.
	ctx, cancel := context.WithCancel(context.Background())
	_, err = TransferData(ctx, fx.src, dst, TransferOptions{
		BatchSize: 20,
		Progress: func(table string, copied, total int64) {
			if table == "alerts" && copied >= 40 {
				cancel()
			}
		},
	})
	require.ErrorIs(t, err, context.Canceled)

	var partial int
	require.NoError(t, dst.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM alerts`).Scan(&partial))
	require.GreaterOrEqual(t, partial, 40)
	require.Less(t, partial, 120)

	// Rerunning continues where the copy stopped: no duplicates, no
	// missing rows.
	report, err := TransferData(context.Background(), fx.src, dst, TransferOptions{BatchSize: 20})
	require.NoError(t, err)

	var final int
	require.NoError(t, dst.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM alerts`).Scan(&final))
	assert.Equal(t, 120, final)

	var distinctIDs int
	require.NoError(t, dst.db.QueryRowContext(context.Background(),
		`SELECT COUNT(DISTINCT id) FROM alerts`).Scan(&distinctIDs))
	assert.Equal(t, 120, distinctIDs, "a resumed run must not duplicate alert ids")

	for _, rep := range report {
		if rep.Table == "alerts" {
			assert.Equal(t, int64(120), rep.Source)
			assert.Equal(t, int64(120-partial), rep.Inserted, "the resumed run inserts only the missing rows")
			assert.Equal(t, int64(partial), rep.Skipped)
		}
	}
}

func TestTransferCountsReportEveryPlanTable(t *testing.T) {
	dir := t.TempDir()
	fx := seedTransferFixture(t, "sqlite://"+filepath.Join(dir, "src.db"), 5)

	counts, err := TransferCounts(context.Background(), fx.src)
	require.NoError(t, err)
	require.Len(t, counts, len(transferTables))
	byName := make(map[string]int64, len(counts))
	for _, c := range counts {
		byName[c.Table] = c.Rows
	}
	assert.Equal(t, int64(5), byName["alerts"])
	assert.Equal(t, int64(1), byName["monitors"])
	assert.Equal(t, int64(30), byName["delivery_attempts"])
	assert.Equal(t, int64(2), byName["saved_searches"])
	assert.Equal(t, int64(0), byName["audit_log"])
	assert.Equal(t, int64(0), byName["alert_dedup"], "sqlite has no alert_dedup table; its count is zero")
}

// TestTransferTablesMatchSchema is the guard that keeps transferTables
// complete: when a migration adds a table, this test names it instead of the
// copy silently missing it. The Postgres variant (transfer_pg_test.go) also
// covers the partition children.
func TestTransferTablesMatchSchema(t *testing.T) {
	url := "sqlite://" + filepath.Join(t.TempDir(), "schema.db")
	require.NoError(t, Migrate(url))
	st, err := NewSQLite(context.Background(), url)
	require.NoError(t, err)
	t.Cleanup(st.Close)

	rows, err := st.db.Query(`
		SELECT name FROM sqlite_master
		 WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		 ORDER BY name`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	allowed := map[string]bool{"schema_migrations": true}
	for _, tt := range transferTables {
		allowed[tt.name] = true
	}
	var unexpected []string
	present := make(map[string]bool)
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		present[name] = true
		if !allowed[name] {
			unexpected = append(unexpected, name)
		}
	}
	require.NoError(t, rows.Err())
	assert.Empty(t, unexpected,
		"tables exist that store migrate does not copy; add them to transferTables (and to transfer_pg_test.go's partition-aware check): %s",
		strings.Join(unexpected, ", "))

	// And the reverse direction: the plan must not name tables the schema
	// does not have.
	var missing []string
	for _, tt := range transferTables {
		if !present[tt.name] && !transferDerived[tt.name] {
			missing = append(missing, tt.name)
		}
	}
	assert.Empty(t, missing, "transferTables names tables the schema does not create: %s", strings.Join(missing, ", "))

	// alert_dedup is Postgres-only by design; SQLite must genuinely not
	// have it, or the dedup guard would have silently changed shape.
	assert.False(t, present["alert_dedup"], "sqlite grew an alert_dedup table; revisit transferDerived")
}

func TestCoerceForSQLite(t *testing.T) {
	at, err := parseSQLiteTime(transferFixtureBase)
	require.NoError(t, err)
	v, err := coerceForSQLite(at)
	require.NoError(t, err)
	assert.Equal(t, transferFixtureBase, v)

	v, err = coerceForSQLite(true)
	require.NoError(t, err)
	assert.Equal(t, int64(1), v)

	v, err = coerceForSQLite([]byte(`{"a":1}`))
	require.NoError(t, err)
	assert.Equal(t, `{"a":1}`, v)

	v, err = coerceForSQLite(int64(7))
	require.NoError(t, err)
	assert.Equal(t, int64(7), v)
}

func TestCoerceForPostgres(t *testing.T) {
	at, err := parseSQLiteTime(transferFixtureBase)
	require.NoError(t, err)

	// SQLite stores booleans as 0/1 integers.
	v, err := coerceForPostgres("boolean", int64(1))
	require.NoError(t, err)
	assert.Equal(t, true, v)
	v, err = coerceForPostgres("boolean", int64(0))
	require.NoError(t, err)
	assert.Equal(t, false, v)

	// SQLite stores timestamps as text in the canonical layout.
	v, err = coerceForPostgres("timestamp with time zone", transferFixtureBase)
	require.NoError(t, err)
	assert.Equal(t, at.UTC(), v)

	// jsonb arrives as a string from SQLite and stays valid JSON text.
	v, err = coerceForPostgres("jsonb", `{"a":1}`)
	require.NoError(t, err)
	assert.Equal(t, `{"a":1}`, v)

	// NULL passes through untouched.
	v, err = coerceForPostgres("timestamptz", nil)
	require.NoError(t, err)
	assert.Nil(t, v)

	// Garbage that is not a timestamp is an error, not a silent zero.
	_, err = coerceForPostgres("timestamp with time zone", "not-a-time")
	assert.Error(t, err)
}
