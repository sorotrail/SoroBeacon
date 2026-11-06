package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

func openTestSQLiteForWorkspaceDeletion(t *testing.T) (*SQLite, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "ws_delete.db")
	dbURL := "sqlite://" + dbPath
	require.NoError(t, Migrate(dbURL))
	st, err := NewSQLite(context.Background(), dbURL)
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	return st, dbURL
}

// TestWorkspaceDeletionCoversEverySchemaTable derives the table inventory
// directly from the migration files and the live migrated SQLite catalog,
// asserting that every table in the schema is either covered by workspace
// deletion or explicitly classified as an instance-global table with no
// workspace column or foreign key to a workspace-scoped table.
func TestWorkspaceDeletionCoversEverySchemaTable(t *testing.T) {
	st, _ := openTestSQLiteForWorkspaceDeletion(t)
	ctx := context.Background()

	covered := make(map[string]bool)
	for _, tbl := range WorkspaceDeletionCoveredTables() {
		covered[tbl] = true
	}
	global := InstanceGlobalTables()

	for tbl := range covered {
		assert.NotContains(t, global, tbl, "table %q cannot be both workspace-covered and instance-global", tbl)
	}

	// 1. Parse all CREATE TABLE statements from both Postgres and SQLite migrations.
	createTableRE := regexp.MustCompile(`(?i)\bCREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-zA-Z0-9_]+)\b`)
	transientPartitionTables := map[string]bool{
		"if":                   true, // dynamic PL/pgSQL format('CREATE TABLE IF NOT EXISTS %I ...')
		"alerts_default":       true,
		"alerts_unpartitioned": true,
	}
	postgresOnlyCoveredTables := map[string]bool{
		"alert_dedup": true, // Postgres partitioned alerts dedup table (0011_alerts_partitioning.up.sql)
	}

	for _, dir := range []string{"migrations", "migrations/sqlite"} {
		entries, err := migrationsFS.ReadDir(dir)
		require.NoError(t, err)
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".up.sql") {
				continue
			}
			raw, err := migrationsFS.ReadFile(dir + "/" + entry.Name())
			require.NoError(t, err)
			matches := createTableRE.FindAllStringSubmatch(string(raw), -1)
			for _, m := range matches {
				tbl := strings.ToLower(m[1])
				if transientPartitionTables[tbl] || strings.HasSuffix(tbl, "_new") {
					continue
				}
				if !covered[tbl] && global[tbl] == "" {
					t.Errorf("%s/%s creates table %q which is neither in WorkspaceDeletionCoveredTables() nor InstanceGlobalTables()",
						dir, entry.Name(), tbl)
				}
			}
		}
	}

	// 2. Inspect every live table in the migrated SQLite database.
	rows, err := st.db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name ASC`)
	require.NoError(t, err)
	var liveTables []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		liveTables = append(liveTables, name)
	}
	require.NoError(t, rows.Err())
	_ = rows.Close()

	for _, tbl := range liveTables {
		if !covered[tbl] && global[tbl] == "" {
			t.Fatalf("live migrated table %q is unclassified: add it to WorkspaceDeletionCoveredTables() (and DeleteWorkspace) or InstanceGlobalTables()", tbl)
		}
	}
	for tbl := range covered {
		if postgresOnlyCoveredTables[tbl] {
			continue
		}
		assert.Contains(t, liveTables, tbl, "covered table %q must exist in migrated schema", tbl)
	}

	// 3. Mechanically verify via PRAGMA table_info and PRAGMA foreign_key_list
	// that no table in InstanceGlobalTables() carries workspace_id or references
	// any workspace-covered table.
	for _, tbl := range liveTables {
		hasWorkspaceCol := false
		colRows, err := st.db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, tbl))
		require.NoError(t, err)
		for colRows.Next() {
			var (
				cid       int
				name      string
				ctype     string
				notNull   int
				dfltValue sql.NullString
				pk        int
			)
			require.NoError(t, colRows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &pk))
			if strings.EqualFold(name, "workspace_id") {
				hasWorkspaceCol = true
			}
		}
		require.NoError(t, colRows.Err())
		_ = colRows.Close()

		if hasWorkspaceCol && tbl != "audit_log" {
			assert.True(t, covered[tbl], "table %q has a workspace_id column and MUST be covered by workspace deletion", tbl)
		}

		fkRows, err := st.db.QueryContext(ctx, fmt.Sprintf(`PRAGMA foreign_key_list(%s)`, tbl))
		require.NoError(t, err)
		for fkRows.Next() {
			var (
				id       int
				seq      int
				refTable string
				fromCol  string
				toCol    string
				onUpdate string
				onDelete string
				match    string
			)
			require.NoError(t, fkRows.Scan(&id, &seq, &refTable, &fromCol, &toCol, &onUpdate, &onDelete, &match))
			refLower := strings.ToLower(refTable)
			if covered[refLower] {
				assert.True(t, covered[tbl],
					"table %q has a foreign key (%s -> %s.%s) referencing workspace-covered table %q and MUST be covered by workspace deletion",
					tbl, fromCol, refLower, toCol, refLower)
			}
		}
		require.NoError(t, fkRows.Err())
		_ = fkRows.Close()
	}

	// 4. Populate every covered table for a target workspace, run DeleteWorkspace,
	// and verify every covered table (except workspace_deletions, which stores the
	// completion tombstone) has 0 rows remaining.
	targetWS := workspace.ID("ws-schema-full")
	populateEveryCoveredTableForWorkspace(t, st, targetWS)

	for _, tbl := range WorkspaceDeletionCoveredTables() {
		if tbl == "workspace_deletions" || postgresOnlyCoveredTables[tbl] {
			continue
		}
		var n int64
		require.NoError(t, st.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s`, tbl)).Scan(&n))
		// Note: workspaces has 'default' plus 'ws-schema-full'.
		if tbl == "workspaces" {
			require.GreaterOrEqual(t, n, int64(2), "table %s must be populated before deletion", tbl)
		} else {
			require.GreaterOrEqual(t, n, int64(1), "table %s must be populated before deletion", tbl)
		}
	}

	preview, err := st.DeleteWorkspace(ctx, targetWS, WorkspaceDeletionOptions{DryRun: true})
	require.NoError(t, err)
	require.True(t, preview.DryRun)
	require.Equal(t, WorkspaceDeletionStatusPreview, preview.Status)
	require.GreaterOrEqual(t, preview.Counts.Monitors, int64(1))
	require.GreaterOrEqual(t, preview.Counts.Rules, int64(2))
	require.GreaterOrEqual(t, preview.Counts.Channels, int64(1))
	require.GreaterOrEqual(t, preview.Counts.Alerts, int64(1))
	require.Equal(t, int64(1), preview.Counts.Workspaces)

	res, err := st.DeleteWorkspace(ctx, targetWS, WorkspaceDeletionOptions{
		Confirm: string(targetWS),
		Actor:   "schema-test",
	})
	require.NoError(t, err)
	require.True(t, res.Completed)
	require.Equal(t, WorkspaceDeletionStatusCompleted, res.Status)
	require.Equal(t, preview.Counts, res.Counts)

	for _, tbl := range WorkspaceDeletionCoveredTables() {
		if postgresOnlyCoveredTables[tbl] {
			continue
		}
		var n int64
		require.NoError(t, st.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s`, tbl)).Scan(&n))
		switch tbl {
		case "workspace_deletions":
			assert.Equal(t, int64(1), n, "workspace_deletions must retain the completed tombstone")
		case "workspaces":
			assert.Equal(t, int64(1), n, "only the default workspace row should remain in workspaces")
		default:
			assert.Equal(t, int64(0), n, "table %s must have 0 rows after deleting the only populated tenant workspace", tbl)
		}
	}
}

func populateEveryCoveredTableForWorkspace(t *testing.T, st Store, ws workspace.ID) {
	t.Helper()
	sysCtx := workspace.WithSystem(context.Background())
	wsCtx := workspace.With(context.Background(), ws)

	require.NoError(t, st.EnsureWorkspace(sysCtx, ws))
	require.NoError(t, st.SetWorkspaceGuidance(sysCtx, ws, "https://runbooks.example.com/ws", "Page on-call"))

	ch := &Channel{
		Name:                "ch-" + string(ws),
		Type:                "webhook",
		Config:              json.RawMessage(`{"url":"https://hooks.example.com/secret-` + string(ws) + `"}`),
		Enabled:             true,
		DigestMode:          DigestModeWindow,
		DigestWindowSeconds: 60,
	}
	require.NoError(t, st.CreateChannel(wsCtx, ch))

	require.NoError(t, st.RotateChannelSigningKey(wsCtx, ch.ID, &ChannelSigningKey{
		KeyID:  "k1-" + string(ws),
		Secret: "sec-" + string(ws),
	}))
	require.NoError(t, st.PushDigestAlert(wsCtx, ch.ID, json.RawMessage(`{"id":1}`)))

	mon := &Monitor{
		Name:        "mon-" + string(ws),
		ContractIDs: []string{"CCONTRACT_" + strings.ToUpper(strings.ReplaceAll(string(ws), "-", "_"))},
		Enabled:     true,
		ChannelIDs:  []int64{ch.ID},
	}
	require.NoError(t, st.CreateMonitor(wsCtx, mon))
	require.NoError(t, st.SetMonitorChannels(wsCtx, mon.ID, []int64{ch.ID}))

	r1 := &Rule{
		MonitorID: mon.ID,
		Type:      "event_match",
		Params:    json.RawMessage(`{"event_name":"transfer"}`),
		Enabled:   true,
	}
	require.NoError(t, st.CreateRule(wsCtx, r1))

	r2 := &Rule{
		MonitorID: mon.ID,
		Type:      "absence_of_event",
		Params:    json.RawMessage(`{"event_name":"heartbeat","window_seconds":300}`),
		Enabled:   true,
	}
	require.NoError(t, st.CreateRule(wsCtx, r2))

	require.NoError(t, st.RecordAbsenceSeen(wsCtx, r2.ID, "heartbeat", time.Now().UTC()))
	require.NoError(t, st.CreateInhibition(wsCtx, &Inhibition{
		SourceRuleID:        r1.ID,
		TargetRuleID:        r2.ID,
		FiringWindowSeconds: 300,
	}))

	policy, err := st.SetEscalationPolicy(wsCtx, mon.ID, []EscalationStep{
		{DelaySeconds: 0, ChannelIDs: []int64{ch.ID}},
		{DelaySeconds: 60, ChannelIDs: []int64{ch.ID}},
	})
	require.NoError(t, err)

	mw := &MaintenanceWindow{
		Reason:    "maint-" + string(ws),
		Scope:     MaintenanceScopeMonitor,
		MonitorID: &mon.ID,
		StartAt:   time.Now().UTC().Add(-time.Hour),
		EndAt:     time.Now().UTC().Add(time.Hour),
	}
	require.NoError(t, st.CreateMaintenanceWindow(wsCtx, mw))

	require.NoError(t, st.UpsertBackfill(wsCtx, &Backfill{
		MonitorID:  mon.ID,
		FromLedger: 100,
		ToLedger:   200,
		NextLedger: 150,
	}))

	alert := &Alert{
		MonitorID: mon.ID,
		RuleID:    r1.ID,
		EventID:   "evt-1-" + string(ws),
		Payload:   json.RawMessage(`{"contract_id":"C1","event_name":"transfer"}`),
		Ledger:    123,
	}
	created, err := st.CreateAlert(wsCtx, alert)
	require.NoError(t, err)
	require.Equal(t, AlertCreated, created)

	require.NoError(t, st.RecordDeliveryAttempt(wsCtx, &DeliveryAttempt{
		AlertID:         alert.ID,
		ChannelID:       ch.ID,
		Status:          DeliveryStatusFailed,
		ResponseSnippet: "500 error",
	}))
	_, err = st.ReconcileChannelAcknowledgements(wsCtx, DefaultAckReconciliationConfig())
	require.NoError(t, err)
	require.NoError(t, st.CreateDeadLetter(wsCtx, &DeadLetter{
		AlertID:      alert.ID,
		ChannelID:    ch.ID,
		LastError:    "permanent failure",
		AttemptCount: 3,
		LastStatus:   500,
	}))
	require.NoError(t, st.ScheduleEscalation(wsCtx, alert.ID, policy.ID, json.RawMessage(`{"id":1}`), 1, time.Now().UTC().Add(time.Minute)))

	require.NoError(t, st.CreateSavedSearch(wsCtx, &SavedSearch{
		Name:      "search-" + string(ws),
		Filter:    SavedSearchFilter{MonitorID: mon.ID},
		IsDefault: true,
	}))
	require.NoError(t, st.CreateMonitorTemplate(wsCtx, &MonitorTemplate{
		Name:        "tmpl-" + string(ws),
		Description: "desc",
		Rules:       []MonitorTemplateRule{{Type: "event_match", Params: json.RawMessage(`{}`)}},
		ChannelIDs:  []int64{ch.ID},
	}))

	tok1 := &auth.Token{
		Name:   "tok1-" + string(ws),
		Hash:   "hash-1-" + string(ws),
		Prefix: "sba_1234",
		Scopes: []auth.Scope{auth.ScopeMonitorsRead},
	}
	require.NoError(t, st.CreateAPIToken(wsCtx, tok1))
	tok2 := &auth.Token{
		Name:   "tok2-" + string(ws),
		Hash:   "hash-2-" + string(ws),
		Prefix: "sba_5678",
		Scopes: []auth.Scope{auth.ScopeMonitorsRead},
	}
	require.NoError(t, st.RotateAPIToken(wsCtx, tok1.ID, tok2, time.Now().UTC().Add(time.Hour)))

	now := time.Now().UTC()
	pStart, pEnd := TruncateUsagePeriod(now)
	require.NoError(t, st.RecordWorkspaceUsageBatch(sysCtx, []WorkspaceUsageDelta{{
		WorkspaceID:     string(ws),
		PeriodStart:     pStart,
		PeriodEnd:       pEnd,
		AlertsRaised:    3,
		RuleEvaluations: 10,
		APIRequests:     5,
		Notifications: []WorkspaceNotificationUsage{
			{ChannelType: "webhook", Outcome: "ok", Count: 2},
		},
	}}))

	_, err = st.SetWorkspaceQuota(sysCtx, WorkspaceQuota{
		WorkspaceID:           string(ws),
		MaxMonitors:           10,
		MaxRules:              20,
		MaxChannels:           10,
		MaxSubscriptions:      5,
		QueryCostBudget:       100,
		QueryCostRefillPerSec: 10,
	})
	require.NoError(t, err)
	_, err = st.ConsumeWorkspaceQueryBudget(sysCtx, string(ws), 5.0, now)
	require.NoError(t, err)

	_, err = st.SetWorkspaceRetentionPolicy(sysCtx, SetWorkspaceRetentionInput{
		WorkspaceID:    string(ws),
		Retention:      30 * 24 * time.Hour,
		ConfirmShorten: true,
		Actor:          "schema-test",
	})
	require.NoError(t, err)

	outcome, _, err := st.ClaimIdempotencyKey(wsCtx, "idem-"+string(ws), "POST", "/monitors", "reqhash", now, now.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, IdempotencyClaimed, outcome)
}

func TestDeleteWorkspaceResumesInterruptedDeletionAndIsIdempotent(t *testing.T) {
	st, _ := openTestSQLiteForWorkspaceDeletion(t)
	ctx := context.Background()
	targetWS := workspace.ID("ws-resumable")
	wsCtx := workspace.With(ctx, targetWS)

	populateEveryCoveredTableForWorkspace(t, st, targetWS)

	// Add 24 more alerts (25 total) with delivery attempts and dead letters so
	// batched alert deletion spans multiple bounded passes.
	monitors, err := st.ListMonitors(wsCtx, false)
	require.NoError(t, err)
	require.Len(t, monitors, 1)
	rules, err := st.ListRules(wsCtx, monitors[0].ID, false)
	require.NoError(t, err)
	require.NotEmpty(t, rules)
	channels, err := st.ListChannels(wsCtx, false)
	require.NoError(t, err)
	require.Len(t, channels, 1)

	for i := 2; i <= 25; i++ {
		a := &Alert{
			MonitorID: monitors[0].ID,
			RuleID:    rules[0].ID,
			EventID:   fmt.Sprintf("evt-%d", i),
			Payload:   json.RawMessage(`{"contract_id":"C1","event_name":"transfer"}`),
			Ledger:    uint32(100 + i),
		}
		created, err := st.CreateAlert(wsCtx, a)
		require.NoError(t, err)
		require.Equal(t, AlertCreated, created)
		require.NoError(t, st.RecordDeliveryAttempt(wsCtx, &DeliveryAttempt{
			AlertID:         a.ID,
			ChannelID:       channels[0].ID,
			Status:          DeliveryStatusFailed,
			ResponseSnippet: "failed",
		}))
	}

	var stopHookCalls int
	var stoppedChannels []int64
	onStop := func(_ context.Context, wsID string, channelIDs []int64) {
		stopHookCalls++
		assert.Equal(t, string(targetWS), wsID)
		stoppedChannels = append([]int64{}, channelIDs...)
	}

	// Pass 1: Delete at most 2 batches of 7 alerts (14 alerts deleted, 11 remaining).
	res1, err := st.DeleteWorkspace(ctx, targetWS, WorkspaceDeletionOptions{
		Confirm:         string(targetWS),
		Actor:           "operator-1",
		BatchSize:       7,
		MaxAlertBatches: 2,
		OnStop:          onStop,
	})
	require.NoError(t, err)
	require.False(t, res1.Completed, "deletion must stop at MaxAlertBatches when alerts remain")
	require.Equal(t, WorkspaceDeletionStatusDeletingAlerts, res1.Status)
	require.Equal(t, int64(2), res1.BatchesCompleted)
	require.Equal(t, int64(14), res1.Counts.Alerts)
	require.Equal(t, int64(1), res1.MonitorsDisabled)
	require.Equal(t, int64(1), res1.ChannelsDisabled)
	require.Equal(t, 1, stopHookCalls)
	require.Equal(t, []int64{channels[0].ID}, stoppedChannels)

	// Verify monitors and channels were disabled immediately in Phase 1 before
	// alert deletion finished, and pending digests / escalations were cleared.
	enabledMonitors, err := st.ListMonitors(wsCtx, true)
	require.NoError(t, err)
	require.Empty(t, enabledMonitors, "poller must see 0 enabled monitors as soon as deletion starts")
	enabledChannels, err := st.ListChannels(wsCtx, true)
	require.NoError(t, err)
	require.Empty(t, enabledChannels, "dispatcher must see 0 enabled channels as soon as deletion starts")
	digests, err := st.ListDigestAlerts(wsCtx, channels[0].ID)
	require.NoError(t, err)
	require.Empty(t, digests, "pending digests must be cleared in Phase 1")
	dueEsc, err := st.DueEscalations(workspace.WithSystem(ctx), time.Now().UTC().Add(time.Hour), 100)
	require.NoError(t, err)
	require.Empty(t, dueEsc, "pending escalations must be cancelled in Phase 1")

	// Pass 2: Resume and run 1 more batch of 7 alerts (21 cumulative alerts deleted, 4 remaining).
	res2, err := st.DeleteWorkspace(ctx, targetWS, WorkspaceDeletionOptions{
		Confirm:         string(targetWS),
		Actor:           "operator-1",
		BatchSize:       7,
		MaxAlertBatches: 1,
	})
	require.NoError(t, err)
	require.False(t, res2.Completed)
	require.Equal(t, WorkspaceDeletionStatusDeletingAlerts, res2.Status)
	require.Equal(t, int64(3), res2.BatchesCompleted)
	require.Equal(t, int64(21), res2.Counts.Alerts)

	// Pass 3: Resume without MaxAlertBatches limit to finish the remaining 4 alerts and Phase 3 cleanup.
	res3, err := st.DeleteWorkspace(ctx, targetWS, WorkspaceDeletionOptions{
		Confirm:   string(targetWS),
		Actor:     "operator-1",
		BatchSize: 7,
	})
	require.NoError(t, err)
	require.True(t, res3.Completed)
	require.False(t, res3.IdempotentReplay)
	require.Equal(t, WorkspaceDeletionStatusCompleted, res3.Status)
	require.Equal(t, int64(4), res3.BatchesCompleted)
	require.Equal(t, int64(25), res3.Counts.Alerts)
	require.Equal(t, int64(25), res3.Counts.DeliveryAttempts)
	require.Equal(t, int64(1), res3.Counts.Monitors)
	require.Equal(t, int64(2), res3.Counts.Rules)
	require.Equal(t, int64(1), res3.Counts.Channels)
	require.Equal(t, int64(1), res3.Counts.Workspaces)
	require.NotNil(t, res3.CompletedAt)

	// Pass 4: Re-running a completed deletion is idempotent and returns the completed record.
	res4, err := st.DeleteWorkspace(ctx, targetWS, WorkspaceDeletionOptions{
		Confirm: string(targetWS),
		Actor:   "operator-1",
	})
	require.NoError(t, err)
	require.True(t, res4.Completed)
	require.True(t, res4.IdempotentReplay)
	require.Equal(t, res3.Counts, res4.Counts)
}

func TestDeleteWorkspaceMultiTenantIsolation(t *testing.T) {
	st, _ := openTestSQLiteForWorkspaceDeletion(t)
	ctx := context.Background()

	wsVictim := workspace.ID("tenant-victim")
	wsSurvivor := workspace.ID("tenant-survivor")

	populateEveryCoveredTableForWorkspace(t, st, wsVictim)
	populateEveryCoveredTableForWorkspace(t, st, wsSurvivor)

	survivorBefore, err := st.sqliteCountWorkspaceRows(ctx, string(wsSurvivor))
	require.NoError(t, err)
	require.Greater(t, survivorBefore.Total(), int64(15))

	res, err := st.DeleteWorkspace(ctx, wsVictim, WorkspaceDeletionOptions{
		Confirm:   string(wsVictim),
		Actor:     "admin",
		BatchSize: 2,
	})
	require.NoError(t, err)
	require.True(t, res.Completed)

	victimAfter, err := st.sqliteCountWorkspaceRows(ctx, string(wsVictim))
	require.NoError(t, err)
	require.Equal(t, int64(0), victimAfter.Total(), "every row belonging to victim workspace must be gone")

	survivorAfter, err := st.sqliteCountWorkspaceRows(ctx, string(wsSurvivor))
	require.NoError(t, err)
	require.Equal(t, survivorBefore, survivorAfter, "survivor workspace row counts must be 100%% untouched")

	// Also verify survivor data is readable through normal scoped store methods.
	survivorCtx := workspace.With(ctx, wsSurvivor)
	mons, err := st.ListMonitors(survivorCtx, false)
	require.NoError(t, err)
	require.Len(t, mons, 1)
	assert.Equal(t, "mon-tenant-survivor", mons[0].Name)

	chs, err := st.ListChannels(survivorCtx, false)
	require.NoError(t, err)
	require.Len(t, chs, 1)
	assert.Equal(t, "ch-tenant-survivor", chs[0].Name)

	alerts, err := st.ListAlerts(survivorCtx, AlertFilter{})
	require.NoError(t, err)
	require.Len(t, alerts, 1)

	searches, err := st.ListSavedSearches(survivorCtx)
	require.NoError(t, err)
	require.Len(t, searches, 1)

	tmpls, err := st.ListMonitorTemplates(survivorCtx)
	require.NoError(t, err)
	require.Len(t, tmpls, 1)

	tokens, err := st.ListAPITokens(survivorCtx)
	require.NoError(t, err)
	require.Len(t, tokens, 2)
}

func TestWorkspaceDeletionCoveredTablesSortedConsistency(t *testing.T) {
	tables := WorkspaceDeletionCoveredTables()
	seen := make(map[string]bool, len(tables))
	for _, tbl := range tables {
		require.False(t, seen[tbl], "duplicate table %q in WorkspaceDeletionCoveredTables", tbl)
		seen[tbl] = true
	}
	sorted := append([]string{}, tables...)
	sort.Strings(sorted)
	require.Len(t, sorted, len(tables))
}
