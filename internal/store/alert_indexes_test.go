package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// TestPostgresAlertListingUsesIndexes pins the plans of the alert listing
// paths to the indexes migration 0034 added for them (issue #116). It seeds
// enough rows that a sequential scan is measurably more expensive than the
// index descent, then EXPLAINs the statements the store actually runs —
// buildAlertQuery and buildDeliveryAttemptsQuery produce both the executed
// statement and the explained one, so the two cannot drift — and fails when a
// query stops using its index, which is what stops this work silently
// regressing later.
//
// Like every Postgres-backed test here it runs against TEST_DATABASE_URL and
// skips when it is unset, so `go test ./...` still works without a database.
func TestPostgresAlertListingUsesIndexes(t *testing.T) {
	st := testStore(t)
	// The seed is twenty thousand alerts and attempts; the next test in this
	// package resets before it starts, but leaving that cleanup implicit
	// would make this test's blast radius depend on test order.
	t.Cleanup(func() { _ = st.resetConformance(context.Background()) })
	ctx := context.Background()

	// Volume: one hundred monitors times two hundred alerts, every fourth
	// block of per-monitor rows in a second workspace. The monitor count is
	// what makes the monitor-filtered plan unambiguous: a listing that walks
	// alerts in time order and filters monitor_id row by row reads a hundred
	// rows for each one it returns, while the 0034 composite descends
	// directly. With too few monitors the two plans cost about the same and
	// the choice becomes a tie-break, which would pin nothing.
	const (
		monitorCount     = 100
		alertsPerMonitor = 200
		totalAlerts      = monitorCount * alertsPerMonitor
		otherWorkspace   = "idx-other"
		page             = 50
	)

	monitorIDs := make([]int64, monitorCount)
	ruleIDs := make([]int64, monitorCount)
	for i := range monitorIDs {
		m := &Monitor{Name: fmt.Sprintf("idx-review-%d", i), ContractIDs: []string{"C"}, Enabled: true}
		require.NoError(t, st.CreateMonitor(ctx, m))
		r := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
		require.NoError(t, st.CreateRule(ctx, r))
		monitorIDs[i], ruleIDs[i] = m.ID, r.ID
	}
	ch := &Channel{Name: "idx-review-hook", Type: "webhook", Config: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateChannel(ctx, ch))

	// The month start anchors every seeded timestamp inside the current
	// monthly partition (see 0011); the spread of totalAlerts seconds stays
	// within the month whatever day the test runs.
	var seedBase time.Time
	require.NoError(t, st.pool.QueryRow(ctx, `SELECT date_trunc('month', now())`).Scan(&seedBase))

	// One bulk insert rather than one CreateAlert per row: twenty thousand
	// round trips would dominate the test, and CreateAlert would exercise the
	// dedup guard and cooldown paths this test has no interest in. The
	// modulus spreads alerts across monitors evenly; every fourth block of
	// per-monitor rows belongs to another tenant, so each monitor's alerts
	// are mixed across both workspaces.
	_, err := st.pool.Exec(ctx, `
		INSERT INTO alerts (monitor_id, rule_id, event_id, payload, created_at, workspace_id)
		SELECT ($1::bigint[])[g % $3 + 1], ($2::bigint[])[g % $3 + 1], 'ev-idx-' || g,
		       json_build_object('contract_id', 'C' || (g % 50))::jsonb,
		       date_trunc('month', now()) + make_interval(secs => g),
		       CASE WHEN ((g - 1) / $3) % 4 = 0 THEN $4 ELSE 'default' END
		  FROM generate_series(1, $5) AS g`,
		monitorIDs, ruleIDs, monitorCount, otherWorkspace, totalAlerts)
	require.NoError(t, err)

	// One attempt per alert, a third of them failures, so the attempts table
	// has real distribution for the status-filtered plan below.
	_, err = st.pool.Exec(ctx, `
		INSERT INTO delivery_attempts (alert_id, alert_created_at, channel_id, status)
		SELECT a.id, a.created_at, $1,
		       CASE WHEN a.id % 3 = 0 THEN $2::text ELSE $3::text END
		  FROM alerts a`,
		ch.ID, DeliveryStatusFailed, DeliveryStatusSuccess)
	require.NoError(t, err)

	// The hot alert: newest of the first monitor, with three extra attempts
	// recorded through the store so the workspace check on that path runs
	// too. Its snippet is plain prose — snippets render on the dashboard and
	// a fixture must never resemble a credential.
	var hotID int64
	require.NoError(t, st.pool.QueryRow(ctx,
		`SELECT id FROM alerts WHERE monitor_id = $1 AND workspace_id = $2 ORDER BY created_at DESC, id DESC LIMIT 1`,
		monitorIDs[0], workspace.Default).Scan(&hotID))
	for _, status := range []string{DeliveryStatusFailed, DeliveryStatusSuccess, DeliveryStatusFailed} {
		require.NoError(t, st.RecordDeliveryAttempt(ctx, &DeliveryAttempt{
			AlertID:         hotID,
			ChannelID:       ch.ID,
			Status:          status,
			ResponseSnippet: "upstream rejected the request with response code 500",
		}))
	}

	// Without fresh statistics the planner guesses, and a guessed plan is not
	// the plan production runs.
	_, err = st.pool.Exec(ctx, `ANALYZE`)
	require.NoError(t, err)

	// Relations the planner knows hold rows: the partitions the seed landed
	// in, and the attempts table. An EXPLAIN estimates at least one row for
	// an empty monthly partition even after ANALYZE, so "the planner expects
	// rows" cannot be read off the plan; pg_class can tell which relations
	// actually have them.
	dataRelations := dataBearingRelations(t, st)

	t.Run("tenant first page is index-driven", func(t *testing.T) {
		// The unfiltered page may be served either by a per-partition
		// descent on the 0027 workspace index or by a Merge Append over the
		// created_at indexes with an Incremental Sort for the id tiebreak —
		// both early-terminate after LIMIT rows and neither scans the table.
		// Which one wins is the planner's cost question, so this asserts the
		// property that matters: no sequential scan over rows.
		q, args := buildAlertQuery(ctx, AlertFilter{Limit: page})
		plan := explainJSON(t, st, q, args...)
		assertNoSequentialScanOnData(t, plan, dataRelations)
	})

	t.Run("time-ranged page is index-driven", func(t *testing.T) {
		q, args := buildAlertQuery(ctx, AlertFilter{
			From:  seedBase.Add(2000 * time.Second),
			To:    seedBase.Add(8000 * time.Second),
			Limit: page,
		})
		plan := explainJSON(t, st, q, args...)
		assertNoSequentialScanOnData(t, plan, dataRelations)
	})

	t.Run("monitor-filtered page uses the workspace+monitor composite", func(t *testing.T) {
		// Pin the column list rather than the index name: an index created
		// on a partitioned parent lands on each partition under a generated
		// <partition>_<columns> name, so the parent's name never appears in
		// the plan. Dropping the composite makes this assertion fail.
		q, args := buildAlertQuery(ctx, AlertFilter{MonitorID: monitorIDs[0], Limit: page})
		plan := explainJSON(t, st, q, args...)
		assertScanThroughIndex(t, plan, "monitor_id_created_at_id_idx")
		assertNoSequentialScanOnData(t, plan, dataRelations)
	})

	t.Run("monitor-filtered keyset page still uses the composite", func(t *testing.T) {
		// A cursor page is the shape the dashboard spends most of its time
		// in, and its (created_at, id) comparison is the predicate most
		// likely to push a planner off the index.
		q, args := buildAlertQuery(ctx, AlertFilter{MonitorID: monitorIDs[0], Limit: page, AfterID: hotID})
		plan := explainJSON(t, st, q, args...)
		assertScanThroughIndex(t, plan, "monitor_id_created_at_id_idx")
		assertNoSequentialScanOnData(t, plan, dataRelations)
	})

	t.Run("delivery attempts page uses the alert order index", func(t *testing.T) {
		// delivery_attempts is a plain table, so this index keeps the exact
		// name the migration gave it.
		q, args := buildDeliveryAttemptsQuery(hotID, "", workspace.Default)
		plan := explainJSON(t, st, q, args...)
		assertScanThroughIndex(t, plan, "delivery_attempts_alert_idx")
		assertNoSequentialScanOnData(t, plan, dataRelations)
	})

	t.Run("status-filtered delivery attempts page uses the same index", func(t *testing.T) {
		q, args := buildDeliveryAttemptsQuery(hotID, DeliveryStatusFailed, workspace.Default)
		plan := explainJSON(t, st, q, args...)
		assertScanThroughIndex(t, plan, "delivery_attempts_alert_idx")
		assertNoSequentialScanOnData(t, plan, dataRelations)
	})

	t.Run("cursor pagination over the monitor filter has no duplicates and no gaps", func(t *testing.T) {
		// The full listing straight from SQL is the ground truth the paged
		// walk must reproduce exactly, in both directions: any duplicate
		// shows up as a mismatch at the repeat's position, any gap as a
		// shift of everything after it.
		var wantDesc, wantAsc []int64
		rows, err := st.pool.Query(ctx,
			`SELECT id FROM alerts WHERE workspace_id = $1 AND monitor_id = $2 ORDER BY created_at DESC, id DESC`,
			workspace.Default, monitorIDs[0])
		require.NoError(t, err)
		wantDesc, err = pgx.CollectRows(rows, pgx.RowTo[int64])
		require.NoError(t, err)
		rows, err = st.pool.Query(ctx,
			`SELECT id FROM alerts WHERE workspace_id = $1 AND monitor_id = $2 ORDER BY created_at ASC, id ASC`,
			workspace.Default, monitorIDs[0])
		require.NoError(t, err)
		wantAsc, err = pgx.CollectRows(rows, pgx.RowTo[int64])
		require.NoError(t, err)
		// One monitor owns alertsPerMonitor seeded alerts, a quarter of them
		// in the other workspace, so its tenant-scoped listing is three
		// quarters of that: exactly three pages of `page` rows. The guard
		// sits on that boundary rather than above it — three full pages and
		// the terminating empty request are the least the walk can do and
		// still prove it pages — and it stays strict enough to catch a seed
		// that shrank to a single page.
		require.GreaterOrEqual(t, len(wantDesc), 3*page, "the walk should page several times, or it proves nothing")

		pageAll := func(f AlertFilter) []int64 {
			t.Helper()
			var got []int64
			for after, pages := int64(0), 0; ; pages++ {
				require.Less(t, pages, totalAlerts, "paging with the cursor did not terminate")
				f.AfterID = after
				gotPage, err := st.ListAlerts(ctx, f)
				require.NoError(t, err)
				if len(gotPage) == 0 {
					return got
				}
				for _, a := range gotPage {
					got = append(got, a.ID)
				}
				after = gotPage[len(gotPage)-1].ID
			}
		}

		assert.Equal(t, wantDesc, pageAll(AlertFilter{MonitorID: monitorIDs[0], Limit: page}),
			"descending pages must reproduce the listing with no duplicate and no gap")
		assert.Equal(t, wantAsc, pageAll(AlertFilter{MonitorID: monitorIDs[0], Limit: page, Sort: "created_at_asc"}),
			"ascending pages must reproduce the listing with no duplicate and no gap")
	})
}

// explainJSON runs EXPLAIN (FORMAT JSON) over statement with args and returns
// the parsed plan tree.
func explainJSON(t *testing.T, st *Postgres, statement string, args ...any) explainNode {
	t.Helper()
	var raw []byte
	require.NoError(t, st.pool.QueryRow(context.Background(), `EXPLAIN (FORMAT JSON) `+statement, args...).Scan(&raw))
	var wrapped []struct {
		Plan explainNode `json:"Plan"`
	}
	require.NoError(t, json.Unmarshal(raw, &wrapped))
	require.Len(t, wrapped, 1, "EXPLAIN (FORMAT JSON) wraps a single plan in a one-element array")
	return wrapped[0].Plan
}

// explainNode is the subset of an EXPLAIN (FORMAT JSON) plan this test reads.
// The JSON keys keep Postgres's own spelling.
type explainNode struct {
	NodeType     string        `json:"Node Type"`
	RelationName string        `json:"Relation Name"`
	IndexName    string        `json:"Index Name"`
	PlanRows     float64       `json:"Plan Rows"`
	Plans        []explainNode `json:"Plans"`
}

// walkPlan calls fn for every node in the plan tree, parents before children.
func (n explainNode) walkPlan(fn func(explainNode)) {
	fn(n)
	for _, child := range n.Plans {
		child.walkPlan(fn)
	}
}

// assertScanThroughIndex fails unless some scan in the plan reads through an
// index whose name contains wantIndex. The match is on a distinctive substring
// rather than the full name because indexes on a partitioned parent reach the
// plan through their per-partition children, whose names Postgres generates as
// <partition>_<columns> — the parent's own name never appears.
func assertScanThroughIndex(t *testing.T, plan explainNode, wantIndex string) {
	t.Helper()
	var found bool
	plan.walkPlan(func(n explainNode) {
		if strings.Contains(n.IndexName, wantIndex) {
			found = true
		}
	})
	require.True(t, found, "no scan in the plan reads through index %s; plan:\n%s",
		wantIndex, mustJSON(t, plan))
}

// assertNoSequentialScanOnData fails when any node sequential-scans one of
// relations — the relations the seed filled. A sequential scan over an empty
// monthly partition is harmless and allowed; one over the rows the query
// should read through an index is the regression this test exists to catch.
func assertNoSequentialScanOnData(t *testing.T, plan explainNode, relations map[string]bool) {
	t.Helper()
	var sequential []string
	plan.walkPlan(func(n explainNode) {
		if strings.Contains(n.NodeType, "Seq Scan") && relations[n.RelationName] {
			sequential = append(sequential, fmt.Sprintf("%s on %s", n.NodeType, n.RelationName))
		}
	})
	assert.Empty(t, sequential, "sequential scans over rows that indexes should carry: %v\nplan:\n%s",
		sequential, mustJSON(t, plan))
}

// dataBearingRelations returns the names of the alerts partitions and the
// delivery_attempts table that ANALYZE reported non-empty rows for, which is
// the set assertNoSequentialScanOnData protects.
func dataBearingRelations(t *testing.T, st *Postgres) map[string]bool {
	t.Helper()
	rows, err := st.pool.Query(context.Background(), `
		SELECT c.relname
		  FROM pg_class c
		  JOIN pg_inherits i ON i.inhrelid = c.oid
		 WHERE i.inhparent = 'alerts'::regclass AND c.reltuples > 0
		UNION
		SELECT relname FROM pg_class
		 WHERE relname = 'delivery_attempts' AND reltuples > 0`)
	require.NoError(t, err)
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	require.NotEmpty(t, names, "the seed should have filled at least one alerts partition")
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return set
}

// mustJSON renders a plan for a failure message.
func mustJSON(t *testing.T, plan explainNode) string {
	t.Helper()
	dump, err := json.MarshalIndent(plan, "", "  ")
	require.NoError(t, err)
	return string(dump)
}
