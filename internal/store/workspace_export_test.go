package store

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

var createTableRegex = regexp.MustCompile(`(?i)\bCREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-zA-Z0-9_]+)\s*\(`)

func discoverMigrationSchemaTables(t *testing.T) map[string]bool {
	t.Helper()
	discovered := map[string]bool{
		"schema_migrations": true,
	}
	err := fs.WalkDir(migrationsFS, "migrations", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(path, ".up.sql") {
			return nil
		}
		raw, err := fs.ReadFile(migrationsFS, path)
		if err != nil {
			return err
		}
		for _, m := range createTableRegex.FindAllStringSubmatch(string(raw), -1) {
			name := strings.ToLower(m[1])
			// SQLite table-rebuild migrations create temporary *_new tables and
			// immediately rename them back inside the same migration.
			if strings.HasSuffix(name, "_new") || strings.HasSuffix(name, "_old") {
				continue
			}
			discovered[name] = true
		}
		return nil
	})
	require.NoError(t, err)
	return discovered
}

func validateSchemaTableClassification(schemaTables map[string]bool, exported []string, nonExported map[string]string) []string {
	var problems []string
	exportedSet := make(map[string]bool, len(exported))
	for _, tbl := range exported {
		if exportedSet[tbl] {
			problems = append(problems, "duplicate table in WorkspaceExportTables: "+tbl)
		}
		exportedSet[tbl] = true
		if _, both := nonExported[tbl]; both {
			problems = append(problems, "table listed in both WorkspaceExportTables and NonExportedInstanceTables: "+tbl)
		}
		if !schemaTables[tbl] {
			problems = append(problems, "WorkspaceExportTables names non-existent schema table: "+tbl)
		}
	}
	for tbl, reason := range nonExported {
		if strings.TrimSpace(reason) == "" {
			problems = append(problems, "NonExportedInstanceTables has empty justification for: "+tbl)
		}
		if !schemaTables[tbl] {
			problems = append(problems, "NonExportedInstanceTables names non-existent schema table: "+tbl)
		}
	}
	for tbl := range schemaTables {
		_, isNonExported := nonExported[tbl]
		if !exportedSet[tbl] && !isNonExported {
			problems = append(problems, "unclassified schema table (must be added to WorkspaceExportTables or NonExportedInstanceTables): "+tbl)
		}
	}
	return problems
}

// TestWorkspaceExportCoversEverySchemaTable mechanically verifies that every
// table created across all Postgres and SQLite migrations (and present in a
// migrated SQLite database and transferTables) is explicitly classified as
// either workspace-exported or non-exported instance state.
func TestWorkspaceExportCoversEverySchemaTable(t *testing.T) {
	schemaTables := discoverMigrationSchemaTables(t)

	// Cross-check against a live migrated SQLite database's sqlite_master.
	dbURL := "sqlite://" + filepath.Join(t.TempDir(), "schema_check.db")
	require.NoError(t, Migrate(dbURL))
	st, err := NewSQLite(context.Background(), dbURL)
	require.NoError(t, err)
	t.Cleanup(st.Close)

	rows, err := st.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		schemaTables[strings.ToLower(name)] = true
	}
	require.NoError(t, rows.Err())

	// Cross-check against transferTables as well.
	for _, spec := range transferTables {
		schemaTables[spec.name] = true
	}

	problems := validateSchemaTableClassification(schemaTables, WorkspaceExportTables(), NonExportedInstanceTables)
	assert.Empty(t, problems, "every schema table must be classified for workspace export")

	// Mutation check: verify that an unclassified table is caught immediately.
	mutated := make(map[string]bool, len(schemaTables)+1)
	for k, v := range schemaTables {
		mutated[k] = v
	}
	mutated["unclassified_future_table"] = true
	mutProblems := validateSchemaTableClassification(mutated, WorkspaceExportTables(), NonExportedInstanceTables)
	require.NotEmpty(t, mutProblems)
	assert.Contains(t, mutProblems[0], "unclassified_future_table")
}

// testWorkspaceExportIsolation populates two workspaces ("alpha" and "beta")
// across every workspace-exported table in the schema and proves that exporting
// "alpha" includes all of "alpha"'s records, leaks zero "beta" records or
// substrings, honours include_secrets and include_alerts, and produces
// deterministic output.
func testWorkspaceExportIsolation(t *testing.T, newStore conformanceFactory) {
	ctx := context.Background()
	st := newStore(t)

	key := bytes.Repeat([]byte{0x42}, 32)
	cipher, err := NewAESGCMCipher(key)
	require.NoError(t, err)
	st.setCipher(cipher)
	t.Cleanup(func() { st.setCipher(nil) })

	wsAlpha := workspace.ID("alpha")
	wsBeta := workspace.ID("beta")
	alphaCtx := workspace.With(ctx, wsAlpha)
	betaCtx := workspace.With(ctx, wsBeta)

	baseTime := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	seedTenant := func(wCtx context.Context, ws workspace.ID, prefix string) {
		require.NoError(t, st.EnsureWorkspace(wCtx, ws))
		require.NoError(t, st.SetWorkspaceGuidance(wCtx, ws,
			"https://runbooks.example.com/"+prefix+"-default",
			prefix+"-workspace-guidance"))

		_, err := st.SetWorkspaceQuota(wCtx, WorkspaceQuota{
			WorkspaceID:           string(ws),
			MaxMonitors:           50,
			MaxRules:              100,
			MaxChannels:           25,
			MaxSubscriptions:      10,
			QueryCostBudget:       500,
			QueryCostRefillPerSec: 10,
		})
		require.NoError(t, err)

		dec, err := st.ConsumeWorkspaceQueryBudget(wCtx, string(ws), 25, baseTime)
		require.NoError(t, err)
		require.True(t, dec.Allowed)

		_, err = st.SetWorkspaceRetentionPolicy(wCtx, SetWorkspaceRetentionInput{
			WorkspaceID:    string(ws),
			Retention:      30 * 24 * time.Hour,
			ConfirmShorten: true,
			Actor:          prefix + "-admin",
		})
		require.NoError(t, err)

		ch1 := &Channel{
			Name:                prefix + "-webhook-primary",
			Type:                "webhook",
			Config:              json.RawMessage(`{"url":"https://hooks.example.com/` + prefix + `-secret-hook","secret":"` + prefix + `-top-secret-hmac","headers":{"Authorization":"Bearer ` + prefix + `-header-token"},"template":"` + prefix + `-tpl"}`),
			Enabled:             true,
			DigestMode:          DigestModeWindow,
			DigestWindowSeconds: 120,
			MinSeverity:         SeverityWarning,
			Timeout:             10 * time.Second,
		}
		require.NoError(t, st.CreateChannel(wCtx, ch1))

		sk1 := &ChannelSigningKey{
			ChannelID: ch1.ID,
			KeyID:     prefix + "-key-v1",
			Secret:    prefix + "-signing-secret-v1",
			IsCurrent: true,
			Status:    SigningKeyStatusActive,
		}
		require.NoError(t, st.CreateChannelSigningKey(wCtx, sk1))

		sk2 := &ChannelSigningKey{
			KeyID:  prefix + "-key-v2",
			Secret: prefix + "-signing-secret-v2",
		}
		require.NoError(t, st.RotateChannelSigningKey(wCtx, ch1.ID, sk2))

		require.NoError(t, st.PushDigestAlert(wCtx, ch1.ID, json.RawMessage(`{"summary":"`+prefix+`-digest-payload"}`)))

		monActive := &Monitor{
			Name:             prefix + "-active-monitor",
			ContractIDs:      []string{"C-" + strings.ToUpper(prefix) + "-1"},
			Tags:             []string{prefix + "-tag"},
			Enabled:          true,
			Priority:         PriorityHigh,
			Network:          "testnet-" + prefix,
			RunbookURL:       "https://runbooks.example.com/" + prefix + "-mon",
			ResponseGuidance: prefix + "-monitor-guidance",
		}
		require.NoError(t, st.CreateMonitor(wCtx, monActive))
		require.NoError(t, st.SetMonitorChannels(wCtx, monActive.ID, []int64{ch1.ID}))

		monDeleted := &Monitor{
			Name:        prefix + "-deleted-monitor",
			ContractIDs: []string{"C-" + strings.ToUpper(prefix) + "-DEL"},
			Enabled:     false,
			Priority:    PriorityLow,
			Network:     "testnet-" + prefix,
		}
		require.NoError(t, st.CreateMonitor(wCtx, monDeleted))
		require.NoError(t, st.SoftDeleteMonitor(wCtx, monDeleted.ID))

		rule1 := &Rule{
			MonitorID:        monActive.ID,
			Type:             "event_emitted",
			Params:           json.RawMessage(`{"event_name":"` + prefix + `_transfer"}`),
			Enabled:          true,
			Severity:         SeverityCritical,
			RunbookURL:       "https://runbooks.example.com/" + prefix + "-rule1",
			ResponseGuidance: prefix + "-rule1-guidance",
		}
		require.NoError(t, st.CreateRule(wCtx, rule1))

		rule2 := &Rule{
			MonitorID: monActive.ID,
			Type:      "absence_of_event",
			Params:    json.RawMessage(`{"event_name":"` + prefix + `_heartbeat","window_seconds":300}`),
			Enabled:   true,
			Severity:  SeverityWarning,
		}
		require.NoError(t, st.CreateRule(wCtx, rule2))

		require.NoError(t, st.RecordAbsenceSeen(wCtx, rule2.ID, prefix+"_heartbeat", baseTime))

		require.NoError(t, st.CreateInhibition(wCtx, &Inhibition{
			SourceRuleID:        rule1.ID,
			TargetRuleID:        rule2.ID,
			FiringWindowSeconds: 180,
		}))

		pol, err := st.SetEscalationPolicy(wCtx, monActive.ID, []EscalationStep{
			{Position: 0, DelaySeconds: 0, ChannelIDs: []int64{ch1.ID}},
			{Position: 1, DelaySeconds: 300, ChannelIDs: []int64{ch1.ID}},
		})
		require.NoError(t, err)

		monIDCopy := monActive.ID
		require.NoError(t, st.CreateMaintenanceWindow(wCtx, &MaintenanceWindow{
			Reason:    prefix + "-maintenance-window",
			Scope:     MaintenanceScopeMonitor,
			MonitorID: &monIDCopy,
			StartAt:   baseTime,
			EndAt:     baseTime.Add(2 * time.Hour),
		}))

		contractIDCopy := "C-" + strings.ToUpper(prefix) + "-1"
		require.NoError(t, st.CreateMaintenanceWindow(wCtx, &MaintenanceWindow{
			Reason:     prefix + "-contract-maintenance-window",
			Scope:      MaintenanceScopeContract,
			ContractID: &contractIDCopy,
			StartAt:    baseTime,
			EndAt:      baseTime.Add(2 * time.Hour),
		}))

		require.NoError(t, st.UpsertBackfill(wCtx, &Backfill{
			MonitorID:  monActive.ID,
			FromLedger: 100,
			ToLedger:   200,
			NextLedger: 150,
			Cursor:     prefix + "-cursor-150",
			Deliver:    true,
			Complete:   false,
		}))

		require.NoError(t, st.CreateSavedSearch(wCtx, &SavedSearch{
			Name: prefix + "-saved-search",
			Filter: SavedSearchFilter{
				MonitorID:  monActive.ID,
				ContractID: "C-" + strings.ToUpper(prefix) + "-1",
				Sort:       "created_at_desc",
			},
			IsDefault: true,
		}))

		require.NoError(t, st.CreateMonitorTemplate(wCtx, &MonitorTemplate{
			Name:        prefix + "-template",
			Description: prefix + " template description",
			Rules: []MonitorTemplateRule{
				{Type: "event_emitted", Params: json.RawMessage(`{"event_name":"` + prefix + `_evt"}`)},
			},
			ChannelIDs: []int64{ch1.ID},
			Parameters: []TemplateParameter{
				{Name: prefix + "_param", Required: true, Default: prefix + "_val"},
			},
		}))

		tok1 := &auth.Token{
			Name:   prefix + "-token-v1",
			Hash:   prefix + "-sha256-token-hash-1111111111111111",
			Prefix: "sb_" + prefix + "1",
			Scopes: []auth.Scope{auth.ScopeMonitorsRead, auth.ScopeAlertsRead},
		}
		require.NoError(t, st.CreateAPIToken(wCtx, tok1))
		tok2 := &auth.Token{
			Name:   prefix + "-token-v2",
			Hash:   prefix + "-sha256-token-hash-2222222222222222",
			Prefix: "sb_" + prefix + "2",
			Scopes: []auth.Scope{auth.ScopeMonitorsRead, auth.ScopeAlertsRead},
		}
		require.NoError(t, st.RotateAPIToken(wCtx, tok1.ID, tok2, baseTime.Add(time.Hour)))

		require.NoError(t, st.RecordWorkspaceUsageBatch(wCtx, []WorkspaceUsageDelta{
			{
				WorkspaceID:     string(ws),
				PeriodStart:     baseTime.Truncate(time.Hour),
				PeriodEnd:       baseTime.Truncate(time.Hour).Add(time.Hour),
				AlertsRaised:    3,
				RuleEvaluations: 10,
				APIRequests:     7,
				Notifications: []WorkspaceNotificationUsage{
					{ChannelType: "webhook", Outcome: "ok", Count: 2},
					{ChannelType: "webhook", Outcome: "error", Count: 1},
				},
			},
		}))

		a1 := &Alert{
			MonitorID: monActive.ID,
			RuleID:    rule1.ID,
			EventID:   prefix + "-ev-1",
			Payload:   json.RawMessage(`{"contract_id":"C-` + strings.ToUpper(prefix) + `-1","marker":"` + prefix + `-alert-1"}`),
			Severity:  SeverityCritical,
			Ledger:    101,
		}
		out1, err := st.CreateAlert(wCtx, a1)
		require.NoError(t, err)
		require.Equal(t, AlertCreated, out1)

		a2 := &Alert{
			MonitorID: monActive.ID,
			RuleID:    rule1.ID,
			EventID:   prefix + "-ev-2",
			Payload:   json.RawMessage(`{"contract_id":"C-` + strings.ToUpper(prefix) + `-1","marker":"` + prefix + `-alert-2"}`),
			Severity:  SeverityWarning,
			Ledger:    102,
		}
		out2, err := st.CreateAlert(wCtx, a2)
		require.NoError(t, err)
		require.Equal(t, AlertCreated, out2)

		aFed := &Alert{
			Federated:        true,
			FederationOrigin: "peer-" + prefix + ".example.com",
			SourceMonitorID:  900,
			SourceRuleID:     901,
			EventID:          prefix + "-fed-ev-1",
			Payload:          json.RawMessage(`{"marker":"` + prefix + `-fed-alert"}`),
			Severity:         SeverityWarning,
			Ledger:           103,
		}
		outFed, err := st.CreateAlert(wCtx, aFed)
		require.NoError(t, err)
		require.Equal(t, AlertCreated, outFed)

		require.NoError(t, st.RecordDeliveryAttempt(wCtx, &DeliveryAttempt{
			AlertID:         a1.ID,
			ChannelID:       ch1.ID,
			Status:          DeliveryStatusSuccess,
			ResponseSnippet: prefix + "-delivered-ok",
		}))
		require.NoError(t, st.RecordDeliveryAttempt(wCtx, &DeliveryAttempt{
			AlertID:         a2.ID,
			ChannelID:       ch1.ID,
			Status:          DeliveryStatusFailed,
			ResponseSnippet: prefix + "-delivery-failed",
		}))

		require.NoError(t, st.CreateDeadLetter(wCtx, &DeadLetter{
			AlertID:      a2.ID,
			ChannelID:    ch1.ID,
			LastError:    prefix + "-permanent-webhook-failure",
			AttemptCount: 4,
			LastStatus:   502,
		}))

		require.NoError(t, st.ScheduleEscalation(wCtx, a1.ID, pol.ID,
			json.RawMessage(`{"id":1,"marker":"`+prefix+`-esc"}`), 1, baseTime.Add(5*time.Minute)))

		_, err = st.ReconcileChannelAcknowledgements(wCtx, AckReconciliationConfig{
			LookbackWindow:    24 * time.Hour,
			CriticalAckWindow: time.Minute,
			WarningAckWindow:  time.Minute,
			MinDeliveries:     1,
			FlagThresholdRate: 0.5,
		})
		require.NoError(t, err)
	}

	seedTenant(alphaCtx, wsAlpha, "alpha")
	seedTenant(betaCtx, wsBeta, "beta")

	// Also create a global maintenance window to verify instance-wide rows do
	// not leak into a single-workspace export.
	require.NoError(t, st.CreateMaintenanceWindow(ctx, &MaintenanceWindow{
		Reason:  "global-instance-maintenance",
		Scope:   MaintenanceScopeGlobal,
		StartAt: baseTime,
		EndAt:   baseTime.Add(time.Hour),
	}))

	// --- Case 1: Default options (IncludeSecrets=false, IncludeAlerts=false) ---
	var defaultJSON strings.Builder
	defaultOpts := WorkspaceExportOptions{
		IncludeSecrets: false,
		IncludeAlerts:  false,
		ExportedAt:     baseTime,
	}
	require.NoError(t, WriteWorkspaceExportJSON(ctx, &defaultJSON, st, wsAlpha, defaultOpts))
	rawDefault := defaultJSON.String()

	// Zero "beta" records or substrings anywhere in alpha's export.
	assert.NotContains(t, rawDefault, "beta")
	assert.NotContains(t, rawDefault, "global-instance-maintenance")

	// Secrets must be redacted in default export.
	assert.NotContains(t, rawDefault, "alpha-secret-hook")
	assert.NotContains(t, rawDefault, "alpha-top-secret-hmac")
	assert.NotContains(t, rawDefault, "alpha-header-token")
	assert.NotContains(t, rawDefault, "alpha-signing-secret-v1")
	assert.NotContains(t, rawDefault, "alpha-signing-secret-v2")
	assert.NotContains(t, rawDefault, "alpha-sha256-token-hash")

	var defaultBundle WorkspaceExportBundle
	require.NoError(t, json.Unmarshal([]byte(rawDefault), &defaultBundle))

	assert.Equal(t, WorkspaceExportFormat, defaultBundle.Format)
	assert.Equal(t, WorkspaceExportVersion, defaultBundle.Version)
	assert.Equal(t, "alpha", defaultBundle.WorkspaceID)
	assert.False(t, defaultBundle.IncludeSecrets)
	assert.True(t, defaultBundle.SecretsRedacted)
	assert.False(t, defaultBundle.IncludeAlerts)
	assert.False(t, defaultBundle.AlertsIncluded)
	assert.Equal(t, WorkspaceExportConfigTables, defaultBundle.Manifest.Tables)

	// Verify all 20 config/state/rollup tables are populated for alpha.
	require.Len(t, defaultBundle.Workspaces, 1)
	assert.Equal(t, "alpha", defaultBundle.Workspaces[0].ID)
	assert.Equal(t, "alpha-workspace-guidance", defaultBundle.Workspaces[0].DefaultResponseGuidance)

	require.Len(t, defaultBundle.WorkspaceQuotas, 1)
	assert.Equal(t, int64(50), defaultBundle.WorkspaceQuotas[0].MaxMonitors)

	require.Len(t, defaultBundle.WorkspaceQueryBudgetState, 1)
	assert.Equal(t, "alpha", defaultBundle.WorkspaceQueryBudgetState[0].WorkspaceID)

	require.Len(t, defaultBundle.WorkspaceRetentionPolicies, 1)
	assert.Equal(t, "alpha", defaultBundle.WorkspaceRetentionPolicies[0].WorkspaceID)
	assert.Equal(t, int64(30*24*3600), defaultBundle.WorkspaceRetentionPolicies[0].RetentionSeconds)

	require.Len(t, defaultBundle.Channels, 1)
	assert.Equal(t, "alpha-webhook-primary", defaultBundle.Channels[0].Name)
	var redactedCfg map[string]any
	require.NoError(t, json.Unmarshal(defaultBundle.Channels[0].Config, &redactedCfg))
	assert.Equal(t, RedactedSecretPlaceholder, redactedCfg["url"])
	assert.Equal(t, RedactedSecretPlaceholder, redactedCfg["secret"])
	assert.Equal(t, "alpha-tpl", redactedCfg["template"], "non-secret config fields stay intact")
	hdrs, ok := redactedCfg["headers"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, RedactedSecretPlaceholder, hdrs["Authorization"])

	require.Len(t, defaultBundle.ChannelSigningKeys, 2)
	for _, k := range defaultBundle.ChannelSigningKeys {
		assert.Equal(t, RedactedSecretPlaceholder, k.Secret)
	}

	require.Len(t, defaultBundle.ChannelAckReconciliations, 1)
	assert.Equal(t, "alpha", defaultBundle.ChannelAckReconciliations[0].WorkspaceID)

	require.Len(t, defaultBundle.PendingDigests, 1)
	require.Len(t, defaultBundle.Monitors, 2, "both active and soft-deleted monitors are exported")
	assert.Equal(t, "alpha-active-monitor", defaultBundle.Monitors[0].Name)
	assert.Nil(t, defaultBundle.Monitors[0].DeletedAt)
	assert.Equal(t, "alpha-deleted-monitor", defaultBundle.Monitors[1].Name)
	assert.NotNil(t, defaultBundle.Monitors[1].DeletedAt)

	require.Len(t, defaultBundle.MonitorChannels, 1)
	require.Len(t, defaultBundle.Rules, 2)
	require.Len(t, defaultBundle.RuleAbsenceState, 1)
	require.Len(t, defaultBundle.AlertInhibitions, 1)
	require.Len(t, defaultBundle.EscalationPolicies, 1)
	require.Len(t, defaultBundle.EscalationSteps, 2)
	require.Len(t, defaultBundle.EscalationStepChannels, 2)
	require.Len(t, defaultBundle.MaintenanceWindows, 2)
	assert.Equal(t, "alpha-maintenance-window", defaultBundle.MaintenanceWindows[0].Reason)
	assert.Equal(t, MaintenanceScopeMonitor, defaultBundle.MaintenanceWindows[0].Scope)
	assert.Equal(t, "alpha-contract-maintenance-window", defaultBundle.MaintenanceWindows[1].Reason)
	assert.Equal(t, MaintenanceScopeContract, defaultBundle.MaintenanceWindows[1].Scope)
	require.Len(t, defaultBundle.Backfills, 1)
	assert.Equal(t, "alpha-cursor-150", defaultBundle.Backfills[0].Cursor)
	require.Len(t, defaultBundle.SavedSearches, 1)
	require.Len(t, defaultBundle.MonitorTemplates, 1)
	require.Len(t, defaultBundle.APITokens, 2)
	for _, tok := range defaultBundle.APITokens {
		assert.Equal(t, RedactedSecretPlaceholder, tok.TokenHash)
	}
	require.Len(t, defaultBundle.WorkspaceUsageRollups, 1)
	require.Len(t, defaultBundle.WorkspaceNotificationUsageRollups, 2)

	// Alert tables are empty when IncludeAlerts=false.
	assert.Empty(t, defaultBundle.Alerts)
	assert.Empty(t, defaultBundle.AlertDedup)
	assert.Empty(t, defaultBundle.DeliveryAttempts)
	assert.Empty(t, defaultBundle.DeadLetters)
	assert.Empty(t, defaultBundle.AlertEscalations)

	// --- Case 2: Full export (IncludeSecrets=true, IncludeAlerts=true, PageSize=1) ---
	fullOpts := WorkspaceExportOptions{
		IncludeSecrets: true,
		IncludeAlerts:  true,
		PageSize:       1, // Force multi-page keyset streaming across every alert table.
		ExportedAt:     baseTime,
	}
	var fullJSON strings.Builder
	require.NoError(t, WriteWorkspaceExportJSON(ctx, &fullJSON, st, wsAlpha, fullOpts))
	rawFull := fullJSON.String()

	assert.NotContains(t, rawFull, "beta")
	assert.NotContains(t, rawFull, "global-instance-maintenance")

	var fullBundle WorkspaceExportBundle
	require.NoError(t, json.Unmarshal([]byte(rawFull), &fullBundle))

	assert.True(t, fullBundle.IncludeSecrets)
	assert.False(t, fullBundle.SecretsRedacted)
	assert.True(t, fullBundle.IncludeAlerts)
	assert.True(t, fullBundle.AlertsIncluded)
	assert.Equal(t, WorkspaceExportTables(), fullBundle.Manifest.Tables)

	// Verify secrets are decrypted and present when IncludeSecrets=true.
	var cleartextCfg map[string]any
	require.NoError(t, json.Unmarshal(fullBundle.Channels[0].Config, &cleartextCfg))
	assert.Equal(t, "https://hooks.example.com/alpha-secret-hook", cleartextCfg["url"])
	assert.Equal(t, "alpha-top-secret-hmac", cleartextCfg["secret"])
	assert.Equal(t, "alpha-signing-secret-v1", fullBundle.ChannelSigningKeys[0].Secret)
	assert.Equal(t, "alpha-signing-secret-v2", fullBundle.ChannelSigningKeys[1].Secret)
	assert.Equal(t, "alpha-sha256-token-hash-1111111111111111", fullBundle.APITokens[0].TokenHash)
	assert.Equal(t, "alpha-sha256-token-hash-2222222222222222", fullBundle.APITokens[1].TokenHash)

	// Verify all 5 alert tables streamed across multiple 1-row pages in ascending order.
	require.Len(t, fullBundle.Alerts, 3)
	assert.Less(t, fullBundle.Alerts[0].ID, fullBundle.Alerts[1].ID)
	assert.Less(t, fullBundle.Alerts[1].ID, fullBundle.Alerts[2].ID)
	assert.True(t, fullBundle.Alerts[2].Federated)
	assert.Equal(t, "peer-alpha.example.com", fullBundle.Alerts[2].FederationOrigin)

	require.Len(t, fullBundle.AlertDedup, 2, "local alerts have dedup rows; federated alert has no local rule_id")
	assert.Less(t, fullBundle.AlertDedup[0].AlertID, fullBundle.AlertDedup[1].AlertID)

	require.Len(t, fullBundle.DeliveryAttempts, 2)
	assert.Less(t, fullBundle.DeliveryAttempts[0].ID, fullBundle.DeliveryAttempts[1].ID)

	require.Len(t, fullBundle.DeadLetters, 1)
	assert.Equal(t, "alpha-permanent-webhook-failure", fullBundle.DeadLetters[0].LastError)

	require.Len(t, fullBundle.AlertEscalations, 1)
	assert.Equal(t, fullBundle.Alerts[0].ID, fullBundle.AlertEscalations[0].AlertID)

	// Every one of the 27 exported tables must have non-zero count in Summary.
	for _, tbl := range WorkspaceExportTables() {
		assert.Greater(t, fullBundle.Summary.TableCounts[tbl], int64(0), "table %s should have at least 1 exported row", tbl)
	}

	// --- Case 3: Determinism across repeated JSON and NDJSON runs ---
	var fullJSON2 strings.Builder
	require.NoError(t, WriteWorkspaceExportJSON(ctx, &fullJSON2, st, wsAlpha, fullOpts))
	assert.Equal(t, rawFull, fullJSON2.String(), "repeated JSON exports must be byte-for-byte deterministic")

	var ndjson1, ndjson2 strings.Builder
	require.NoError(t, WriteWorkspaceExportNDJSON(ctx, &ndjson1, st, wsAlpha, fullOpts))
	require.NoError(t, WriteWorkspaceExportNDJSON(ctx, &ndjson2, st, wsAlpha, fullOpts))
	assert.Equal(t, ndjson1.String(), ndjson2.String(), "repeated NDJSON exports must be byte-for-byte deterministic")
	assert.NotContains(t, ndjson1.String(), "beta")
}
