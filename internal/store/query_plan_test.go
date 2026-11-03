package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// How to update query-plan expectations when a query or index intentionally changes:
//
//  1. If a migration adds, renames, or drops an index on a load-bearing table
//     (alerts, delivery_attempts, rules, monitors), update loadBearingMigrationIndexes
//     below so TestMigrationDeclaredIndexesCatalogue and
//     TestMigrationIndexesPresentInPostgres reflect the new index name and table.
//  2. If a read query in internal/store/postgres.go changes its predicates, sort
//     order, or backing index, update the corresponding queryPlanExpectation entry
//     in buildQueryPlanExpectations:
//       - wantIndexSuffixes lists the normalized index names (for partitioned alerts
//         indexes, the per-partition suffix after "alerts_YYYY_MM_"; for unpartitioned
//         tables, the full index name) that an Index Scan, Index Only Scan, or
//         Bitmap Index Scan must use.
//       - mustTouchPartitions and mustPrunePartitions pin partition pruning on
//         time-bounded alerts queries across the three seeded monthly partitions.
//  3. Run `go test ./internal/store/... -run "TestMigration|TestEvaluateQueryPlan|TestQueryPlan"`
//     (with TEST_DATABASE_URL set for the Postgres EXPLAIN gate) to verify both the
//     positive plan assertions and the transactional index-drop regression checks.

// queryPlanNode represents a node in PostgreSQL's EXPLAIN (FORMAT JSON) output.
type queryPlanNode struct {
	NodeType           string          `json:"Node Type"`
	ParentRelationship string          `json:"Parent Relationship"`
	SubplanName        string          `json:"Subplan Name"`
	RelationName       string          `json:"Relation Name"`
	IndexName          string          `json:"Index Name"`
	IndexCond          string          `json:"Index Cond"`
	Filter             string          `json:"Filter"`
	PlanRows           float64         `json:"Plan Rows"`
	TotalCost          float64         `json:"Total Cost"`
	Plans              []queryPlanNode `json:"Plans"`
}

// walkAll visits every node in the plan tree, including InitPlan subqueries.
func (n queryPlanNode) walkAll(fn func(queryPlanNode)) {
	fn(n)
	for _, child := range n.Plans {
		child.walkAll(fn)
	}
}

// walkMainPlan visits every node in the main query plan tree while skipping
// InitPlan subqueries (such as the single-row primary-key lookup emitted by
// keyset cursor pagination `(SELECT created_at, id FROM alerts WHERE id = $N)`).
func (n queryPlanNode) walkMainPlan(fn func(queryPlanNode)) {
	if n.ParentRelationship == "InitPlan" {
		return
	}
	fn(n)
	for _, child := range n.Plans {
		child.walkMainPlan(fn)
	}
}

var partitionIndexPrefixRe = regexp.MustCompile(`^alerts_(?:\d{4}_\d{2}|default)_(.+)$`)

// normalizePartitionIndexName strips the generated per-partition prefix
// ("alerts_YYYY_MM_" or "alerts_default_") from child partition index names so
// expectations can match the exact column-suffix signature without colliding
// with wider composite indexes (for example distinguishing
// "rule_id_created_at_id_idx" from "monitor_id_rule_id_created_at_id_idx").
func normalizePartitionIndexName(indexName string) string {
	if m := partitionIndexPrefixRe.FindStringSubmatch(indexName); m != nil {
		return m[1]
	}
	return indexName
}

// isIndexAccessNode reports whether nodeType is one of PostgreSQL's indexed
// scan operators.
func isIndexAccessNode(nodeType string) bool {
	switch nodeType {
	case "Index Scan", "Index Only Scan", "Bitmap Index Scan":
		return true
	default:
		return false
	}
}

// queryPlanExpectation defines the expected access path and partition-pruning
// contract for a single load-bearing store query.
type queryPlanExpectation struct {
	name                string
	category            string
	wantIndexSuffixes   []string
	mustTouchPartitions []string
	mustPrunePartitions []string
	build               func() (string, []any)
}

// evaluateQueryPlan checks a parsed EXPLAIN (FORMAT JSON) tree against exp.
// It returns a descriptive error naming exp.name and exp.category whenever:
//   - any data-bearing relation is read via a Seq Scan,
//   - no Index Scan / Index Only Scan / Bitmap Index Scan uses an expected index,
//   - an expected partition is missing from the main plan, or
//   - a partition that should have been pruned is scanned in the main plan.
func evaluateQueryPlan(plan queryPlanNode, exp queryPlanExpectation, dataRelations map[string]bool) error {
	var seqScans []string
	plan.walkAll(func(n queryPlanNode) {
		if strings.Contains(n.NodeType, "Seq Scan") && dataRelations[n.RelationName] {
			seqScans = append(seqScans, fmt.Sprintf("%s on %s", n.NodeType, n.RelationName))
		}
	})
	if len(seqScans) > 0 {
		return fmt.Errorf("query %q (%s): unexpected sequential scan on data-bearing relation(s) %v; expected index scan via %v",
			exp.name, exp.category, seqScans, exp.wantIndexSuffixes)
	}

	var matchedIndex bool
	var observedIndexScans []string
	plan.walkAll(func(n queryPlanNode) {
		if !isIndexAccessNode(n.NodeType) || n.IndexName == "" {
			return
		}
		norm := normalizePartitionIndexName(n.IndexName)
		observedIndexScans = append(observedIndexScans, fmt.Sprintf("%s(%s)", n.NodeType, n.IndexName))
		for _, want := range exp.wantIndexSuffixes {
			if norm == want || n.IndexName == want {
				matchedIndex = true
				return
			}
		}
	})
	if !matchedIndex {
		return fmt.Errorf("query %q (%s): no index scan matched expected index %v (observed index scans: %v)",
			exp.name, exp.category, exp.wantIndexSuffixes, observedIndexScans)
	}

	if len(exp.mustTouchPartitions) > 0 || len(exp.mustPrunePartitions) > 0 {
		touched := make(map[string]bool)
		plan.walkMainPlan(func(n queryPlanNode) {
			if n.RelationName != "" {
				touched[n.RelationName] = true
			}
			for _, part := range exp.mustTouchPartitions {
				if strings.HasPrefix(n.IndexName, part+"_") {
					touched[part] = true
				}
			}
			for _, part := range exp.mustPrunePartitions {
				if strings.HasPrefix(n.IndexName, part+"_") {
					touched[part] = true
				}
			}
		})

		for _, wantPart := range exp.mustTouchPartitions {
			if !touched[wantPart] {
				return fmt.Errorf("query %q (%s): expected active partition %q to be scanned in main plan, but it was not touched",
					exp.name, exp.category, wantPart)
			}
		}
		for _, prunedPart := range exp.mustPrunePartitions {
			if touched[prunedPart] {
				return fmt.Errorf("query %q (%s): expected out-of-window partition %q to be pruned, but it was scanned in main plan",
					exp.name, exp.category, prunedPart)
			}
		}
	}

	return nil
}

// migrationIndexSpec describes an index declared in internal/store/migrations/*.up.sql.
type migrationIndexSpec struct {
	name          string
	table         string
	partitioned   bool
	planSignature string
}

// loadBearingMigrationIndexes lists the load-bearing indexes declared across
// internal/store/migrations/ that back the store's read queries.
var loadBearingMigrationIndexes = []migrationIndexSpec{
	{
		name:          "alerts_workspace_created_idx",
		table:         "alerts",
		partitioned:   true,
		planSignature: "workspace_id_created_at_id_idx",
	},
	{
		name:          "alerts_monitor_created_idx",
		table:         "alerts",
		partitioned:   true,
		planSignature: "monitor_id_created_at_id_idx",
	},
	{
		name:          "alerts_rule_created_idx",
		table:         "alerts",
		partitioned:   true,
		planSignature: "rule_id_created_at_id_idx",
	},
	{
		name:          "alerts_monitor_rule_created_idx",
		table:         "alerts",
		partitioned:   true,
		planSignature: "monitor_id_rule_id_created_at_id_idx",
	},
	{
		name:          "alerts_contract_created_idx",
		table:         "alerts",
		partitioned:   true,
		planSignature: "expr_created_at_id_idx",
	},
	{
		name:          "alerts_network_created_idx",
		table:         "alerts",
		partitioned:   true,
		planSignature: "network_created_at_id_idx",
	},
	{
		name:          "alerts_created_at_idx",
		table:         "alerts",
		partitioned:   true,
		planSignature: "created_at_idx",
	},
	{
		name:          "delivery_attempts_alert_idx",
		table:         "delivery_attempts",
		partitioned:   false,
		planSignature: "delivery_attempts_alert_idx",
	},
	{
		name:          "delivery_attempts_channel_attempted_idx",
		table:         "delivery_attempts",
		partitioned:   false,
		planSignature: "delivery_attempts_channel_attempted_idx",
	},
	{
		name:          "rules_monitor_id_idx",
		table:         "rules",
		partitioned:   false,
		planSignature: "rules_monitor_id_idx",
	},
}

var (
	createIndexRe = regexp.MustCompile(`(?is)CREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-zA-Z0-9_]+)\s+ON\s+([a-zA-Z0-9_]+)`)
	renameIndexRe = regexp.MustCompile(`(?is)ALTER\s+INDEX\s+(?:IF\s+EXISTS\s+)?([a-zA-Z0-9_]+)\s+RENAME\s+TO\s+([a-zA-Z0-9_]+)`)
	renameTableRe = regexp.MustCompile(`(?is)ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?([a-zA-Z0-9_]+)\s+RENAME\s+TO\s+([a-zA-Z0-9_]+)`)
	dropIndexRe   = regexp.MustCompile(`(?is)DROP\s+INDEX\s+(?:IF\s+EXISTS\s+)?([a-zA-Z0-9_]+)`)
	dropTableRe   = regexp.MustCompile(`(?is)DROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?([a-zA-Z0-9_]+)`)
)

// parseActiveMigrationIndexes replays internal/store/migrations/*.up.sql in
// version order and returns the map of active index_name -> table_name.
func parseActiveMigrationIndexes(t *testing.T) map[string]string {
	t.Helper()
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	require.NoError(t, err)

	var upFiles []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			upFiles = append(upFiles, e.Name())
		}
	}
	sort.Strings(upFiles)
	require.NotEmpty(t, upFiles, "expected embedded .up.sql migrations")

	active := make(map[string]string)
	for _, name := range upFiles {
		raw, err := fs.ReadFile(migrationsFS, "migrations/"+name)
		require.NoError(t, err)
		sqlText := stripSQLLineComments(string(raw))
		for _, stmt := range strings.Split(sqlText, ";") {
			stmt = strings.TrimSpace(stmt)
			if stmt == "" {
				continue
			}
			if m := renameTableRe.FindStringSubmatch(stmt); m != nil {
				oldTable, newTable := m[1], m[2]
				for idx, tbl := range active {
					if tbl == oldTable {
						active[idx] = newTable
					}
				}
				continue
			}
			if m := renameIndexRe.FindStringSubmatch(stmt); m != nil {
				oldIdx, newIdx := m[1], m[2]
				if tbl, ok := active[oldIdx]; ok {
					delete(active, oldIdx)
					active[newIdx] = tbl
				}
				continue
			}
			if m := dropTableRe.FindStringSubmatch(stmt); m != nil {
				droppedTable := m[1]
				for idx, tbl := range active {
					if tbl == droppedTable {
						delete(active, idx)
					}
				}
				continue
			}
			if m := dropIndexRe.FindStringSubmatch(stmt); m != nil {
				delete(active, m[1])
				continue
			}
			if m := createIndexRe.FindStringSubmatch(stmt); m != nil {
				active[m[1]] = m[2]
			}
		}
	}
	return active
}

func stripSQLLineComments(sqlText string) string {
	lines := strings.Split(sqlText, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if idx := strings.Index(line, "--"); idx >= 0 {
			line = line[:idx]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// TestMigrationDeclaredIndexesCatalogue verifies (without requiring a live
// Postgres instance) that every load-bearing index expected by the query-plan
// regression suite is declared and active in internal/store/migrations/.
func TestMigrationDeclaredIndexesCatalogue(t *testing.T) {
	active := parseActiveMigrationIndexes(t)
	for _, spec := range loadBearingMigrationIndexes {
		gotTable, exists := active[spec.name]
		require.True(t, exists,
			"load-bearing index %q (for table %q) is missing from internal/store/migrations/*.up.sql",
			spec.name, spec.table)
		assert.Equal(t, spec.table, gotTable,
			"load-bearing index %q declared on unexpected table", spec.name)
	}
}

// TestEvaluateQueryPlanUnit exercises the EXPLAIN (FORMAT JSON) tree inspector
// against synthetic plans so regression-detection logic is verified even when
// TEST_DATABASE_URL is unset.
func TestEvaluateQueryPlanUnit(t *testing.T) {
	assert.Equal(t, "monitor_id_created_at_id_idx",
		normalizePartitionIndexName("alerts_2026_02_monitor_id_created_at_id_idx"))
	assert.Equal(t, "monitor_id_rule_id_created_at_id_idx",
		normalizePartitionIndexName("alerts_2026_02_monitor_id_rule_id_created_at_id_idx"))
	assert.Equal(t, "rule_id_created_at_id_idx",
		normalizePartitionIndexName("alerts_default_rule_id_created_at_id_idx"))
	assert.Equal(t, "delivery_attempts_alert_idx",
		normalizePartitionIndexName("delivery_attempts_alert_idx"))

	dataRels := map[string]bool{
		"alerts_2026_01":    true,
		"alerts_2026_02":    true,
		"alerts_2026_03":    true,
		"delivery_attempts": true,
	}

	t.Run("accepts matching index scan and pruned partitions", func(t *testing.T) {
		plan := queryPlanNode{
			NodeType: "Limit",
			Plans: []queryPlanNode{
				{
					NodeType:           "Aggregate",
					ParentRelationship: "InitPlan",
					Plans: []queryPlanNode{
						{
							NodeType:     "Index Only Scan",
							RelationName: "alerts_2026_01",
							IndexName:    "alerts_2026_01_pkey",
						},
					},
				},
				{
					NodeType:     "Index Scan",
					RelationName: "alerts_2026_02",
					IndexName:    "alerts_2026_02_monitor_id_created_at_id_idx",
				},
			},
		}
		exp := queryPlanExpectation{
			name:                "ListAlerts/monitor_time_window_cursor",
			category:            "cursor_pagination",
			wantIndexSuffixes:   []string{"monitor_id_created_at_id_idx"},
			mustTouchPartitions: []string{"alerts_2026_02"},
			mustPrunePartitions: []string{"alerts_2026_01", "alerts_2026_03"},
		}
		require.NoError(t, evaluateQueryPlan(plan, exp, dataRels))
	})

	t.Run("rejects sequential scan on data-bearing relation", func(t *testing.T) {
		plan := queryPlanNode{
			NodeType:     "Seq Scan",
			RelationName: "alerts_2026_02",
		}
		exp := queryPlanExpectation{
			name:              "ListAlerts/monitor_filter",
			category:          "alerts_list",
			wantIndexSuffixes: []string{"monitor_id_created_at_id_idx"},
		}
		err := evaluateQueryPlan(plan, exp, dataRels)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ListAlerts/monitor_filter")
		assert.Contains(t, err.Error(), "sequential scan")
	})

	t.Run("does not confuse monitor_rule composite index with single-column rule index", func(t *testing.T) {
		plan := queryPlanNode{
			NodeType:     "Index Scan",
			RelationName: "alerts_2026_02",
			IndexName:    "alerts_2026_02_monitor_id_rule_id_created_at_id_idx",
		}
		exp := queryPlanExpectation{
			name:              "ListAlerts/rule_filter",
			category:          "alerts_list",
			wantIndexSuffixes: []string{"rule_id_created_at_id_idx"},
		}
		err := evaluateQueryPlan(plan, exp, dataRels)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ListAlerts/rule_filter")
		assert.Contains(t, err.Error(), "no index scan matched")
	})

	t.Run("rejects unpruned out-of-window partition", func(t *testing.T) {
		plan := queryPlanNode{
			NodeType: "Append",
			Plans: []queryPlanNode{
				{
					NodeType:     "Index Scan",
					RelationName: "alerts_2026_01",
					IndexName:    "alerts_2026_01_monitor_id_created_at_id_idx",
				},
				{
					NodeType:     "Index Scan",
					RelationName: "alerts_2026_02",
					IndexName:    "alerts_2026_02_monitor_id_created_at_id_idx",
				},
			},
		}
		exp := queryPlanExpectation{
			name:                "ListAlerts/monitor_time_window",
			category:            "alerts_list",
			wantIndexSuffixes:   []string{"monitor_id_created_at_id_idx"},
			mustTouchPartitions: []string{"alerts_2026_02"},
			mustPrunePartitions: []string{"alerts_2026_01", "alerts_2026_03"},
		}
		err := evaluateQueryPlan(plan, exp, dataRels)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ListAlerts/monitor_time_window")
		assert.Contains(t, err.Error(), "alerts_2026_01")
		assert.Contains(t, err.Error(), "pruned")
	})
}

// TestMigrationIndexesPresentInPostgres verifies that every active index
// declared in internal/store/migrations/*.up.sql is present in pg_indexes
// after running Migrate.
func TestMigrationIndexesPresentInPostgres(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres migration index catalog check")
	}
	st, ok := newTestPostgres(t).(*Postgres)
	require.True(t, ok)

	rows, err := st.pool.Query(context.Background(), `
		SELECT indexname, tablename
		  FROM pg_indexes
		 WHERE schemaname = current_schema()`)
	require.NoError(t, err)
	defer rows.Close()

	inPostgres := make(map[string]string)
	for rows.Next() {
		var idxName, tblName string
		require.NoError(t, rows.Scan(&idxName, &tblName))
		inPostgres[idxName] = tblName
	}
	require.NoError(t, rows.Err())

	declared := parseActiveMigrationIndexes(t)
	for idxName, wantTable := range declared {
		gotTable, exists := inPostgres[idxName]
		require.True(t, exists,
			"migration-declared index %q on table %q is missing from pg_indexes", idxName, wantTable)
		assert.Equal(t, wantTable, gotTable,
			"migration-declared index %q is attached to unexpected table in pg_indexes", idxName)
	}
}

// TestQueryPlanRegressionGate seeds a multi-partition, multi-workspace dataset
// large enough to exercise PostgreSQL's cost-based planner (24,000 alerts
// across three monthly partitions and 6,000 delivery attempts across 25
// channels), runs EXPLAIN (FORMAT JSON) on every load-bearing store query, and
// verifies both index selection, partition pruning, and failure detection when
// any load-bearing index is dropped.
func TestQueryPlanRegressionGate(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres query-plan regression gate")
	}
	st, ok := newTestPostgres(t).(*Postgres)
	require.True(t, ok)
	ctx := context.Background()

	// Anchor three consecutive monthly partitions around the current UTC month
	// so both explicit [From, To) windows in the middle month and relative
	// lookbacks (now() - 24h / 7d) hit populated partitions and prune older ones.
	now := time.Now().UTC().Truncate(time.Second)
	m0 := monthStart(now).AddDate(0, -2, 0)
	m1 := monthStart(now).AddDate(0, -1, 0)
	m2 := monthStart(now)
	months := []time.Time{m0, m1, m2}

	part0 := alertPartitionName(m0)
	part1 := alertPartitionName(m1)
	part2 := alertPartitionName(m2)

	existingParts, err := st.alertPartitions(ctx)
	require.NoError(t, err)
	existedBefore := make(map[string]bool, len(existingParts))
	for _, p := range existingParts {
		existedBefore[p] = true
	}

	require.NoError(t, st.EnsureAlertPartitions(ctx, m0, 3))
	t.Cleanup(func() {
		_, _ = st.pool.Exec(ctx, `TRUNCATE alerts, delivery_attempts, alert_dedup RESTART IDENTITY CASCADE`)
		for _, p := range []string{part0, part1, part2} {
			if !existedBefore[p] {
				dropPartition(t, st, p)
			}
		}
	})

	const (
		monitorCount             = 100
		alertsPerMonitorPerMonth = 80 // 100 * 80 * 3 = 24,000 alerts total (8,000 per monthly partition)
		channelCount             = 25
		selectiveWorkspace       = workspace.ID("workspace-selective")
		selectiveNetwork         = "futurenet"
	)

	scopedDefault := workspace.With(ctx, workspace.Default)
	scopedSelective := workspace.With(ctx, selectiveWorkspace)

	monitors := make([]int64, monitorCount)
	rules := make([]int64, monitorCount*2)
	for m := 0; m < monitorCount; m++ {
		monCtx := scopedDefault
		net := "testnet"
		if m%2 == 1 {
			net = "mainnet"
		}
		if m == 0 {
			net = selectiveNetwork
		}
		if m == 1 {
			monCtx = scopedSelective
		}
		mon := &Monitor{
			Name:        fmt.Sprintf("qp-mon-%03d", m),
			ContractIDs: []string{fmt.Sprintf("contract-%03d", m)},
			Network:     net,
			Enabled:     true,
		}
		require.NoError(t, st.CreateMonitor(monCtx, mon))
		monitors[m] = mon.ID

		for r := 0; r < 2; r++ {
			rule := &Rule{
				MonitorID: mon.ID,
				Type:      "event_emitted",
				Params:    json.RawMessage(`{}`),
				Enabled:   true,
			}
			require.NoError(t, st.CreateRule(monCtx, rule))
			rules[m*2+r] = rule.ID
		}
	}

	channels := make([]int64, channelCount)
	for c := 0; c < channelCount; c++ {
		ch := &Channel{
			Name:    fmt.Sprintf("qp-chan-%02d", c),
			Type:    "webhook",
			Config:  json.RawMessage(`{"url":"https://example.invalid/hook"}`),
			Enabled: true,
		}
		require.NoError(t, st.CreateChannel(scopedDefault, ch))
		channels[c] = ch.ID
	}

	const totalAlerts = monitorCount * alertsPerMonitorPerMonth * 3
	alertRows := make([][]any, 0, totalAlerts)
	for pIdx, mStart := range months {
		for i := 0; i < alertsPerMonitorPerMonth; i++ {
			for m := 0; m < monitorCount; m++ {
				seq := i*monitorCount + m // 0..7,999 within each monthly partition
				// 8,000 * 4 minutes = 22.22 days, staying strictly within [mStart, mStart + 1 month).
				createdAt := mStart.Add(time.Duration(seq+1) * 4 * time.Minute)
				ws := string(workspace.Default)
				net := "testnet"
				if m%2 == 1 {
					net = "mainnet"
				}
				if m == 0 {
					net = selectiveNetwork
				}
				if m == 1 {
					ws = string(selectiveWorkspace)
				}
				var ackAt any
				if i%2 == 0 {
					tAck := createdAt.Add(time.Minute)
					ackAt = tAck
				}
				ledger := int64(pIdx*10000 + seq + 1)
				alertRows = append(alertRows, []any{
					monitors[m],
					rules[m*2+(i%2)],
					fmt.Sprintf("qp-ev-%d-%d-%d", pIdx, m, i),
					[]byte(fmt.Sprintf(`{"contract_id":"contract-%03d","event_name":"transfer"}`, m)),
					createdAt,
					ws,
					net,
					ackAt,
					ledger,
				})
			}
		}
	}

	_, err = st.pool.CopyFrom(
		ctx,
		pgx.Identifier{"alerts"},
		[]string{"monitor_id", "rule_id", "event_id", "payload", "created_at", "workspace_id", "network", "acknowledged_at", "ledger"},
		pgx.CopyFromRows(alertRows),
	)
	require.NoError(t, err)

	hotMonitor := monitors[0]
	hotRule := rules[0]
	hotChannel := channels[0]

	var cursorAlertID int64
	var cursorCreatedAt time.Time
	require.NoError(t, st.pool.QueryRow(ctx,
		`SELECT id, created_at FROM alerts WHERE monitor_id = $1 AND created_at >= $2 AND created_at < $3 ORDER BY created_at DESC, id DESC LIMIT 1`,
		hotMonitor, m1, m2,
	).Scan(&cursorAlertID, &cursorCreatedAt))

	// Seed 6,000 delivery attempts distributed across 25 channels and across
	// the three monthly windows so both alert_id lookups and (channel_id, attempted_at)
	// aggregates exercise their dedicated indexes.
	const totalAttempts = 6000
	attemptRows := make([][]any, 0, totalAttempts)
	for i := 0; i < totalAttempts; i++ {
		status := DeliveryStatusSuccess
		if i%5 == 0 {
			status = DeliveryStatusFailed
		}
		alertID := cursorAlertID
		alertCreated := cursorCreatedAt
		if i >= 2 {
			alertIdx := (i * 3) % len(alertRows)
			alertID = int64(alertIdx + 1)
			alertCreated = alertRows[alertIdx][4].(time.Time)
		}
		chanID := channels[i%channelCount]
		attemptedAt := alertCreated.Add(15 * time.Second)
		attemptRows = append(attemptRows, []any{
			alertID,
			alertCreated,
			chanID,
			status,
			"",
			attemptedAt,
		})
	}

	_, err = st.pool.CopyFrom(
		ctx,
		pgx.Identifier{"delivery_attempts"},
		[]string{"alert_id", "alert_created_at", "channel_id", "status", "response_snippet", "attempted_at"},
		pgx.CopyFromRows(attemptRows),
	)
	require.NoError(t, err)

	_, err = st.pool.Exec(ctx, `ANALYZE monitors, rules, channels, alerts, delivery_attempts`)
	require.NoError(t, err)

	dataRels := dataBearingRelations(t, st)
	require.True(t, dataRels[part0], "expected partition %s to hold rows", part0)
	require.True(t, dataRels[part1], "expected partition %s to hold rows", part1)
	require.True(t, dataRels[part2], "expected partition %s to hold rows", part2)
	require.True(t, dataRels["delivery_attempts"], "expected delivery_attempts to hold rows")

	expectations := buildQueryPlanExpectations(
		ctx,
		scopedDefault,
		scopedSelective,
		hotMonitor,
		hotRule,
		hotChannel,
		cursorAlertID,
		selectiveNetwork,
		m0, m1, m2,
		part0, part1, part2,
	)

	for _, exp := range expectations {
		t.Run(exp.category+"/"+exp.name, func(t *testing.T) {
			stmt, args := exp.build()
			plan := explainQueryPlan(t, ctx, st.pool, stmt, args...)
			require.NoError(t, evaluateQueryPlan(plan, exp, dataRels),
				"full plan JSON:\n%s", formatPlanJSON(t, plan))
		})
	}

	// Verify the "Done when" invariant directly: removing any load-bearing index
	// inside a rolled-back transaction causes the gate to fail with an error
	// naming the regressed query.
	t.Run("dropping_any_load_bearing_index_fails_gate", func(t *testing.T) {
		byIndex := map[string]queryPlanExpectation{
			"alerts_workspace_created_idx":            findExpectation(t, expectations, "workspace_default_page"),
			"alerts_monitor_created_idx":              findExpectation(t, expectations, "monitor_filter"),
			"alerts_rule_created_idx":                 findExpectation(t, expectations, "rule_filter"),
			"alerts_monitor_rule_created_idx":         findExpectation(t, expectations, "monitor_and_rule_filter"),
			"alerts_contract_created_idx":             findExpectation(t, expectations, "contract_filter"),
			"alerts_network_created_idx":              findExpectation(t, expectations, "network_filter_scoped"),
			"alerts_created_at_idx":                   findExpectation(t, expectations, "expired_alerts_cutoff"),
			"delivery_attempts_alert_idx":             findExpectation(t, expectations, "delivery_attempts_by_alert"),
			"delivery_attempts_channel_attempted_idx": findExpectation(t, expectations, "channel_delivery_stats_window"),
		}

		for idxName, exp := range byIndex {
			t.Run(idxName, func(t *testing.T) {
				tx, err := st.pool.Begin(ctx)
				require.NoError(t, err)
				defer func() { _ = tx.Rollback(ctx) }()

				_, err = tx.Exec(ctx, `DROP INDEX `+idxName)
				require.NoError(t, err)

				stmt, args := exp.build()
				regressedPlan := explainQueryPlan(t, ctx, tx, stmt, args...)
				gateErr := evaluateQueryPlan(regressedPlan, exp, dataRels)
				require.Error(t, gateErr,
					"dropping index %s must cause query %q to fail the query-plan gate", idxName, exp.name)
				assert.Contains(t, gateErr.Error(), exp.name,
					"failure message must name the regressed query")
			})
		}
	})
}

// buildQueryPlanExpectations returns the complete table of load-bearing queries
// and their expected index access paths and partition-pruning invariants.
func buildQueryPlanExpectations(
	unscopedCtx context.Context,
	scopedDefault context.Context,
	scopedSelective context.Context,
	hotMonitor int64,
	hotRule int64,
	hotChannel int64,
	cursorAlertID int64,
	selectiveNetwork string,
	m0, m1, m2 time.Time,
	part0, part1, part2 string,
) []queryPlanExpectation {
	// A 10-day window strictly inside part1 ([m1, m2)), so part0 and part2
	// must be pruned while part1 has ~3,600 rows across all monitors.
	winFrom := m1.Add(5 * 24 * time.Hour)
	winTo := m1.Add(15 * 24 * time.Hour)

	// A cutoff near the tail of part2 (matching ~4% of part2 and 0 rows in part0/part1).
	recentCutoffPart2 := m2.Add(21 * 24 * time.Hour)

	// An expiration cutoff near the start of part0 (matching the earliest ~4% of part0
	// and pruning part1 and part2).
	expireCutoffPart0 := m0.Add(24 * time.Hour)

	return []queryPlanExpectation{
		// --- 1. Alerts listing: individual filters and combinations ---
		{
			name:              "workspace_default_page",
			category:          "alerts_list",
			wantIndexSuffixes: []string{"workspace_id_created_at_id_idx"},
			build: func() (string, []any) {
				return buildAlertQuery(scopedDefault, AlertFilter{Limit: 50})
			},
		},
		{
			name:              "monitor_filter",
			category:          "alerts_list",
			wantIndexSuffixes: []string{"monitor_id_created_at_id_idx"},
			build: func() (string, []any) {
				return buildAlertQuery(scopedDefault, AlertFilter{MonitorID: hotMonitor, Limit: 50})
			},
		},
		{
			name:              "rule_filter",
			category:          "alerts_list",
			wantIndexSuffixes: []string{"rule_id_created_at_id_idx"},
			build: func() (string, []any) {
				return buildAlertQuery(scopedDefault, AlertFilter{RuleID: hotRule, Limit: 50})
			},
		},
		{
			name:              "contract_filter",
			category:          "alerts_list",
			wantIndexSuffixes: []string{"expr_created_at_id_idx"},
			build: func() (string, []any) {
				return buildAlertQuery(scopedDefault, AlertFilter{ContractID: "contract-000", Limit: 50})
			},
		},
		{
			name:              "network_filter_unscoped",
			category:          "alerts_list",
			wantIndexSuffixes: []string{"network_created_at_id_idx"},
			build: func() (string, []any) {
				return buildAlertQuery(unscopedCtx, AlertFilter{Network: "testnet", Limit: 50})
			},
		},
		{
			name:              "network_filter_scoped",
			category:          "alerts_list",
			wantIndexSuffixes: []string{"network_created_at_id_idx"},
			build: func() (string, []any) {
				return buildAlertQuery(scopedDefault, AlertFilter{Network: selectiveNetwork, Limit: 50})
			},
		},
		{
			name:              "acknowledged_filter",
			category:          "alerts_list",
			wantIndexSuffixes: []string{"workspace_id_created_at_id_idx"},
			build: func() (string, []any) {
				return buildAlertQuery(scopedDefault, AlertFilter{Acknowledged: "acknowledged", Limit: 50})
			},
		},
		{
			name:              "unacknowledged_filter",
			category:          "alerts_list",
			wantIndexSuffixes: []string{"workspace_id_created_at_id_idx"},
			build: func() (string, []any) {
				return buildAlertQuery(scopedDefault, AlertFilter{Acknowledged: "unacknowledged", Limit: 50})
			},
		},
		{
			name:                "time_window_filter_prunes_partitions",
			category:            "alerts_list",
			wantIndexSuffixes:   []string{"workspace_id_created_at_id_idx", "created_at_idx"},
			mustTouchPartitions: []string{part1},
			mustPrunePartitions: []string{part0, part2},
			build: func() (string, []any) {
				return buildAlertQuery(scopedDefault, AlertFilter{From: winFrom, To: winTo, Limit: 50})
			},
		},
		{
			name:              "monitor_and_rule_filter",
			category:          "alerts_list",
			wantIndexSuffixes: []string{"monitor_id_rule_id_created_at_id_idx"},
			build: func() (string, []any) {
				return buildAlertQuery(scopedDefault, AlertFilter{MonitorID: hotMonitor, RuleID: hotRule, Limit: 50})
			},
		},
		{
			name:                "monitor_and_time_window_filter",
			category:            "alerts_list",
			wantIndexSuffixes:   []string{"monitor_id_created_at_id_idx"},
			mustTouchPartitions: []string{part1},
			mustPrunePartitions: []string{part0, part2},
			build: func() (string, []any) {
				return buildAlertQuery(scopedDefault, AlertFilter{MonitorID: hotMonitor, From: winFrom, To: winTo, Limit: 50})
			},
		},
		{
			name:                "rule_and_time_window_filter",
			category:            "alerts_list",
			wantIndexSuffixes:   []string{"rule_id_created_at_id_idx"},
			mustTouchPartitions: []string{part1},
			mustPrunePartitions: []string{part0, part2},
			build: func() (string, []any) {
				return buildAlertQuery(scopedDefault, AlertFilter{RuleID: hotRule, From: winFrom, To: winTo, Limit: 50})
			},
		},
		{
			name:                "contract_and_time_window_filter",
			category:            "alerts_list",
			wantIndexSuffixes:   []string{"expr_created_at_id_idx"},
			mustTouchPartitions: []string{part1},
			mustPrunePartitions: []string{part0, part2},
			build: func() (string, []any) {
				return buildAlertQuery(scopedDefault, AlertFilter{ContractID: "contract-000", From: winFrom, To: winTo, Limit: 50})
			},
		},
		{
			name:                "network_and_time_window_filter",
			category:            "alerts_list",
			wantIndexSuffixes:   []string{"network_created_at_id_idx"},
			mustTouchPartitions: []string{part1},
			mustPrunePartitions: []string{part0, part2},
			build: func() (string, []any) {
				return buildAlertQuery(scopedDefault, AlertFilter{Network: selectiveNetwork, From: winFrom, To: winTo, Limit: 50})
			},
		},
		{
			name:                "monitor_rule_and_time_window_filter",
			category:            "alerts_list",
			wantIndexSuffixes:   []string{"monitor_id_rule_id_created_at_id_idx"},
			mustTouchPartitions: []string{part1},
			mustPrunePartitions: []string{part0, part2},
			build: func() (string, []any) {
				return buildAlertQuery(scopedDefault, AlertFilter{MonitorID: hotMonitor, RuleID: hotRule, From: winFrom, To: winTo, Limit: 50})
			},
		},
		{
			name:              "monitor_and_acknowledged_filter",
			category:          "alerts_list",
			wantIndexSuffixes: []string{"monitor_id_created_at_id_idx"},
			build: func() (string, []any) {
				return buildAlertQuery(scopedDefault, AlertFilter{MonitorID: hotMonitor, Acknowledged: "unacknowledged", Limit: 50})
			},
		},

		// --- 2. Cursor-paged reads (keyset pagination) ---
		{
			name:              "cursor_desc_workspace",
			category:          "cursor_pagination",
			wantIndexSuffixes: []string{"workspace_id_created_at_id_idx"},
			build: func() (string, []any) {
				return buildAlertQuery(scopedDefault, AlertFilter{AfterID: cursorAlertID, Limit: 50})
			},
		},
		{
			name:              "cursor_asc_workspace",
			category:          "cursor_pagination",
			wantIndexSuffixes: []string{"workspace_id_created_at_id_idx"},
			build: func() (string, []any) {
				return buildAlertQuery(scopedDefault, AlertFilter{AfterID: cursorAlertID, Sort: "created_at_asc", Limit: 50})
			},
		},
		{
			name:              "cursor_desc_monitor",
			category:          "cursor_pagination",
			wantIndexSuffixes: []string{"monitor_id_created_at_id_idx"},
			build: func() (string, []any) {
				return buildAlertQuery(scopedDefault, AlertFilter{MonitorID: hotMonitor, AfterID: cursorAlertID, Limit: 50})
			},
		},
		{
			name:              "cursor_asc_monitor",
			category:          "cursor_pagination",
			wantIndexSuffixes: []string{"monitor_id_created_at_id_idx"},
			build: func() (string, []any) {
				return buildAlertQuery(scopedDefault, AlertFilter{MonitorID: hotMonitor, AfterID: cursorAlertID, Sort: "created_at_asc", Limit: 50})
			},
		},
		{
			name:              "cursor_desc_rule",
			category:          "cursor_pagination",
			wantIndexSuffixes: []string{"rule_id_created_at_id_idx"},
			build: func() (string, []any) {
				return buildAlertQuery(scopedDefault, AlertFilter{RuleID: hotRule, AfterID: cursorAlertID, Limit: 50})
			},
		},
		{
			name:                "cursor_desc_monitor_with_time_window",
			category:            "cursor_pagination",
			wantIndexSuffixes:   []string{"monitor_id_created_at_id_idx"},
			mustTouchPartitions: []string{part1},
			mustPrunePartitions: []string{part0, part2},
			build: func() (string, []any) {
				return buildAlertQuery(scopedDefault, AlertFilter{
					MonitorID: hotMonitor,
					From:      winFrom,
					To:        winTo,
					AfterID:   cursorAlertID,
					Limit:     50,
				})
			},
		},

		// --- 3. Alert statistics and rule-window queries ---
		{
			name:              "stats_workspace_total_alerts",
			category:          "alert_stats",
			wantIndexSuffixes: []string{"workspace_id_created_at_id_idx"},
			build: func() (string, []any) {
				return `SELECT count(*) FROM alerts WHERE workspace_id = $1`,
					[]any{workspaceID(scopedSelective)}
			},
		},
		{
			name:                "stats_workspace_24h_window",
			category:            "alert_stats",
			wantIndexSuffixes:   []string{"workspace_id_created_at_id_idx", "created_at_idx"},
			mustTouchPartitions: []string{part2},
			mustPrunePartitions: []string{part0, part1},
			build: func() (string, []any) {
				return `SELECT count(*) FROM alerts WHERE workspace_id = $1 AND created_at > $2`,
					[]any{workspace.Default, recentCutoffPart2}
			},
		},
		{
			name:              "monitor_stats_total_and_last_alert",
			category:          "alert_stats",
			wantIndexSuffixes: []string{"monitor_id_created_at_id_idx"},
			build: func() (string, []any) {
				return `SELECT
					(SELECT count(*) FROM alerts WHERE monitor_id = $1 AND workspace_id = $2),
					(SELECT max(created_at) FROM alerts WHERE monitor_id = $1 AND workspace_id = $2)`,
					[]any{hotMonitor, workspace.Default}
			},
		},
		{
			name:                "monitor_stats_recent_window_counts",
			category:            "alert_stats",
			wantIndexSuffixes:   []string{"monitor_id_created_at_id_idx"},
			mustTouchPartitions: []string{part2},
			mustPrunePartitions: []string{part0},
			build: func() (string, []any) {
				return `SELECT
					(SELECT count(*) FROM alerts WHERE monitor_id = $1 AND workspace_id = $2 AND created_at > now() - interval '24 hours'),
					(SELECT count(*) FROM alerts WHERE monitor_id = $1 AND workspace_id = $2 AND created_at > now() - interval '7 days')`,
					[]any{hotMonitor, workspace.Default}
			},
		},
		{
			name:              "monitor_stats_per_rule_counts",
			category:          "alert_stats",
			wantIndexSuffixes: []string{"rule_id_created_at_id_idx"},
			build: func() (string, []any) {
				return `SELECT r.id, r.type, count(a.id)
					FROM rules r
					LEFT JOIN alerts a ON a.rule_id = r.id
					WHERE r.monitor_id = $1
					GROUP BY r.id, r.type
					ORDER BY r.id`,
					[]any{hotMonitor}
			},
		},
		{
			name:                "rule_fired_within_window",
			category:            "alert_stats",
			wantIndexSuffixes:   []string{"rule_id_created_at_id_idx"},
			mustTouchPartitions: []string{part2},
			mustPrunePartitions: []string{part0, part1},
			build: func() (string, []any) {
				return `SELECT EXISTS(SELECT 1 FROM alerts WHERE rule_id = $1 AND created_at >= $2)`,
					[]any{hotRule, recentCutoffPart2}
			},
		},
		{
			name:                "list_rule_alert_times_window",
			category:            "alert_stats",
			wantIndexSuffixes:   []string{"rule_id_created_at_id_idx"},
			mustTouchPartitions: []string{part2},
			mustPrunePartitions: []string{part0, part1},
			build: func() (string, []any) {
				return `SELECT created_at FROM alerts WHERE rule_id = $1 AND created_at >= $2 AND workspace_id = $3 ORDER BY created_at`,
					[]any{hotRule, recentCutoffPart2, workspace.Default}
			},
		},
		{
			name:                "expired_alerts_cutoff",
			category:            "alert_stats",
			wantIndexSuffixes:   []string{"created_at_idx"},
			mustTouchPartitions: []string{part0},
			mustPrunePartitions: []string{part1, part2},
			build: func() (string, []any) {
				return `SELECT id, monitor_id, rule_id, event_id, payload, enrichment, created_at, ledger, retracted_at, backfilled, inhibited_by_rule_id, suppressed, suppression_reason, acknowledged_at, acknowledged_by, network, federated, federation_origin, source_monitor_id, source_rule_id, rule_version
					FROM alerts WHERE created_at < $1 ORDER BY created_at ASC, id ASC LIMIT $2`,
					[]any{expireCutoffPart0, 100}
			},
		},

		// --- 4. Delivery-attempt history ---
		{
			name:              "delivery_attempts_by_alert",
			category:          "delivery_history",
			wantIndexSuffixes: []string{"delivery_attempts_alert_idx"},
			build: func() (string, []any) {
				return buildDeliveryAttemptsQuery(cursorAlertID, "", workspace.Default)
			},
		},
		{
			name:              "delivery_attempts_by_alert_and_status",
			category:          "delivery_history",
			wantIndexSuffixes: []string{"delivery_attempts_alert_idx"},
			build: func() (string, []any) {
				return buildDeliveryAttemptsQuery(cursorAlertID, DeliveryStatusFailed, workspace.Default)
			},
		},
		{
			name:              "channel_delivery_stats_window",
			category:          "delivery_history",
			wantIndexSuffixes: []string{"delivery_attempts_channel_attempted_idx"},
			build: func() (string, []any) {
				return `SELECT
					(SELECT count(*) FROM channels WHERE id = $1 AND workspace_id = $5) > 0,
					count(*),
					count(*) FILTER (WHERE da.status = $2),
					count(*) FILTER (WHERE da.status = $3),
					max(da.attempted_at) FILTER (WHERE da.status = $2),
					max(da.attempted_at) FILTER (WHERE da.status = $3)
				FROM delivery_attempts da
				WHERE da.channel_id = $1 AND da.attempted_at >= $4`,
					[]any{hotChannel, DeliveryStatusSuccess, DeliveryStatusFailed, m1, workspace.Default}
			},
		},
		{
			name:              "channel_delivery_change_key",
			category:          "delivery_history",
			wantIndexSuffixes: []string{"delivery_attempts_channel_attempted_idx"},
			build: func() (string, []any) {
				return `SELECT
					(SELECT count(*) FROM channels WHERE id = $1 AND workspace_id = $2) > 0,
					COALESCE(max(da.id), 0),
					count(*)
				FROM delivery_attempts da
				WHERE da.channel_id = $1`,
					[]any{hotChannel, workspace.Default}
			},
		},
	}
}

type queryRowRunner interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func explainQueryPlan(t *testing.T, ctx context.Context, q queryRowRunner, statement string, args ...any) queryPlanNode {
	t.Helper()
	var raw []byte
	require.NoError(t, q.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+statement, args...).Scan(&raw))
	var wrapped []struct {
		Plan queryPlanNode `json:"Plan"`
	}
	require.NoError(t, json.Unmarshal(raw, &wrapped))
	require.Len(t, wrapped, 1, "EXPLAIN (FORMAT JSON) wraps a single plan in a one-element array")
	return wrapped[0].Plan
}

func findExpectation(t *testing.T, all []queryPlanExpectation, name string) queryPlanExpectation {
	t.Helper()
	for _, exp := range all {
		if exp.name == name {
			return exp
		}
	}
	t.Fatalf("queryPlanExpectation %q not found", name)
	return queryPlanExpectation{}
}

func formatPlanJSON(t *testing.T, plan queryPlanNode) string {
	t.Helper()
	raw, err := json.MarshalIndent(plan, "", "  ")
	require.NoError(t, err)
	return string(raw)
}
