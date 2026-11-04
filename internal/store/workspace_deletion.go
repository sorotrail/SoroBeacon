package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// WorkspaceDeletionCoveredTables returns the canonical list of all tables in
// the schema whose rows belong to a workspace (directly via workspace_id/id or
// transitively via foreign keys to monitors, rules, channels, or alerts) and
// are covered by DeleteWorkspace.
func WorkspaceDeletionCoveredTables() []string {
	return []string{
		"workspaces",
		"workspace_deletions",
		"workspace_quotas",
		"workspace_retention_policies",
		"workspace_query_budget_state",
		"workspace_usage_rollups",
		"workspace_notification_usage_rollups",
		"idempotency_keys",
		"api_tokens",
		"saved_searches",
		"monitor_templates",
		"channels",
		"channel_signing_keys",
		"channel_ack_reconciliations",
		"pending_digests",
		"monitors",
		"monitor_channels",
		"rules",
		"rule_absence_state",
		"alert_inhibitions",
		"escalation_policies",
		"escalation_steps",
		"escalation_step_channels",
		"maintenance_windows",
		"backfills",
		"alerts",
		"alert_dedup",
		"delivery_attempts",
		"dead_letters",
		"alert_escalations",
	}
}

// InstanceGlobalTables returns the tables in the schema that store instance-wide
// operational state rather than tenant-owned rows, along with the reason each
// is not deleted when a single workspace is removed.
func InstanceGlobalTables() map[string]string {
	return map[string]string{
		"schema_migrations":       "golang-migrate schema version bookkeeping",
		"ingest_state":            "legacy single-network poller ledger cursor",
		"ledger_hashes":           "legacy single-network reorg hash window",
		"network_ingest_state":    "per-network poller ledger cursor",
		"network_ledger_hashes":   "per-network reorg hash window",
		"contract_ingest_cursors": "shared per-contract ingest cursors (cleaned when unreferenced across all workspaces)",
		"retention_gaps":          "instance-wide ledger retention gap log",
		"event_archive":           "instance-wide raw Soroban ledger event archive",
		"audit_log":               "instance-wide append-only audit trail recording configuration and workspace lifecycle actions",
		"persisted_operations":    "instance-wide vetted GraphQL persisted query registry",
	}
}

// SafeDeleteWorkspace calls st.DeleteWorkspace while tolerating partial test
// fakes that embed a nil store.Store.
func SafeDeleteWorkspace(ctx context.Context, st Workspaces, id workspace.ID, opts WorkspaceDeletionOptions) (res *WorkspaceDeletionResult, err error) {
	if st == nil {
		return nil, ErrUnavailable
	}
	defer func() {
		if r := recover(); r != nil {
			res = nil
			err = ErrUnavailable
		}
	}()
	return st.DeleteWorkspace(ctx, id, opts)
}

// SafeGetWorkspaceDeletion calls st.GetWorkspaceDeletion while tolerating
// partial test fakes that embed a nil store.Store.
func SafeGetWorkspaceDeletion(ctx context.Context, st Workspaces, id workspace.ID) (res *WorkspaceDeletionResult, err error) {
	if st == nil {
		return nil, ErrNotFound
	}
	defer func() {
		if r := recover(); r != nil {
			res = nil
			err = ErrNotFound
		}
	}()
	return st.GetWorkspaceDeletion(ctx, id)
}

func normalizeWorkspaceDeleteBatchSize(n int) int {
	if n <= 0 {
		return DefaultWorkspaceDeleteBatchSize
	}
	if n > 10000 {
		return 10000
	}
	return n
}

func marshalDeletionCounts(c WorkspaceDeletionCounts) string {
	b, err := json.Marshal(c)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func unmarshalDeletionCounts(raw string) WorkspaceDeletionCounts {
	var c WorkspaceDeletionCounts
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &c)
	}
	return c
}

// GetWorkspaceDeletion returns the persisted deletion progress or completed
// tombstone for id on Postgres.
func (p *Postgres) GetWorkspaceDeletion(ctx context.Context, id workspace.ID) (*WorkspaceDeletionResult, error) {
	if !workspace.Valid(string(id)) {
		return nil, workspace.ErrInvalidID
	}
	ws := string(id)
	var (
		statusStr   string
		actor       string
		batchSize   int
		batches     int64
		lastAlertID int64
		alertsDel   int64
		delivDel    int64
		deadDel     int64
		escDel      int64
		dedupDel    int64
		monDel      int64
		rulesDel    int64
		chanDel     int64
		searchDel   int64
		tmplDel     int64
		tokDel      int64
		countsJSON  string
		startedAt   time.Time
		updatedAt   time.Time
		completedAt *time.Time
	)
	err := p.pool.QueryRow(ctx, `
		SELECT status, actor, batch_size, batches_completed, last_alert_id,
		       alerts_deleted, delivery_attempts_deleted, dead_letters_deleted,
		       alert_escalations_deleted, alert_dedup_deleted,
		       monitors_deleted, rules_deleted, channels_deleted,
		       saved_searches_deleted, monitor_templates_deleted, api_tokens_deleted,
		       counts_json, started_at, updated_at, completed_at
		FROM workspace_deletions
		WHERE workspace_id = $1`, ws,
	).Scan(
		&statusStr, &actor, &batchSize, &batches, &lastAlertID,
		&alertsDel, &delivDel, &deadDel, &escDel, &dedupDel,
		&monDel, &rulesDel, &chanDel, &searchDel, &tmplDel, &tokDel,
		&countsJSON, &startedAt, &updatedAt, &completedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get workspace deletion: %w", err)
	}
	counts := unmarshalDeletionCounts(countsJSON)
	counts.Alerts = alertsDel
	counts.DeliveryAttempts = delivDel
	counts.DeadLetters = deadDel
	counts.AlertEscalations = escDel
	counts.AlertDedup = dedupDel
	counts.Monitors = monDel
	counts.Rules = rulesDel
	counts.Channels = chanDel
	counts.SavedSearches = searchDel
	counts.MonitorTemplates = tmplDel
	counts.APITokens = tokDel

	sAt := startedAt.UTC()
	uAt := updatedAt.UTC()
	var cAt *time.Time
	if completedAt != nil {
		t := completedAt.UTC()
		cAt = &t
	}
	status := WorkspaceDeletionStatus(statusStr)
	return &WorkspaceDeletionResult{
		WorkspaceID:      ws,
		Status:           status,
		DryRun:           false,
		Completed:        status == WorkspaceDeletionStatusCompleted,
		Irreversible:     true,
		Actor:            actor,
		BatchSize:        batchSize,
		BatchesCompleted: batches,
		LastAlertID:      lastAlertID,
		Counts:           counts,
		StartedAt:        &sAt,
		UpdatedAt:        &uAt,
		CompletedAt:      cAt,
	}, nil
}

func (p *Postgres) pgCountWorkspaceRows(ctx context.Context, ws string) (WorkspaceDeletionCounts, error) {
	var c WorkspaceDeletionCounts
	queries := []struct {
		dest  *int64
		query string
	}{
		{&c.Workspaces, `SELECT COUNT(*) FROM workspaces WHERE id = $1`},
		{&c.Monitors, `SELECT COUNT(*) FROM monitors WHERE workspace_id = $1`},
		{&c.Rules, `SELECT COUNT(*) FROM rules WHERE monitor_id IN (SELECT id FROM monitors WHERE workspace_id = $1)`},
		{&c.RuleAbsenceState, `SELECT COUNT(*) FROM rule_absence_state WHERE rule_id IN (SELECT r.id FROM rules r JOIN monitors m ON m.id = r.monitor_id WHERE m.workspace_id = $1)`},
		{&c.AlertInhibitions, `SELECT COUNT(*) FROM alert_inhibitions WHERE source_rule_id IN (SELECT r.id FROM rules r JOIN monitors m ON m.id = r.monitor_id WHERE m.workspace_id = $1) OR target_rule_id IN (SELECT r.id FROM rules r JOIN monitors m ON m.id = r.monitor_id WHERE m.workspace_id = $1)`},
		{&c.Channels, `SELECT COUNT(*) FROM channels WHERE workspace_id = $1`},
		{&c.MonitorChannels, `SELECT COUNT(*) FROM monitor_channels WHERE monitor_id IN (SELECT id FROM monitors WHERE workspace_id = $1) OR channel_id IN (SELECT id FROM channels WHERE workspace_id = $1)`},
		{&c.ChannelSigningKeys, `SELECT COUNT(*) FROM channel_signing_keys WHERE workspace_id = $1 OR channel_id IN (SELECT id FROM channels WHERE workspace_id = $1)`},
		{&c.ChannelAckReconciliations, `SELECT COUNT(*) FROM channel_ack_reconciliations WHERE workspace_id = $1 OR channel_id IN (SELECT id FROM channels WHERE workspace_id = $1)`},
		{&c.PendingDigests, `SELECT COUNT(*) FROM pending_digests WHERE channel_id IN (SELECT id FROM channels WHERE workspace_id = $1)`},
		{&c.EscalationPolicies, `SELECT COUNT(*) FROM escalation_policies WHERE monitor_id IN (SELECT id FROM monitors WHERE workspace_id = $1)`},
		{&c.EscalationSteps, `SELECT COUNT(*) FROM escalation_steps WHERE policy_id IN (SELECT ep.id FROM escalation_policies ep JOIN monitors m ON m.id = ep.monitor_id WHERE m.workspace_id = $1)`},
		{&c.EscalationStepChannels, `SELECT COUNT(*) FROM escalation_step_channels WHERE step_id IN (SELECT es.id FROM escalation_steps es JOIN escalation_policies ep ON ep.id = es.policy_id JOIN monitors m ON m.id = ep.monitor_id WHERE m.workspace_id = $1) OR channel_id IN (SELECT id FROM channels WHERE workspace_id = $1)`},
		{&c.MaintenanceWindows, `SELECT COUNT(*) FROM maintenance_windows WHERE monitor_id IN (SELECT id FROM monitors WHERE workspace_id = $1)`},
		{&c.Backfills, `SELECT COUNT(*) FROM backfills WHERE monitor_id IN (SELECT id FROM monitors WHERE workspace_id = $1)`},
		{&c.Alerts, `SELECT COUNT(*) FROM alerts WHERE workspace_id = $1`},
		{&c.AlertDedup, `SELECT COUNT(*) FROM alert_dedup WHERE alert_id IN (SELECT id FROM alerts WHERE workspace_id = $1) OR rule_id IN (SELECT r.id FROM rules r JOIN monitors m ON m.id = r.monitor_id WHERE m.workspace_id = $1)`},
		{&c.DeliveryAttempts, `SELECT COUNT(*) FROM delivery_attempts WHERE alert_id IN (SELECT id FROM alerts WHERE workspace_id = $1) OR channel_id IN (SELECT id FROM channels WHERE workspace_id = $1)`},
		{&c.DeadLetters, `SELECT COUNT(*) FROM dead_letters WHERE alert_id IN (SELECT id FROM alerts WHERE workspace_id = $1) OR channel_id IN (SELECT id FROM channels WHERE workspace_id = $1)`},
		{&c.AlertEscalations, `SELECT COUNT(*) FROM alert_escalations WHERE alert_id IN (SELECT id FROM alerts WHERE workspace_id = $1) OR policy_id IN (SELECT ep.id FROM escalation_policies ep JOIN monitors m ON m.id = ep.monitor_id WHERE m.workspace_id = $1)`},
		{&c.SavedSearches, `SELECT COUNT(*) FROM saved_searches WHERE workspace_id = $1`},
		{&c.MonitorTemplates, `SELECT COUNT(*) FROM monitor_templates WHERE workspace_id = $1`},
		{&c.APITokens, `SELECT COUNT(*) FROM api_tokens WHERE workspace_id = $1`},
		{&c.WorkspaceUsageRollups, `SELECT COUNT(*) FROM workspace_usage_rollups WHERE workspace_id = $1`},
		{&c.WorkspaceNotificationUsageRollup, `SELECT COUNT(*) FROM workspace_notification_usage_rollups WHERE workspace_id = $1`},
		{&c.WorkspaceQuotas, `SELECT COUNT(*) FROM workspace_quotas WHERE workspace_id = $1`},
		{&c.WorkspaceQueryBudgetState, `SELECT COUNT(*) FROM workspace_query_budget_state WHERE workspace_id = $1`},
		{&c.IdempotencyKeys, `SELECT COUNT(*) FROM idempotency_keys WHERE workspace_id = $1`},
	}
	for _, q := range queries {
		if err := p.pool.QueryRow(ctx, q.query, ws).Scan(q.dest); err != nil {
			return WorkspaceDeletionCounts{}, fmt.Errorf("count workspace rows: %w", err)
		}
	}
	return c, nil
}

// DeleteWorkspace implements bounded, resumable deletion of a workspace and
// every row belonging to it on Postgres.
func (p *Postgres) DeleteWorkspace(ctx context.Context, id workspace.ID, opts WorkspaceDeletionOptions) (*WorkspaceDeletionResult, error) {
	if !workspace.Valid(string(id)) {
		return nil, workspace.ErrInvalidID
	}
	ws := string(id)
	batchSize := normalizeWorkspaceDeleteBatchSize(opts.BatchSize)

	currentCounts, err := p.pgCountWorkspaceRows(ctx, ws)
	if err != nil {
		return nil, err
	}

	if opts.DryRun {
		if currentCounts.Total() == 0 {
			return nil, ErrNotFound
		}
		return &WorkspaceDeletionResult{
			WorkspaceID:  ws,
			Status:       WorkspaceDeletionStatusPreview,
			DryRun:       true,
			Completed:    false,
			Irreversible: true,
			Actor:        opts.Actor,
			BatchSize:    batchSize,
			Counts:       currentCounts,
		}, nil
	}

	if err := workspace.ValidateDeletionConfirmation(id, opts.Confirm); err != nil {
		return nil, err
	}

	existing, existingErr := p.GetWorkspaceDeletion(ctx, id)
	if existingErr != nil && !errors.Is(existingErr, ErrNotFound) {
		return nil, existingErr
	}
	if existing != nil && existing.Completed && currentCounts.Total() == 0 {
		existing.IdempotentReplay = true
		return existing, nil
	}
	if existing == nil && currentCounts.Total() == 0 {
		return nil, ErrNotFound
	}

	// Phase 1: Stop evaluation and delivery first before removing rows.
	channelIDs, err := p.pgListWorkspaceChannelIDs(ctx, ws)
	if err != nil {
		return nil, err
	}
	resetCompleted := existing != nil && existing.Completed && currentCounts.Total() > 0
	monitorsDisabled, channelsDisabled, tokensRevoked, err := p.pgStopWorkspacePhase(ctx, ws, opts.Actor, batchSize, resetCompleted)
	if err != nil {
		return nil, err
	}
	if opts.OnStop != nil {
		opts.OnStop(ctx, ws, channelIDs)
	}

	// Phase 2: Bounded, resumable batched alert deletion.
	batchesRun := 0
	for {
		if opts.MaxAlertBatches > 0 && batchesRun >= opts.MaxAlertBatches {
			hasMore, err := p.pgHasRemainingAlerts(ctx, ws)
			if err != nil {
				return nil, err
			}
			if hasMore {
				res, err := p.GetWorkspaceDeletion(ctx, id)
				if err != nil {
					return nil, err
				}
				res.MonitorsDisabled = monitorsDisabled
				res.ChannelsDisabled = channelsDisabled
				res.TokensRevoked = tokensRevoked
				return res, nil
			}
			break
		}
		deletedInBatch, err := p.pgDeleteWorkspaceAlertBatch(ctx, ws, batchSize)
		if err != nil {
			return nil, err
		}
		if deletedInBatch == 0 {
			break
		}
		batchesRun++
	}

	// Phase 3: Delete all remaining configuration, state, usage, quota, token,
	// and workspace rows in foreign-key dependency order.
	if err := p.pgFinalizeWorkspaceDeletion(ctx, ws); err != nil {
		return nil, err
	}

	res, err := p.GetWorkspaceDeletion(ctx, id)
	if err != nil {
		return nil, err
	}
	res.MonitorsDisabled = monitorsDisabled
	res.ChannelsDisabled = channelsDisabled
	res.TokensRevoked = tokensRevoked
	return res, nil
}

func (p *Postgres) pgListWorkspaceChannelIDs(ctx context.Context, ws string) ([]int64, error) {
	rows, err := p.pool.Query(ctx, `SELECT id FROM channels WHERE workspace_id = $1 ORDER BY id ASC`, ws)
	if err != nil {
		return nil, fmt.Errorf("list workspace channel ids: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan workspace channel id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (p *Postgres) pgHasRemainingAlerts(ctx context.Context, ws string) (bool, error) {
	var exists bool
	err := p.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM alerts WHERE workspace_id = $1)`, ws).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check remaining workspace alerts: %w", err)
	}
	return exists, nil
}

func (p *Postgres) pgStopWorkspacePhase(ctx context.Context, ws, actor string, batchSize int, resetCompleted bool) (monitorsDisabled, channelsDisabled, tokensRevoked int64, err error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("begin workspace stop phase: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	now := time.Now().UTC()
	if resetCompleted {
		if _, err := tx.Exec(ctx, `DELETE FROM workspace_deletions WHERE workspace_id = $1`, ws); err != nil {
			return 0, 0, 0, fmt.Errorf("reset workspace deletion row: %w", err)
		}
	}

	var prevCountsJSON string
	err = tx.QueryRow(ctx, `
		INSERT INTO workspace_deletions (workspace_id, status, actor, batch_size, started_at, updated_at)
		VALUES ($1, 'stopping', $2, $3, $4, $4)
		ON CONFLICT (workspace_id) DO UPDATE SET
			status = 'stopping',
			actor = CASE WHEN EXCLUDED.actor <> '' THEN EXCLUDED.actor ELSE workspace_deletions.actor END,
			batch_size = EXCLUDED.batch_size,
			updated_at = EXCLUDED.updated_at
		RETURNING counts_json`,
		ws, actor, batchSize, now,
	).Scan(&prevCountsJSON)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("upsert workspace deletion stopping state: %w", err)
	}
	counts := unmarshalDeletionCounts(prevCountsJSON)

	tag, err := tx.Exec(ctx, `UPDATE monitors SET enabled = FALSE WHERE workspace_id = $1 AND enabled = TRUE`, ws)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("disable workspace monitors: %w", err)
	}
	monitorsDisabled = tag.RowsAffected()

	tag, err = tx.Exec(ctx, `UPDATE channels SET enabled = FALSE WHERE workspace_id = $1 AND enabled = TRUE`, ws)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("disable workspace channels: %w", err)
	}
	channelsDisabled = tag.RowsAffected()

	tag, err = tx.Exec(ctx, `UPDATE api_tokens SET revoked_at = $2 WHERE workspace_id = $1 AND revoked_at IS NULL`, ws, now)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("revoke workspace api tokens: %w", err)
	}
	tokensRevoked = tag.RowsAffected()

	tag, err = tx.Exec(ctx, `DELETE FROM pending_digests WHERE channel_id IN (SELECT id FROM channels WHERE workspace_id = $1)`, ws)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("clear workspace pending digests: %w", err)
	}
	counts.PendingDigests += tag.RowsAffected()

	tag, err = tx.Exec(ctx, `
		DELETE FROM alert_escalations
		WHERE policy_id IN (
			SELECT ep.id FROM escalation_policies ep
			JOIN monitors m ON m.id = ep.monitor_id
			WHERE m.workspace_id = $1
		) OR alert_id IN (
			SELECT id FROM alerts WHERE workspace_id = $1
		)`, ws)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("cancel workspace alert escalations: %w", err)
	}
	escCleared := tag.RowsAffected()
	counts.AlertEscalations += escCleared

	if _, err := tx.Exec(ctx, `
		UPDATE workspace_deletions
		SET status = 'deleting_alerts',
		    alert_escalations_deleted = alert_escalations_deleted + $2,
		    counts_json = $3,
		    updated_at = $4
		WHERE workspace_id = $1`,
		ws, escCleared, marshalDeletionCounts(counts), now,
	); err != nil {
		return 0, 0, 0, fmt.Errorf("advance workspace deletion to deleting_alerts: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, 0, fmt.Errorf("commit workspace stop phase: %w", err)
	}
	return monitorsDisabled, channelsDisabled, tokensRevoked, nil
}

func (p *Postgres) pgDeleteWorkspaceAlertBatch(ctx context.Context, ws string, batchSize int) (int64, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin workspace alert batch: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `SELECT id FROM alerts WHERE workspace_id = $1 ORDER BY id ASC LIMIT $2`, ws, batchSize)
	if err != nil {
		return 0, fmt.Errorf("select workspace alert batch: %w", err)
	}
	var ids []int64
	var maxID int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan workspace alert id: %w", err)
		}
		ids = append(ids, id)
		if id > maxID {
			maxID = id
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("iterate workspace alert batch: %w", err)
	}
	rows.Close()

	if len(ids) == 0 {
		return 0, nil
	}

	var prevCountsJSON string
	if err := tx.QueryRow(ctx, `SELECT counts_json FROM workspace_deletions WHERE workspace_id = $1 FOR UPDATE`, ws).Scan(&prevCountsJSON); err != nil {
		return 0, fmt.Errorf("read workspace deletion counts: %w", err)
	}
	counts := unmarshalDeletionCounts(prevCountsJSON)

	tag, err := tx.Exec(ctx, `DELETE FROM delivery_attempts WHERE alert_id = ANY($1) AND EXISTS (SELECT 1 FROM alerts WHERE id = delivery_attempts.alert_id AND workspace_id = $2)`, ids, ws)
	if err != nil {
		return 0, fmt.Errorf("delete alert batch delivery attempts: %w", err)
	}
	delivDel := tag.RowsAffected()

	tag, err = tx.Exec(ctx, `DELETE FROM dead_letters WHERE alert_id = ANY($1) AND EXISTS (SELECT 1 FROM alerts WHERE id = dead_letters.alert_id AND workspace_id = $2)`, ids, ws)
	if err != nil {
		return 0, fmt.Errorf("delete alert batch dead letters: %w", err)
	}
	deadDel := tag.RowsAffected()

	tag, err = tx.Exec(ctx, `DELETE FROM alert_escalations WHERE alert_id = ANY($1) AND EXISTS (SELECT 1 FROM alerts WHERE id = alert_escalations.alert_id AND workspace_id = $2)`, ids, ws)
	if err != nil {
		return 0, fmt.Errorf("delete alert batch escalations: %w", err)
	}
	escDel := tag.RowsAffected()

	tag, err = tx.Exec(ctx, `DELETE FROM alert_dedup WHERE alert_id = ANY($1) AND EXISTS (SELECT 1 FROM alerts WHERE id = alert_dedup.alert_id AND workspace_id = $2)`, ids, ws)
	if err != nil {
		return 0, fmt.Errorf("delete alert batch dedup rows: %w", err)
	}
	dedupDel := tag.RowsAffected()

	tag, err = tx.Exec(ctx, `DELETE FROM alerts WHERE workspace_id = $1 AND id = ANY($2)`, ws, ids)
	if err != nil {
		return 0, fmt.Errorf("delete workspace alert batch: %w", err)
	}
	alertsDel := tag.RowsAffected()

	counts.Alerts += alertsDel
	counts.DeliveryAttempts += delivDel
	counts.DeadLetters += deadDel
	counts.AlertEscalations += escDel
	counts.AlertDedup += dedupDel

	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE workspace_deletions
		SET status = 'deleting_alerts',
		    batches_completed = batches_completed + 1,
		    last_alert_id = $2,
		    alerts_deleted = alerts_deleted + $3,
		    delivery_attempts_deleted = delivery_attempts_deleted + $4,
		    dead_letters_deleted = dead_letters_deleted + $5,
		    alert_escalations_deleted = alert_escalations_deleted + $6,
		    alert_dedup_deleted = alert_dedup_deleted + $7,
		    counts_json = $8,
		    updated_at = $9
		WHERE workspace_id = $1`,
		ws, maxID, alertsDel, delivDel, deadDel, escDel, dedupDel, marshalDeletionCounts(counts), now,
	); err != nil {
		return 0, fmt.Errorf("update workspace alert batch progress: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit workspace alert batch: %w", err)
	}
	return alertsDel, nil
}

func (p *Postgres) pgFinalizeWorkspaceDeletion(ctx context.Context, ws string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin workspace finalize deletion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var prevCountsJSON string
	if err := tx.QueryRow(ctx, `SELECT counts_json FROM workspace_deletions WHERE workspace_id = $1 FOR UPDATE`, ws).Scan(&prevCountsJSON); err != nil {
		return fmt.Errorf("load workspace deletion progress: %w", err)
	}
	counts := unmarshalDeletionCounts(prevCountsJSON)

	execCount := func(dest *int64, query string) error {
		tag, err := tx.Exec(ctx, query, ws)
		if err != nil {
			return err
		}
		*dest += tag.RowsAffected()
		return nil
	}

	steps := []struct {
		name  string
		dest  *int64
		query string
	}{
		{"delivery_attempts", &counts.DeliveryAttempts, `DELETE FROM delivery_attempts WHERE channel_id IN (SELECT id FROM channels WHERE workspace_id = $1)`},
		{"dead_letters", &counts.DeadLetters, `DELETE FROM dead_letters WHERE channel_id IN (SELECT id FROM channels WHERE workspace_id = $1)`},
		{"alert_dedup", &counts.AlertDedup, `DELETE FROM alert_dedup WHERE rule_id IN (SELECT r.id FROM rules r JOIN monitors m ON m.id = r.monitor_id WHERE m.workspace_id = $1)`},
		{"alert_inhibitions", &counts.AlertInhibitions, `DELETE FROM alert_inhibitions WHERE source_rule_id IN (SELECT r.id FROM rules r JOIN monitors m ON m.id = r.monitor_id WHERE m.workspace_id = $1) OR target_rule_id IN (SELECT r.id FROM rules r JOIN monitors m ON m.id = r.monitor_id WHERE m.workspace_id = $1)`},
		{"rule_absence_state", &counts.RuleAbsenceState, `DELETE FROM rule_absence_state WHERE rule_id IN (SELECT r.id FROM rules r JOIN monitors m ON m.id = r.monitor_id WHERE m.workspace_id = $1)`},
		{"escalation_step_channels", &counts.EscalationStepChannels, `DELETE FROM escalation_step_channels WHERE step_id IN (SELECT es.id FROM escalation_steps es JOIN escalation_policies ep ON ep.id = es.policy_id JOIN monitors m ON m.id = ep.monitor_id WHERE m.workspace_id = $1) OR channel_id IN (SELECT id FROM channels WHERE workspace_id = $1)`},
		{"escalation_steps", &counts.EscalationSteps, `DELETE FROM escalation_steps WHERE policy_id IN (SELECT ep.id FROM escalation_policies ep JOIN monitors m ON m.id = ep.monitor_id WHERE m.workspace_id = $1)`},
		{"escalation_policies", &counts.EscalationPolicies, `DELETE FROM escalation_policies WHERE monitor_id IN (SELECT id FROM monitors WHERE workspace_id = $1)`},
		{"maintenance_windows", &counts.MaintenanceWindows, `DELETE FROM maintenance_windows WHERE monitor_id IN (SELECT id FROM monitors WHERE workspace_id = $1)`},
		{"backfills", &counts.Backfills, `DELETE FROM backfills WHERE monitor_id IN (SELECT id FROM monitors WHERE workspace_id = $1)`},
		{"monitor_channels", &counts.MonitorChannels, `DELETE FROM monitor_channels WHERE monitor_id IN (SELECT id FROM monitors WHERE workspace_id = $1) OR channel_id IN (SELECT id FROM channels WHERE workspace_id = $1)`},
		{"rules", &counts.Rules, `DELETE FROM rules WHERE monitor_id IN (SELECT id FROM monitors WHERE workspace_id = $1)`},
		{"monitors", &counts.Monitors, `DELETE FROM monitors WHERE workspace_id = $1`},
		{"pending_digests", &counts.PendingDigests, `DELETE FROM pending_digests WHERE channel_id IN (SELECT id FROM channels WHERE workspace_id = $1)`},
		{"channel_signing_keys", &counts.ChannelSigningKeys, `DELETE FROM channel_signing_keys WHERE workspace_id = $1 OR channel_id IN (SELECT id FROM channels WHERE workspace_id = $1)`},
		{"channel_ack_reconciliations", &counts.ChannelAckReconciliations, `DELETE FROM channel_ack_reconciliations WHERE workspace_id = $1 OR channel_id IN (SELECT id FROM channels WHERE workspace_id = $1)`},
		{"channels", &counts.Channels, `DELETE FROM channels WHERE workspace_id = $1`},
		{"saved_searches", &counts.SavedSearches, `DELETE FROM saved_searches WHERE workspace_id = $1`},
		{"monitor_templates", &counts.MonitorTemplates, `DELETE FROM monitor_templates WHERE workspace_id = $1`},
	}

	for _, st := range steps {
		if err := execCount(st.dest, st.query); err != nil {
			return fmt.Errorf("delete workspace %s: %w", st.name, err)
		}
	}

	if _, err := tx.Exec(ctx, `UPDATE api_tokens SET rotated_from_id = NULL, successor_id = NULL WHERE workspace_id = $1`, ws); err != nil {
		return fmt.Errorf("unlink workspace api_tokens rotation refs: %w", err)
	}

	tailSteps := []struct {
		name  string
		dest  *int64
		query string
	}{
		{"api_tokens", &counts.APITokens, `DELETE FROM api_tokens WHERE workspace_id = $1`},
		{"workspace_usage_rollups", &counts.WorkspaceUsageRollups, `DELETE FROM workspace_usage_rollups WHERE workspace_id = $1`},
		{"workspace_notification_usage_rollups", &counts.WorkspaceNotificationUsageRollup, `DELETE FROM workspace_notification_usage_rollups WHERE workspace_id = $1`},
		{"workspace_query_budget_state", &counts.WorkspaceQueryBudgetState, `DELETE FROM workspace_query_budget_state WHERE workspace_id = $1`},
		{"workspace_quotas", &counts.WorkspaceQuotas, `DELETE FROM workspace_quotas WHERE workspace_id = $1`},
		{"idempotency_keys", &counts.IdempotencyKeys, `DELETE FROM idempotency_keys WHERE workspace_id = $1`},
		{"workspaces", &counts.Workspaces, `DELETE FROM workspaces WHERE id = $1`},
	}
	for _, st := range tailSteps {
		if err := execCount(st.dest, st.query); err != nil {
			return fmt.Errorf("delete workspace %s: %w", st.name, err)
		}
	}

	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE workspace_deletions
		SET status = 'completed',
		    alerts_deleted = $2,
		    delivery_attempts_deleted = $3,
		    dead_letters_deleted = $4,
		    alert_escalations_deleted = $5,
		    alert_dedup_deleted = $6,
		    monitors_deleted = $7,
		    rules_deleted = $8,
		    channels_deleted = $9,
		    saved_searches_deleted = $10,
		    monitor_templates_deleted = $11,
		    api_tokens_deleted = $12,
		    counts_json = $13,
		    updated_at = $14,
		    completed_at = $14
		WHERE workspace_id = $1`,
		ws,
		counts.Alerts,
		counts.DeliveryAttempts,
		counts.DeadLetters,
		counts.AlertEscalations,
		counts.AlertDedup,
		counts.Monitors,
		counts.Rules,
		counts.Channels,
		counts.SavedSearches,
		counts.MonitorTemplates,
		counts.APITokens,
		marshalDeletionCounts(counts),
		now,
	); err != nil {
		return fmt.Errorf("mark workspace deletion completed: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit workspace finalize deletion: %w", err)
	}
	return nil
}
