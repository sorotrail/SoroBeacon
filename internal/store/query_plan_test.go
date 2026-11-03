package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// QueryPlanIndexSpec pairs a logical index name from internal/store/migrations
// with the substring Postgres uses in EXPLAIN (FORMAT JSON) plans (including
// per-partition child index names generated as <partition>_<columns>_idx) and
// the reason the index exists.
type QueryPlanIndexSpec struct {
	MigrationIndex  string
	Table           string
	PlanToken       string
	ProtectedReason string
}

// queryPlanIndexCatalog is the authoritative registry of indexes that back hot
// read paths in internal/store. Removing or renaming any of these indexes in a
// migration without updating this catalog and recording a reason will fail
// TestMigrationIndexesMatchQueryPlanCatalog immediately in `go test ./...`.
var queryPlanIndexCatalog = []QueryPlanIndexSpec{
	{
		MigrationIndex:  "alerts_workspace_created_idx",
		Table:           "alerts",
		PlanToken:       "workspace_id_created_at_id_idx",
		ProtectedReason: "Tenant-scoped alert feeds, 24h counters, and cursor pagination ordered by (created_at, id)",
	},
	{
		MigrationIndex:  "alerts_monitor_created_idx",
		Table:           "alerts",
		PlanToken:       "monitor_id_created_at_id_idx",
		ProtectedReason: "Monitor-filtered alert feeds, per-monitor stats, and keyset pagination",
	},
	{
		MigrationIndex:  "alerts_rule_created_idx",
		Table:           "alerts",
		PlanToken:       "rule_id_created_at_id_idx",
		ProtectedReason: "Rule-filtered alert feeds, inhibition firing checks, and per-rule alert counts",
	},
	{
		MigrationIndex:  "alerts_contract_created_idx",
		Table:           "alerts",
		PlanToken:       "created_at_id_idx",
		ProtectedReason: "Contract-filtered alert feeds reading payload->>'contract_id' in reverse chronological order",
	},
	{
		MigrationIndex:  "alerts_monitor_rule_created_idx",
		Table:           "alerts",
		PlanToken:       "monitor_id_rule_id_created_at_id_idx",
		ProtectedReason: "Combined monitor + rule alert feeds without post-filtering",
	},
	{
		MigrationIndex:  "alerts_network_created_idx",
		Table:           "alerts",
		PlanToken:       "network_created_at_id_idx",
		ProtectedReason: "Network-filtered alert feeds across multi-network deployments",
	},
	{
		MigrationIndex:  "alerts_created_at_idx",
		Table:           "alerts",
		PlanToken:       "created_at",
		ProtectedReason: "Time-windowed scans and retention sweeps across monthly alert partitions",
	},
	{
		MigrationIndex:  "delivery_attempts_alert_idx",
		Table:           "delivery_attempts",
		PlanToken:       "delivery_attempts_alert_idx",
		ProtectedReason: "Per-alert delivery attempt history and monitor delivery aggregates",
	},
	{
		MigrationIndex:  "delivery_attempts_channel_attempted_idx",
		Table:           "delivery_attempts",
		PlanToken:       "delivery_attempts_channel_attempted_idx",
		ProtectedReason: "Per-channel windowed delivery statistics (GetChannelDeliveryStats)",
	},
	{
		MigrationIndex:  "monitors_workspace_idx",
		Table:           "monitors",
		PlanToken:       "monitors_workspace_idx",
		ProtectedReason: "Workspace-scoped monitor listings and id-ordered cursor pagination",
	},
	{
		MigrationIndex:  "monitors_workspace_name_idx",
		Table:           "monitors",
		PlanToken:       "monitors_workspace_name_idx",
		ProtectedReason: "Workspace-scoped monitor listings sorted by lower(name), id",
	},
	{
		MigrationIndex:  "channels_workspace_idx",
		Table:           "channels",
		PlanToken:       "channels_workspace_idx",
		ProtectedReason: "Workspace-scoped channel listings and cursor pagination",
	},
	{
		MigrationIndex:  "audit_log_created_at_idx",
		Table:           "audit_log",
		PlanToken:       "audit_log",
		ProtectedReason: "Time-bounded audit log queries",
	},
	{
		MigrationIndex:  "audit_log_target_idx",
		Table:           "audit_log",
		PlanToken:       "audit_log_target_idx",
		ProtectedReason: "Target-filtered audit log queries (target_type, target_id, created_at DESC)",
	},
}

// QueryPlanExpectation describes the required execution plan properties for one
// hot query. Every expectation carries a human-readable Reason so deliberate
// query or index changes document why the expected plan changed.
type QueryPlanExpectation struct {
	Name                   string
	Reason                 string
	Query                  string
	Args                   []any
	RequireIndexAccess     bool
	ExpectedIndexTokens    []string // Any matching token satisfies the index check unless RequireAllIndexTokens is true
	RequireAllIndexTokens  bool
	ForbidSeqScanRelations map[string]bool
	ExpectedPartitions     []string
	PrunedPartitions       []string
}

// QueryPlanViolation captures a regression in an EXPLAIN (FORMAT JSON) plan
// with the full context needed to diagnose the failure in CI logs.
type QueryPlanViolation struct {
	ExpectationName string
	Reason          string
	Query           string
	ExpectedPath    string
	FailureSummary  string
	ActualPlanJSON  string
}

func (v *QueryPlanViolation) Error() string {
	return fmt.Sprintf(
		"query plan regression in %q\n  reason:        %s\n  expected path: %s\n  violation:     %s\n  query:         %s\n  actual plan:\n%s",
		v.ExpectationName,
		v.Reason,
		v.ExpectedPath,
		v.FailureSummary,
		compactSQL(v.Query),
		v.ActualPlanJSON,
	)
}

func compactSQL(q string) string {
	return strings.Join(strings.Fields(q), " ")
}

func formatExpectedPath(exp QueryPlanExpectation) string {
	var parts []string
	if len(exp.ExpectedIndexTokens) > 0 {
		mode := "any of"
		if exp.RequireAllIndexTokens {
			mode = "all of"
		}
		parts = append(parts, fmt.Sprintf("index scan (%s: %s)", mode, strings.Join(exp.ExpectedIndexTokens, ", ")))
	} else if exp.RequireIndexAccess {
		parts = append(parts, "index-backed scan (Index Scan / Index Only Scan / Bitmap Index Scan)")
	}
	if len(exp.ForbidSeqScanRelations) > 0 {
		rels := make([]string, 0, len(exp.ForbidSeqScanRelations))
		for r, forbid := range exp.ForbidSeqScanRelations {
			if forbid {
				rels = append(rels, r)
			}
		}
		sort.Strings(rels)
		parts = append(parts, fmt.Sprintf("no Seq Scan on [%s]", strings.Join(rels, ", ")))
	}
	if len(exp.ExpectedPartitions) > 0 {
		parts = append(parts, fmt.Sprintf("scans active partitions [%s]", strings.Join(exp.ExpectedPartitions, ", ")))
	}
	if len(exp.PrunedPartitions) > 0 {
		parts = append(parts, fmt.Sprintf("prunes out-of-range partitions [%s]", strings.Join(exp.PrunedPartitions, ", ")))
	}
	if len(parts) == 0 {
		return "any valid plan"
	}
	return strings.Join(parts, "; ")
}

// evaluateQueryPlan inspects a parsed EXPLAIN (FORMAT JSON) plan tree against
// exp and returns a descriptive *QueryPlanViolation when any gate fails.
func evaluateQueryPlan(exp QueryPlanExpectation, plan explainNode) *QueryPlanViolation {
	var (
		seqScans       []string
		indexNames     []string
		hasIndexNode   bool
		touchedObjects = make(map[string]bool)
	)

	plan.walkPlan(func(n explainNode) {
		if n.RelationName != "" {
			touchedObjects[n.RelationName] = true
		}
		if n.IndexName != "" {
			touchedObjects[n.IndexName] = true
			indexNames = append(indexNames, n.IndexName)
		}
		if strings.Contains(n.NodeType, "Index Scan") ||
			strings.Contains(n.NodeType, "Index Only Scan") ||
			strings.Contains(n.NodeType, "Bitmap Index Scan") {
			hasIndexNode = true
		}
		if strings.Contains(n.NodeType, "Seq Scan") && exp.ForbidSeqScanRelations[n.RelationName] {
			seqScans = append(seqScans, fmt.Sprintf("%s on %s", n.NodeType, n.RelationName))
		}
	})

	fail := func(summary string) *QueryPlanViolation {
		dump, err := json.MarshalIndent(plan, "", "  ")
		if err != nil {
			dump = []byte(fmt.Sprintf("<marshal error: %v>", err))
		}
		return &QueryPlanViolation{
			ExpectationName: exp.Name,
			Reason:          exp.Reason,
			Query:           exp.Query,
			ExpectedPath:    formatExpectedPath(exp),
			FailureSummary:  summary,
			ActualPlanJSON:  string(dump),
		}
	}

	if len(seqScans) > 0 {
		return fail(fmt.Sprintf("sequential scan over data-bearing relation(s): %s", strings.Join(seqScans, ", ")))
	}

	if exp.RequireIndexAccess && !hasIndexNode {
		return fail("plan contains no Index Scan, Index Only Scan, or Bitmap Index Scan node")
	}

	if len(exp.ExpectedIndexTokens) > 0 {
		if exp.RequireAllIndexTokens {
			var missing []string
			for _, want := range exp.ExpectedIndexTokens {
				found := false
				for _, got := range indexNames {
					if strings.Contains(got, want) {
						found = true
						break
					}
				}
				if !found {
					missing = append(missing, want)
				}
			}
			if len(missing) > 0 {
				return fail(fmt.Sprintf("missing required index token(s) %v in scanned indexes %v", missing, indexNames))
			}
		} else {
			matched := false
			for _, want := range exp.ExpectedIndexTokens {
				for _, got := range indexNames {
					if strings.Contains(got, want) {
						matched = true
						break
					}
				}
				if matched {
					break
				}
			}
			if !matched {
				return fail(fmt.Sprintf("none of expected index tokens %v matched scanned indexes %v", exp.ExpectedIndexTokens, indexNames))
			}
		}
	}

	for _, wantPart := range exp.ExpectedPartitions {
		matched := false
		for obj := range touchedObjects {
			if strings.Contains(obj, wantPart) {
				matched = true
				break
			}
		}
		if !matched {
			return fail(fmt.Sprintf("expected partition %q was not referenced by any scan or index in plan", wantPart))
		}
	}

	var unpruned []string
	for _, prunedPart := range exp.PrunedPartitions {
		for obj := range touchedObjects {
			if strings.Contains(obj, prunedPart) {
				unpruned = append(unpruned, fmt.Sprintf("%s (via %s)", prunedPart, obj))
				break
			}
		}
	}
	if len(unpruned) > 0 {
		return fail(fmt.Sprintf("partition pruning regression: out-of-range partition(s) scanned: %s", strings.Join(unpruned, ", ")))
	}

	return nil
}

func assertQueryPlan(t *testing.T, st *Postgres, exp QueryPlanExpectation) {
	t.Helper()
	plan := explainJSON(t, st, exp.Query, exp.Args...)
	if v := evaluateQueryPlan(exp, plan); v != nil {
		t.Fatalf("%s", v.Error())
	}
}

var (
	createIndexRe = regexp.MustCompile(`(?i)\bCREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?([a-zA-Z0-9_]+)\s+ON\s+([a-zA-Z0-9_]+)`)
	dropIndexRe   = regexp.MustCompile(`(?i)\bDROP\s+INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+EXISTS\s+)?([a-zA-Z0-9_]+)`)
)

// activeMigrationIndexes replays all *.up.sql files in lexical order and
// returns the set of indexes that remain active after all up-migrations run.
// Note that 0011_alerts_partitioning.up.sql drops alerts_old (which drops the
// pre-partitioning 0002/0003 indexes) and recreates them on the partitioned
// alerts table.
func activeMigrationIndexes(t *testing.T) map[string]string {
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
	require.NotEmpty(t, upFiles, "expected embedded Postgres .up.sql migrations")

	active := make(map[string]string)
	for _, name := range upFiles {
		raw, err := fs.ReadFile(migrationsFS, "migrations/"+name)
		require.NoError(t, err)
		sqlText := stripSQLComments(string(raw))

		for _, m := range dropIndexRe.FindAllStringSubmatch(sqlText, -1) {
			delete(active, strings.ToLower(m[1]))
		}
		for _, m := range createIndexRe.FindAllStringSubmatch(sqlText, -1) {
			idxName := strings.ToLower(m[1])
			tableName := strings.ToLower(m[2])
			active[idxName] = tableName
		}
	}
	return active
}

func stripSQLComments(sqlText string) string {
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

// TestMigrationIndexesMatchQueryPlanCatalog runs unconditionally in `go test`
// (without needing TEST_DATABASE_URL) and fails immediately if a migration
// drops or renames an index that a gated query plan depends on.
func TestMigrationIndexesMatchQueryPlanCatalog(t *testing.T) {
	active := activeMigrationIndexes(t)
	for _, spec := range queryPlanIndexCatalog {
		gotTable, ok := active[strings.ToLower(spec.MigrationIndex)]
		require.True(t, ok,
			"migration index %q on table %q (required for: %s) is missing from internal/store/migrations/*.up.sql; update docs/query-plans.md and queryPlanIndexCatalog if this change is deliberate",
			spec.MigrationIndex, spec.Table, spec.ProtectedReason)
		assert.Equal(t, strings.ToLower(spec.Table), gotTable,
			"index %q expected on table %q, found on %q", spec.MigrationIndex, spec.Table, gotTable)
	}

	t.Run("detects dropped index in migration stream", func(t *testing.T) {
		simulated := make(map[string]string, len(active))
		for k, v := range active {
			simulated[k] = v
		}
		dropSQL := stripSQLComments("-- simulate accidental index drop\nDROP INDEX IF EXISTS alerts_monitor_created_idx;")
		for _, m := range dropIndexRe.FindAllStringSubmatch(dropSQL, -1) {
			delete(simulated, strings.ToLower(m[1]))
		}
		_, stillPresent := simulated["alerts_monitor_created_idx"]
		assert.False(t, stillPresent, "parser must remove dropped index from active catalog")
	})
}

// TestQueryPlanGateAnalyzer verifies the EXPLAIN (FORMAT JSON) plan evaluator,
// sequential-scan detector, index-token matcher, partition-pruning verifier,
// and diagnostic failure formatter in standard unit test runs.
func TestQueryPlanGateAnalyzer(t *testing.T) {
	dataRels := map[string]bool{
		"alerts_2026_01":    true,
		"alerts_2026_02":    true,
		"alerts_2026_03":    true,
		"delivery_attempts": true,
	}

	t.Run("accepts index scan on pruned monthly partition", func(t *testing.T) {
		plan := explainNode{
			NodeType: "Limit",
			Plans: []explainNode{
				{
					NodeType:     "Index Scan",
					RelationName: "alerts_2026_02",
					IndexName:    "alerts_2026_02_monitor_id_created_at_id_idx",
					PlanRows:     50,
				},
			},
		}
		exp := QueryPlanExpectation{
			Name:                   "monitor-filtered single-month alerts query",
			Reason:                 "Must use composite (monitor_id, created_at DESC, id DESC) index and prune adjacent months",
			Query:                  "SELECT id FROM alerts WHERE workspace_id = $1 AND monitor_id = $2 AND created_at >= $3 AND created_at <= $4 ORDER BY created_at DESC, id DESC LIMIT $5",
			RequireIndexAccess:     true,
			ExpectedIndexTokens:    []string{"monitor_id_created_at_id_idx"},
			ForbidSeqScanRelations: dataRels,
			ExpectedPartitions:     []string{"alerts_2026_02"},
			PrunedPartitions:       []string{"alerts_2026_01", "alerts_2026_03"},
		}
		assert.Nil(t, evaluateQueryPlan(exp, plan))
	})

	t.Run("rejects sequential scan on data-bearing partition with full diagnostics", func(t *testing.T) {
		plan := explainNode{
			NodeType: "Limit",
			Plans: []explainNode{
				{
					NodeType: "Append",
					Plans: []explainNode{
						{
							NodeType:     "Seq Scan",
							RelationName: "alerts_2026_02",
							PlanRows:     10000,
						},
					},
				},
			},
		}
		exp := QueryPlanExpectation{
			Name:                   "alerts listing by severity",
			Reason:                 "Must descend workspace/created_at index and filter without full partition table scan",
			Query:                  "SELECT id FROM alerts WHERE workspace_id = $1 AND severity = $2 ORDER BY created_at DESC, id DESC LIMIT $3",
			RequireIndexAccess:     true,
			ExpectedIndexTokens:    []string{"workspace_id_created_at_id_idx", "created_at"},
			ForbidSeqScanRelations: dataRels,
		}
		v := evaluateQueryPlan(exp, plan)
		require.NotNil(t, v, "expected sequential scan on alerts_2026_02 to fail the gate")
		msg := v.Error()
		assert.Contains(t, msg, "alerts listing by severity")
		assert.Contains(t, msg, "Seq Scan on alerts_2026_02")
		assert.Contains(t, msg, "SELECT id FROM alerts WHERE workspace_id = $1")
		assert.Contains(t, msg, "expected path:")
		assert.Contains(t, msg, `"Node Type": "Seq Scan"`)
	})

	t.Run("rejects unpruned out-of-range monthly partitions", func(t *testing.T) {
		plan := explainNode{
			NodeType: "Append",
			Plans: []explainNode{
				{
					NodeType:     "Index Scan",
					RelationName: "alerts_2026_01",
					IndexName:    "alerts_2026_01_workspace_id_created_at_id_idx",
				},
				{
					NodeType:     "Index Scan",
					RelationName: "alerts_2026_02",
					IndexName:    "alerts_2026_02_workspace_id_created_at_id_idx",
				},
			},
		}
		exp := QueryPlanExpectation{
			Name:                   "single-month bounded query",
			Reason:                 "Time window inside 2026-02 must prune 2026-01 and 2026-03",
			Query:                  "SELECT id FROM alerts WHERE workspace_id = $1 AND created_at >= $2 AND created_at <= $3",
			RequireIndexAccess:     true,
			ForbidSeqScanRelations: dataRels,
			ExpectedPartitions:     []string{"alerts_2026_02"},
			PrunedPartitions:       []string{"alerts_2026_01", "alerts_2026_03"},
		}
		v := evaluateQueryPlan(exp, plan)
		require.NotNil(t, v, "expected touching alerts_2026_01 to fail partition pruning check")
		assert.Contains(t, v.Error(), "partition pruning regression")
		assert.Contains(t, v.Error(), "alerts_2026_01")
	})

	t.Run("rejects plan when expected index token is absent", func(t *testing.T) {
		plan := explainNode{
			NodeType:     "Index Scan",
			RelationName: "delivery_attempts",
			IndexName:    "delivery_attempts_pkey",
		}
		exp := QueryPlanExpectation{
			Name:                   "channel delivery stats window",
			Reason:                 "Must use delivery_attempts_channel_attempted_idx",
			Query:                  "SELECT count(*) FROM delivery_attempts WHERE channel_id = $1 AND attempted_at >= $2",
			RequireIndexAccess:     true,
			ExpectedIndexTokens:    []string{"delivery_attempts_channel_attempted_idx"},
			ForbidSeqScanRelations: dataRels,
		}
		v := evaluateQueryPlan(exp, plan)
		require.NotNil(t, v)
		assert.Contains(t, v.Error(), "delivery_attempts_channel_attempted_idx")
	})
}

// TestPostgresQueryPlanRegressions seeds a multi-month, multi-tenant Postgres
// dataset across three monthly alerts partitions, runs ANALYZE, and gates the
// EXPLAIN (FORMAT JSON) plans for all hot store read paths:
//  1. Alerts listing with every individual and combined filter
//  2. Partition pruning on time-bounded alert queries
//  3. Alert statistics (GetStats, GetMonitorStats, AlertCountsByDay)
//  4. Delivery-attempt history and per-channel delivery statistics
//  5. Cursor-paged reads across alerts, monitors, channels, and audit_log
func TestPostgresQueryPlanRegressions(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	// Create 3 distinct monthly partitions in a dedicated historical year so
	// partition pruning assertions are deterministic regardless of wall clock.
	month0 := time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC)
	month1 := time.Date(2025, time.February, 1, 0, 0, 0, 0, time.UTC)
	month2 := time.Date(2025, time.March, 1, 0, 0, 0, 0, time.UTC)
	part0 := alertPartitionName(month0) // alerts_2025_01
	part1 := alertPartitionName(month1) // alerts_2025_02
	part2 := alertPartitionName(month2) // alerts_2025_03

	_ = st.resetConformance(ctx)
	for _, pName := range []string{part0, part1, part2} {
		dropPartition(t, st, pName)
	}
	t.Cleanup(func() {
		_ = st.resetConformance(context.Background())
		for _, pName := range []string{part0, part1, part2} {
			dropPartition(t, st, pName)
		}
	})
	require.NoError(t, st.EnsureAlertPartitions(ctx, month0, 3))

	const (
		monitorCount   = 100
		channelCount   = 100
		alertsPerMonth = 6000
		otherWorkspace = "qp-other-ws"
		pageSize       = 25
	)

	require.NoError(t, st.EnsureWorkspace(ctx, workspace.Default))
	require.NoError(t, st.EnsureWorkspace(ctx, workspace.ID(otherWorkspace)))

	monitorIDs := make([]int64, monitorCount)
	ruleIDs := make([]int64, monitorCount)
	for i := range monitorIDs {
		wsCtx := ctx
		if i%4 == 0 {
			wsCtx = workspace.With(ctx, workspace.ID(otherWorkspace))
		}
		netName := "testnet"
		if i%20 == 0 {
			netName = "mainnet"
		}
		m := &Monitor{
			Name:        fmt.Sprintf("qp-mon-%03d", i),
			ContractIDs: []string{fmt.Sprintf("CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCY%02d", i%20)},
			Tags:        []string{fmt.Sprintf("team-%d", i%5), "production"},
			Network:     netName,
			Enabled:     i%5 != 0,
		}
		require.NoError(t, st.CreateMonitor(wsCtx, m))
		r := &Rule{
			MonitorID: m.ID,
			Type:      "event_emitted",
			Severity:  SeverityWarning,
			Params:    json.RawMessage(`{}`),
			Enabled:   true,
		}
		require.NoError(t, st.CreateRule(wsCtx, r))
		monitorIDs[i] = m.ID
		ruleIDs[i] = r.ID
	}

	// Bulk-insert additional monitors and channels so cursor-paged listings on
	// monitors and channels have enough pages that an index scan with LIMIT is
	// decisively cheaper than a full-table sequential scan + sort.
	_, err := st.pool.Exec(ctx, `
		INSERT INTO monitors (name, contract_ids, tags, enabled, priority, workspace_id, network)
		SELECT
			'qp-bulk-mon-' || g,
			'["CCONTRACT"]'::jsonb,
			ARRAY['bulk'],
			true,
			'normal',
			CASE WHEN g % 4 = 0 THEN $1 ELSE 'default' END,
			CASE WHEN g % 10 = 0 THEN 'mainnet' ELSE 'testnet' END
		FROM generate_series(1, 500) AS g`, otherWorkspace)
	require.NoError(t, err)

	channelIDs := make([]int64, channelCount)
	for i := range channelIDs {
		wsCtx := ctx
		if i%4 == 0 {
			wsCtx = workspace.With(ctx, workspace.ID(otherWorkspace))
		}
		chType := "webhook"
		if i%2 == 1 {
			chType = "slack"
		}
		ch := &Channel{
			Name:    fmt.Sprintf("qp-chan-%03d", i),
			Type:    chType,
			Config:  json.RawMessage(`{"url":"https://example.invalid/hook"}`),
			Enabled: true,
		}
		require.NoError(t, st.CreateChannel(wsCtx, ch))
		channelIDs[i] = ch.ID
	}

	_, err = st.pool.Exec(ctx, `
		INSERT INTO channels (name, type, config, enabled, digest_mode, digest_window_seconds, timeout, workspace_id)
		SELECT
			'qp-bulk-chan-' || g,
			'webhook',
			'{}',
			true,
			false,
			0,
			10,
			CASE WHEN g % 4 = 0 THEN $1 ELSE 'default' END
		FROM generate_series(1, 500) AS g`, otherWorkspace)
	require.NoError(t, err)

	// Seed alerts across all three monthly partitions (2025-01, 2025-02,
	// 2025-03). Each row's timestamp stays strictly within its 28-day month
	// window (max offset 2,000,000s ~= 23.1 days).
	seedMonthAlerts := func(base time.Time, prefix string, count int) {
		_, err := st.pool.Exec(ctx, `
			INSERT INTO alerts (
				monitor_id, rule_id, event_id, payload, ledger, created_at,
				workspace_id, network, severity, acknowledged_at, acknowledged_by
			)
			SELECT
				($1::bigint[])[g % $3 + 1],
				($2::bigint[])[g % $3 + 1],
				$4 || '-' || g,
				json_build_object('contract_id', 'CCONTRACT_' || (g % 50))::jsonb,
				100000 + g,
				$5::timestamptz + make_interval(secs => (g * 300) % 2000000),
				CASE WHEN ((g - 1) / $3) % 4 = 0 THEN $6 ELSE 'default' END,
				CASE WHEN (g % 20) = 0 THEN 'mainnet' ELSE 'testnet' END,
				CASE WHEN (g % 5) = 0 THEN 'critical' WHEN (g % 5) = 1 THEN 'warning' ELSE 'info' END,
				CASE WHEN (g % 2) = 0 THEN $5::timestamptz + make_interval(secs => ((g * 300) % 2000000) + 60) ELSE NULL END,
				CASE WHEN (g % 2) = 0 THEN 'operator-token' ELSE '' END
			FROM generate_series(1, $7) AS g`,
			monitorIDs, ruleIDs, monitorCount, prefix, base, otherWorkspace, count)
		require.NoError(t, err)
	}

	seedMonthAlerts(month0, "ev-2025-01", alertsPerMonth)
	seedMonthAlerts(month1, "ev-2025-02", alertsPerMonth)
	seedMonthAlerts(month2, "ev-2025-03", alertsPerMonth)

	// Seed delivery attempts across channels and alerts.
	_, err = st.pool.Exec(ctx, `
		INSERT INTO delivery_attempts (alert_id, alert_created_at, channel_id, status, attempted_at)
		SELECT
			a.id,
			a.created_at,
			($1::bigint[])[a.id % $2 + 1],
			CASE WHEN a.id % 4 = 0 THEN $3::text ELSE $4::text END,
			a.created_at + interval '2 seconds'
		FROM alerts a`,
		channelIDs, channelCount, DeliveryStatusFailed, DeliveryStatusSuccess)
	require.NoError(t, err)

	// Seed audit log entries so cursor pagination and target/time filters have
	// enough rows to make sequential scans measurably worse than index scans.
	nowUTC := time.Now().UTC()
	_, err = st.pool.Exec(ctx, `
		INSERT INTO audit_log (actor, action, target_type, target_id, diff, created_at)
		SELECT
			CASE WHEN g % 3 = 0 THEN 'ci-bot' ELSE 'operator' END,
			'monitor.update',
			CASE WHEN g % 4 = 0 THEN 'channel' ELSE 'monitor' END,
			($1::bigint[])[g % $2 + 1],
			'{"enabled":true}'::jsonb,
			$3::timestamptz - make_interval(mins => g)
		FROM generate_series(1, 3000) AS g`,
		monitorIDs, monitorCount, nowUTC)
	require.NoError(t, err)

	// Run VACUUM ANALYZE so both pg_statistic histograms and visibility maps
	// (enabling Index Only Scans) reflect all seeded partitions and tables.
	_, err = st.pool.Exec(ctx, `VACUUM ANALYZE`)
	require.NoError(t, err)

	dataRels := dataBearingRelations(t, st)
	require.True(t, dataRels[part0], "expected %s to hold rows after ANALYZE", part0)
	require.True(t, dataRels[part1], "expected %s to hold rows after ANALYZE", part1)
	require.True(t, dataRels[part2], "expected %s to hold rows after ANALYZE", part2)
	require.True(t, dataRels["delivery_attempts"], "expected delivery_attempts to hold rows after ANALYZE")

	targetMonitorID := monitorIDs[1] // belongs to default workspace
	targetRuleID := ruleIDs[1]
	targetChannelID := channelIDs[1]

	var cursorAlertID int64
	require.NoError(t, st.pool.QueryRow(ctx,
		`SELECT id FROM alerts WHERE workspace_id = $1 AND monitor_id = $2 ORDER BY created_at DESC, id DESC OFFSET 10 LIMIT 1`,
		workspace.Default, targetMonitorID).Scan(&cursorAlertID))

	var cursorMonitorID int64
	require.NoError(t, st.pool.QueryRow(ctx,
		`SELECT id FROM monitors WHERE workspace_id = $1 AND deleted_at IS NULL ORDER BY id DESC OFFSET 10 LIMIT 1`,
		workspace.Default).Scan(&cursorMonitorID))

	var cursorChannelID int64
	require.NoError(t, st.pool.QueryRow(ctx,
		`SELECT id FROM channels WHERE workspace_id = $1 ORDER BY id DESC OFFSET 10 LIMIT 1`,
		workspace.Default).Scan(&cursorChannelID))

	var cursorAuditID int64
	require.NoError(t, st.pool.QueryRow(ctx,
		`SELECT id FROM audit_log ORDER BY id DESC OFFSET 20 LIMIT 1`).Scan(&cursorAuditID))

	// 1. Alerts listing with each individual filter and combined filters.
	t.Run("alerts listing filters", func(t *testing.T) {
		cases := []struct {
			name   string
			reason string
			filter AlertFilter
			tokens []string
		}{
			{
				name:   "workspace default feed",
				reason: "Unfiltered workspace feed must descend per-partition (workspace_id, created_at DESC, id DESC) or created_at index without sequential scans",
				filter: AlertFilter{Limit: pageSize},
				tokens: []string{"workspace_id_created_at_id_idx", "created_at"},
			},
			{
				name:   "monitor_id filter",
				reason: "Monitor filter must use alerts_monitor_created_idx on each active partition",
				filter: AlertFilter{MonitorID: targetMonitorID, Limit: pageSize},
				tokens: []string{"monitor_id_created_at_id_idx"},
			},
			{
				name:   "rule_id filter",
				reason: "Rule filter must use alerts_rule_created_idx on each active partition",
				filter: AlertFilter{RuleID: targetRuleID, Limit: pageSize},
				tokens: []string{"rule_id_created_at_id_idx"},
			},
			{
				name:   "contract_id filter",
				reason: "Contract filter on payload->>'contract_id' must use expression index alerts_contract_created_idx",
				filter: AlertFilter{ContractID: "CCONTRACT_1", Limit: pageSize},
				tokens: []string{"created_at_id_idx"},
			},
			{
				name:   "network filter",
				reason: "Network filter must use alerts_network_created_idx or workspace created_at index without sequential scan",
				filter: AlertFilter{Network: "mainnet", Limit: pageSize},
				tokens: []string{"network_created_at_id_idx", "workspace_id_created_at_id_idx"},
			},
			{
				name:   "acknowledged filter (acknowledged)",
				reason: "Acknowledged filter must walk workspace/created_at index without full partition sequential scan",
				filter: AlertFilter{Acknowledged: "acknowledged", Limit: pageSize},
				tokens: []string{"workspace_id_created_at_id_idx", "created_at"},
			},
			{
				name:   "acknowledged filter (unacknowledged)",
				reason: "Unacknowledged filter must walk workspace/created_at index without full partition sequential scan",
				filter: AlertFilter{Acknowledged: "unacknowledged", Limit: pageSize},
				tokens: []string{"workspace_id_created_at_id_idx", "created_at"},
			},
			{
				name:   "time range filter",
				reason: "Time-bounded feed must use created_at or workspace+created_at index without sequential scan",
				filter: AlertFilter{
					From:  month1.Add(2 * time.Hour),
					To:    month1.Add(18 * time.Hour),
					Limit: pageSize,
				},
				tokens: []string{"workspace_id_created_at_id_idx", "created_at"},
			},
			{
				name:   "combined monitor_id and rule_id filter",
				reason: "Combined monitor + rule filter must use composite alerts_monitor_rule_created_idx or single-column composite index",
				filter: AlertFilter{MonitorID: targetMonitorID, RuleID: targetRuleID, Limit: pageSize},
				tokens: []string{"monitor_id_rule_id_created_at_id_idx", "monitor_id_created_at_id_idx", "rule_id_created_at_id_idx"},
			},
			{
				name:   "combined monitor_id + acknowledged + time range",
				reason: "Multi-predicate operator filter must use monitor/time index and avoid sequential scans",
				filter: AlertFilter{
					MonitorID:    targetMonitorID,
					Acknowledged: "unacknowledged",
					From:         month0,
					To:           month2.Add(20 * 24 * time.Hour),
					Limit:        pageSize,
				},
				tokens: []string{"monitor_id_created_at_id_idx", "created_at"},
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				q, args := buildAlertQuery(ctx, tc.filter)
				assertQueryPlan(t, st, QueryPlanExpectation{
					Name:                   tc.name,
					Reason:                 tc.reason,
					Query:                  q,
					Args:                   args,
					RequireIndexAccess:     true,
					ExpectedIndexTokens:    tc.tokens,
					ForbidSeqScanRelations: dataRels,
				})
			})
		}
	})

	// 2. Partition pruning on time-bounded alert queries.
	t.Run("partition pruning on time-bounded queries", func(t *testing.T) {
		t.Run("single-month window touches only target monthly partition", func(t *testing.T) {
			q, args := buildAlertQuery(ctx, AlertFilter{
				MonitorID: targetMonitorID,
				From:      month1.Add(2 * 24 * time.Hour),
				To:        month1.Add(15 * 24 * time.Hour),
				Limit:     pageSize,
			})
			assertQueryPlan(t, st, QueryPlanExpectation{
				Name:                   "single-month bounded alerts query (2025-02)",
				Reason:                 "A query bounded within 2025-02 must scan only alerts_2025_02 and prune alerts_2025_01 and alerts_2025_03",
				Query:                  q,
				Args:                   args,
				RequireIndexAccess:     true,
				ExpectedIndexTokens:    []string{"monitor_id_created_at_id_idx", "workspace_id_created_at_id_idx", "created_at"},
				ForbidSeqScanRelations: dataRels,
				ExpectedPartitions:     []string{part1},
				PrunedPartitions:       []string{part0, part2},
			})
		})

		t.Run("two-month window touches only the two overlapping partitions and prunes the third", func(t *testing.T) {
			q, args := buildAlertQuery(ctx, AlertFilter{
				MonitorID: targetMonitorID,
				From:      month1.Add(10 * 24 * time.Hour),
				To:        month2.Add(10 * 24 * time.Hour),
				Limit:     pageSize,
			})
			assertQueryPlan(t, st, QueryPlanExpectation{
				Name:                   "two-month bounded alerts query (2025-02..2025-03)",
				Reason:                 "A query spanning 2025-02 through 2025-03 must prune alerts_2025_01",
				Query:                  q,
				Args:                   args,
				RequireIndexAccess:     true,
				ExpectedIndexTokens:    []string{"monitor_id_created_at_id_idx", "workspace_id_created_at_id_idx", "created_at"},
				ForbidSeqScanRelations: dataRels,
				ExpectedPartitions:     []string{part1, part2},
				PrunedPartitions:       []string{part0},
			})
		})
	})

	// 3. Alert statistics queries.
	t.Run("alert statistics queries", func(t *testing.T) {
		t.Run("GetStats 24h workspace alert counter", func(t *testing.T) {
			// Window inside 2025-03 (last 12h of the 23-day seeded range) so
			// out-of-range partitions (2025-01, 2025-02) are pruned and the
			// active partition uses an index scan over the bounded range.
			q := `SELECT count(*) FROM alerts WHERE workspace_id = $1 AND created_at >= $2 AND created_at < $3`
			args := []any{workspace.Default, month2.Add(22*24*time.Hour + 12*time.Hour), month2.Add(23 * 24 * time.Hour)}
			assertQueryPlan(t, st, QueryPlanExpectation{
				Name:                   "GetStats 24h alert counter",
				Reason:                 "24h headline counter must prune historical partitions outside the window and use index scan on the active partition",
				Query:                  q,
				Args:                   args,
				RequireIndexAccess:     true,
				ExpectedIndexTokens:    []string{"workspace_id_created_at_id_idx", "created_at"},
				ForbidSeqScanRelations: dataRels,
				ExpectedPartitions:     []string{part2},
				PrunedPartitions:       []string{part0, part1},
			})
		})

		t.Run("GetMonitorStats alert and delivery subqueries", func(t *testing.T) {
			q := `
				SELECT
					(SELECT count(*) FROM monitors WHERE id = $1 AND workspace_id = $2) > 0,
					(SELECT count(*) FROM alerts WHERE monitor_id = $1 AND workspace_id = $2),
					(SELECT count(*) FROM alerts WHERE monitor_id = $1 AND workspace_id = $2 AND created_at > now() - interval '24 hours'),
					(SELECT count(*) FROM alerts WHERE monitor_id = $1 AND workspace_id = $2 AND created_at > now() - interval '7 days'),
					(SELECT max(created_at) FROM alerts WHERE monitor_id = $1 AND workspace_id = $2)`
			args := []any{targetMonitorID, workspace.Default}
			assertQueryPlan(t, st, QueryPlanExpectation{
				Name:                   "GetMonitorStats alert subqueries",
				Reason:                 "Per-monitor alert counts and max(created_at) must use alerts_monitor_created_idx without sequential scans",
				Query:                  q,
				Args:                   args,
				RequireIndexAccess:     true,
				ExpectedIndexTokens:    []string{"monitor_id_created_at_id_idx"},
				ForbidSeqScanRelations: dataRels,
			})
		})

		t.Run("GetMonitorStats per-rule match counts", func(t *testing.T) {
			q := `
				SELECT r.id, r.type, count(a.id)
				FROM rules r
				LEFT JOIN alerts a ON a.rule_id = r.id
				WHERE r.monitor_id = $1
				GROUP BY r.id, r.type
				ORDER BY r.id`
			assertQueryPlan(t, st, QueryPlanExpectation{
				Name:                   "GetMonitorStats per-rule counts",
				Reason:                 "Per-rule alert count join must use rules_monitor_id_idx and alerts_rule_created_idx",
				Query:                  q,
				Args:                   []any{targetMonitorID},
				RequireIndexAccess:     true,
				ExpectedIndexTokens:    []string{"rule_id_created_at_id_idx", "rules_monitor_id_idx"},
				ForbidSeqScanRelations: dataRels,
			})
		})
	})

	// 4. Delivery-attempt history and channel delivery statistics.
	t.Run("delivery attempt history and channel stats", func(t *testing.T) {
		t.Run("ListDeliveryAttempts unfiltered", func(t *testing.T) {
			q, args := buildDeliveryAttemptsQuery(cursorAlertID, "", workspace.Default)
			assertQueryPlan(t, st, QueryPlanExpectation{
				Name:                   "ListDeliveryAttempts unfiltered",
				Reason:                 "Per-alert delivery attempt history must use delivery_attempts_alert_idx",
				Query:                  q,
				Args:                   args,
				RequireIndexAccess:     true,
				ExpectedIndexTokens:    []string{"delivery_attempts_alert_idx"},
				ForbidSeqScanRelations: dataRels,
			})
		})

		t.Run("ListDeliveryAttempts status-filtered", func(t *testing.T) {
			q, args := buildDeliveryAttemptsQuery(cursorAlertID, DeliveryStatusFailed, workspace.Default)
			assertQueryPlan(t, st, QueryPlanExpectation{
				Name:                   "ListDeliveryAttempts status-filtered",
				Reason:                 "Status-filtered delivery attempts for an alert must use delivery_attempts_alert_idx",
				Query:                  q,
				Args:                   args,
				RequireIndexAccess:     true,
				ExpectedIndexTokens:    []string{"delivery_attempts_alert_idx"},
				ForbidSeqScanRelations: dataRels,
			})
		})

		t.Run("GetChannelDeliveryStats windowed aggregate", func(t *testing.T) {
			q := `
				SELECT
					(SELECT count(*) FROM channels WHERE id = $1 AND workspace_id = $5) > 0,
					count(*),
					count(*) FILTER (WHERE da.status = $2),
					count(*) FILTER (WHERE da.status = $3),
					max(da.attempted_at) FILTER (WHERE da.status = $2),
					max(da.attempted_at) FILTER (WHERE da.status = $3)
				FROM delivery_attempts da
				WHERE da.channel_id = $1 AND da.attempted_at >= $4`
			args := []any{targetChannelID, DeliveryStatusSuccess, DeliveryStatusFailed, month1, workspace.Default}
			assertQueryPlan(t, st, QueryPlanExpectation{
				Name:                   "GetChannelDeliveryStats aggregate",
				Reason:                 "Per-channel delivery stats must use composite index delivery_attempts_channel_attempted_idx (migration 0032)",
				Query:                  q,
				Args:                   args,
				RequireIndexAccess:     true,
				ExpectedIndexTokens:    []string{"delivery_attempts_channel_attempted_idx"},
				ForbidSeqScanRelations: dataRels,
			})
		})
	})

	// 5. Cursor-paged reads across alerts, monitors, channels, and audit_log.
	t.Run("cursor-paged reads", func(t *testing.T) {
		t.Run("ListAlerts keyset cursor descending", func(t *testing.T) {
			q, args := buildAlertQuery(ctx, AlertFilter{
				MonitorID: targetMonitorID,
				AfterID:   cursorAlertID,
				Limit:     pageSize,
			})
			assertQueryPlan(t, st, QueryPlanExpectation{
				Name:                   "ListAlerts cursor descending",
				Reason:                 "Descending keyset cursor on monitor feed must use alerts_monitor_created_idx without sequential scan",
				Query:                  q,
				Args:                   args,
				RequireIndexAccess:     true,
				ExpectedIndexTokens:    []string{"monitor_id_created_at_id_idx"},
				ForbidSeqScanRelations: dataRels,
			})
		})

		t.Run("ListAlerts keyset cursor ascending", func(t *testing.T) {
			q, args := buildAlertQuery(ctx, AlertFilter{
				MonitorID: targetMonitorID,
				AfterID:   cursorAlertID,
				Sort:      "created_at_asc",
				Limit:     pageSize,
			})
			assertQueryPlan(t, st, QueryPlanExpectation{
				Name:                   "ListAlerts cursor ascending",
				Reason:                 "Ascending keyset cursor on monitor feed must scan alerts_monitor_created_idx backwards/forwards without sequential scan",
				Query:                  q,
				Args:                   args,
				RequireIndexAccess:     true,
				ExpectedIndexTokens:    []string{"monitor_id_created_at_id_idx"},
				ForbidSeqScanRelations: dataRels,
			})
		})

		t.Run("ListMonitorsPage cursor by id DESC", func(t *testing.T) {
			q := `SELECT id, name, contract_ids, tags, enabled, created_at, last_matched_at, priority, network, deleted_at, runbook_url, response_guidance
			      FROM monitors
			      WHERE workspace_id = $1 AND deleted_at IS NULL AND id < $2
			      ORDER BY id DESC LIMIT $3`
			assertQueryPlan(t, st, QueryPlanExpectation{
				Name:                "ListMonitorsPage cursor by id DESC",
				Reason:              "Monitor cursor pagination by id DESC uses monitors_workspace_idx (workspace_id, id DESC) or monitors_pkey",
				Query:               q,
				Args:                []any{workspace.Default, cursorMonitorID, 10},
				RequireIndexAccess:  true,
				ExpectedIndexTokens: []string{"monitors_workspace_idx", "monitors_pkey"},
			})
		})

		t.Run("ListMonitorsPage cursor by lower(name) ASC", func(t *testing.T) {
			q := `SELECT id, name, contract_ids, tags, enabled, created_at, last_matched_at, priority, network, deleted_at, runbook_url, response_guidance
			      FROM monitors
			      WHERE workspace_id = $1 AND deleted_at IS NULL
			        AND (lower(name), id) > (SELECT lower(name), id FROM monitors WHERE id = $2 AND workspace_id = $3 AND deleted_at IS NULL)
			      ORDER BY lower(name) ASC, id ASC LIMIT $4`
			assertQueryPlan(t, st, QueryPlanExpectation{
				Name:                "ListMonitorsPage cursor by name ASC",
				Reason:              "Monitor cursor pagination by name uses monitors_workspace_name_idx (workspace_id, lower(name), id)",
				Query:               q,
				Args:                []any{workspace.Default, cursorMonitorID, workspace.Default, 10},
				RequireIndexAccess:  true,
				ExpectedIndexTokens: []string{"monitors_workspace_name_idx", "monitors_pkey"},
			})
		})

		t.Run("ListChannelsPage cursor by id DESC", func(t *testing.T) {
			q := `SELECT id, name, type, config, enabled, created_at, digest_mode, digest_window_seconds, timeout,
			             consecutive_failures, consecutive_permanent_failures, last_error,
			             last_error_at, last_success_at, disabled_at
			      FROM channels
			      WHERE TRUE AND workspace_id = $1 AND id < $2
			      ORDER BY id DESC LIMIT $3`
			assertQueryPlan(t, st, QueryPlanExpectation{
				Name:                "ListChannelsPage cursor by id DESC",
				Reason:              "Channel cursor pagination by id DESC uses channels_workspace_idx (workspace_id, id DESC) or channels_pkey",
				Query:               q,
				Args:                []any{workspace.Default, cursorChannelID, 10},
				RequireIndexAccess:  true,
				ExpectedIndexTokens: []string{"channels_workspace_idx", "channels_pkey"},
			})
		})

		t.Run("ListAuditEntries cursor by id DESC", func(t *testing.T) {
			q := `SELECT id, actor, action, target_type, target_id, diff, created_at
			      FROM audit_log
			      WHERE TRUE AND id < $1
			      ORDER BY id DESC LIMIT $2`
			assertQueryPlan(t, st, QueryPlanExpectation{
				Name:                   "ListAuditEntries cursor by id DESC",
				Reason:                 "Audit log cursor pagination by id DESC uses audit_log_pkey without scanning full audit_log table",
				Query:                  q,
				Args:                   []any{cursorAuditID, 10},
				RequireIndexAccess:     true,
				ExpectedIndexTokens:    []string{"audit_log_pkey"},
				ForbidSeqScanRelations: map[string]bool{"audit_log": true},
			})
		})

		t.Run("ListAuditEntries target-filtered time range", func(t *testing.T) {
			q := `SELECT id, actor, action, target_type, target_id, diff, created_at
			      FROM audit_log
			      WHERE TRUE AND target_type = $1 AND target_id = $2 AND created_at >= $3
			      ORDER BY id DESC LIMIT $4`
			assertQueryPlan(t, st, QueryPlanExpectation{
				Name:                   "ListAuditEntries target filter",
				Reason:                 "Target-filtered audit log query uses audit_log_target_idx (target_type, target_id, created_at DESC) or audit_log_pkey",
				Query:                  q,
				Args:                   []any{"monitor", targetMonitorID, nowUTC.Add(-24 * time.Hour), pageSize},
				RequireIndexAccess:     true,
				ExpectedIndexTokens:    []string{"audit_log_target_idx", "audit_log_pkey"},
				ForbidSeqScanRelations: map[string]bool{"audit_log": true},
			})
		})
	})
}
