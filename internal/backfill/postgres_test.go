package backfill

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/poller"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// backfillSchema is the dedicated schema these tests live in. It must not be
// "public": the store package's integration tests TRUNCATE the public tables,
// and `go test ./...` runs both package test binaries in parallel against the
// same Postgres.
const backfillSchema = "backfill_test"

// isolatedURL gives the backfill tests their own, freshly recreated schema so
// they never see or destroy the store suite's fixtures. Tests skip when
// TEST_DATABASE_URL is unset, so `go test ./...` works without a database while
// CI (make test-db) runs them against the Postgres service container.
func isolatedURL(t *testing.T, ctx context.Context, raw string) string {
	t.Helper()
	admin, err := pgxpool.New(ctx, raw)
	require.NoError(t, err)
	defer admin.Close()
	// Fresh schema per test: no truncation or residual rows to reason about.
	_, err = admin.Exec(ctx, "DROP SCHEMA IF EXISTS "+backfillSchema+" CASCADE")
	require.NoError(t, err)
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+backfillSchema)
	require.NoError(t, err)

	u, err := url.Parse(raw)
	require.NoError(t, err)
	q := u.Query()
	q.Set("options", "-csearch_path="+backfillSchema)
	u.RawQuery = q.Encode()
	return u.String()
}

// newTestPostgres connects to TEST_DATABASE_URL, in an isolated schema, and
// migrates it.
func newTestPostgres(t *testing.T) (*store.Postgres, context.Context) {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping backfill integration tests")
	}
	ctx := context.Background()
	url := isolatedURL(t, ctx, raw)
	require.NoError(t, store.Migrate(url))
	st, err := store.NewPostgres(ctx, url, store.PoolSettings{})
	require.NoError(t, err)
	t.Cleanup(st.Close)
	return st, ctx
}

// seedPostgresMonitor creates a monitor watching contractA with a "transfer"
// event_emitted rule.
func seedPostgresMonitor(t *testing.T, st *store.Postgres, ctx context.Context) *store.Monitor {
	t.Helper()
	m := &store.Monitor{Name: "m", ContractIDs: []string{contractA}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	r := &store.Rule{MonitorID: m.ID, Type: rules.TypeEventEmitted,
		Params: json.RawMessage(`{"event_name": "transfer"}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, r))
	return m
}

func countAlerts(t *testing.T, st *store.Postgres, ctx context.Context, monitorID int64) []store.Alert {
	t.Helper()
	alerts, err := st.ListAlerts(ctx, store.AlertFilter{MonitorID: monitorID, Limit: 100})
	require.NoError(t, err)
	return alerts
}

// TestBackfillResumesAndDefaultsToNoDelivery is the integration contract from
// the issue: against Postgres, an interrupted backfill resumes from its
// persisted cursor and a default run never reaches the dispatcher.
func TestBackfillResumesAndDefaultsToNoDelivery(t *testing.T) {
	st, ctx := newTestPostgres(t)
	m := seedPostgresMonitor(t, st, ctx)

	pages := map[string]poller.FetchPage{
		"":   {Events: []*stellar.DecodedEvent{transfer("ev-1", 90), transfer("ev-2", 91)}, LatestLedger: 100, NextCursor: "c1"},
		"c1": {Events: []*stellar.DecodedEvent{transfer("ev-3", 92), transfer("ev-4", 93)}, LatestLedger: 100, NextCursor: "c2"},
		"c2": {Events: []*stellar.DecodedEvent{transfer("ev-5", 94)}, LatestLedger: 100},
	}

	registry := rules.NewRegistry()
	d := &fakeDispatcher{}
	ing := poller.NewIngestor(st, registry, d, slog.New(slog.DiscardHandler))
	opts := Options{MonitorID: m.ID, FromLedger: 1, ToLedger: 100, Rate: time.Millisecond}

	// First run is interrupted after the first page.
	interrupted := &fakeSource{tip: 100, pages: pages, failOn: 2}
	_, err := New(interrupted, st, registry, ing, discardLogger()).Run(ctx, opts)
	require.Error(t, err)

	prog, err := st.GetBackfill(ctx, m.ID)
	require.NoError(t, err)
	assert.False(t, prog.Complete)
	assert.Equal(t, "c1", prog.Cursor, "the last completed page's cursor is persisted for resume")

	alerts := countAlerts(t, st, ctx, m.ID)
	require.Len(t, alerts, 2, "the first page's matches are already recorded")
	for _, a := range alerts {
		assert.True(t, a.Backfilled, "backfilled alerts must be marked")
	}
	assert.Empty(t, d.dispatched, "no channel delivery by default")

	// Second run resumes and finishes the range.
	resumed := &fakeSource{tip: 100, pages: pages}
	res, err := New(resumed, st, registry, ing, discardLogger()).Run(ctx, opts)
	require.NoError(t, err)
	assert.True(t, res.Resumed)
	require.NotEmpty(t, resumed.cursors)
	assert.Equal(t, "c1", resumed.cursors[0], "the run resumes at the persisted cursor")

	alerts = countAlerts(t, st, ctx, m.ID)
	assert.Len(t, alerts, 5, "one alert per distinct event, with no duplicates on resume")
	for _, a := range alerts {
		assert.True(t, a.Backfilled)
	}
	assert.Empty(t, d.dispatched, "completing the replay still must not deliver")

	prog, err = st.GetBackfill(ctx, m.ID)
	require.NoError(t, err)
	assert.True(t, prog.Complete)
}

// TestBackfillDeliversWhenFlagged is the explicit-delivery half: with the flag
// set, every created alert is handed to the dispatcher, and the stored alerts
// remain marked as backfilled.
func TestBackfillDeliversWhenFlagged(t *testing.T) {
	st, ctx := newTestPostgres(t)
	m := seedPostgresMonitor(t, st, ctx)

	src := &fakeSource{tip: 100, pages: map[string]poller.FetchPage{
		"": {Events: []*stellar.DecodedEvent{
			transfer("ev-1", 90), transfer("ev-2", 91), transfer("ev-3", 92),
		}, LatestLedger: 100},
	}}

	registry := rules.NewRegistry()
	d := &fakeDispatcher{}
	ing := poller.NewIngestor(st, registry, d, discardLogger())
	res, err := New(src, st, registry, ing, discardLogger()).Run(ctx, Options{
		MonitorID: m.ID, FromLedger: 1, ToLedger: 100, Deliver: true, Rate: time.Millisecond,
	})
	require.NoError(t, err)

	assert.Equal(t, 3, res.Dispatched)
	assert.Len(t, d.dispatched, 3)
	alerts := countAlerts(t, st, ctx, m.ID)
	require.Len(t, alerts, 3)
	for _, a := range alerts {
		assert.True(t, a.Backfilled)
	}
}
