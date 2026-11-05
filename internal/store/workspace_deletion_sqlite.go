package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// GetWorkspaceDeletion returns the persisted deletion progress or completed
// tombstone for id on SQLite.
func (s *SQLite) GetWorkspaceDeletion(ctx context.Context, id workspace.ID) (*WorkspaceDeletionResult, error) {
	if !workspace.Valid(string(id)) {
		return nil, workspace.ErrInvalidID
	}
	ws := string(id)
	var (
		statusStr    string
		actor        string
		batchSize    int
		batches      int64
		lastAlertID  int64
		alertsDel    int64
		delivDel     int64
		deadDel      int64
		escDel       int64
		dedupDel     int64
		monDel       int64
		rulesDel     int64
		chanDel      int64
		searchDel    int64
		tmplDel      int64
		tokDel       int64
		countsJSON   string
		startedStr   string
		updatedStr   string
		completedStr sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT status, actor, batch_size, batches_completed, last_alert_id,
		       alerts_deleted, delivery_attempts_deleted, dead_letters_deleted,
		       alert_escalations_deleted, alert_dedup_deleted,
		       monitors_deleted, rules_deleted, channels_deleted,
		       saved_searches_deleted, monitor_templates_deleted, api_tokens_deleted,
		       counts_json, started_at, updated_at, completed_at
		FROM workspace_deletions
		WHERE workspace_id = ?`, ws,
	).Scan(
		&statusStr, &actor, &batchSize, &batches, &lastAlertID,
		&alertsDel, &delivDel, &deadDel, &escDel, &dedupDel,
		&monDel, &rulesDel, &chanDel, &searchDel, &tmplDel, &tokDel,
		&countsJSON, &startedStr, &updatedStr, &completedStr,
	)
	if errors.Is(err, sql.ErrNoRows) {
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

	var sAt, uAt, cAt *time.Time
	if t, err := parseSQLiteTime(startedStr); err == nil {
		u := t.UTC()
		sAt = &u
	}
	if t, err := parseSQLiteTime(updatedStr); err == nil {
		u := t.UTC()
		uAt = &u
	}
	if completedStr.Valid && completedStr.String != "" {
		if t, err := parseSQLiteTime(completedStr.String); err == nil {
			u := t.UTC()
			cAt = &u
		}
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
		StartedAt:        sAt,
		UpdatedAt:        uAt,
		CompletedAt:      cAt,
	}, nil
}

func (s *SQLite) sqliteCountWorkspaceRows(ctx context.Context, ws string) (WorkspaceDeletionCounts, error) {
	var c WorkspaceDeletionCounts
	queries := []struct {
		dest  *int64
		query string
		args  []any
	}{
		{&c.Workspaces, `SELECT COUNT(*) FROM workspaces WHERE id = ?`, []any{ws}},
		{&c.Monitors, `SELECT COUNT(*) FROM monitors WHERE workspace_id = ?`, []any{ws}},
		{&c.Rules, `SELECT COUNT(*) FROM rules WHERE monitor_id IN (SELECT id FROM monitors WHERE workspace_id = ?)`, []any{ws}},
		{&c.RuleAbsenceState, `SELECT COUNT(*) FROM rule_absence_state WHERE rule_id IN (SELECT r.id FROM rules r JOIN monitors m ON m.id = r.monitor_id WHERE m.workspace_id = ?)`, []any{ws}},
		{&c.AlertInhibitions, `SELECT COUNT(*) FROM alert_inhibitions WHERE source_rule_id IN (SELECT r.id FROM rules r JOIN monitors m ON m.id = r.monitor_id WHERE m.workspace_id = ?) OR target_rule_id IN (SELECT r.id FROM rules r JOIN monitors m ON m.id = r.monitor_id WHERE m.workspace_id = ?)`, []any{ws, ws}},
		{&c.Channels, `SELECT COUNT(*) FROM channels WHERE workspace_id = ?`, []any{ws}},
		{&c.MonitorChannels, `SELECT COUNT(*) FROM monitor_channels WHERE monitor_id IN (SELECT id FROM monitors WHERE workspace_id = ?) OR channel_id IN (SELECT id FROM channels WHERE workspace_id = ?)`, []any{ws, ws}},
		{&c.ChannelSigningKeys, `SELECT COUNT(*) FROM channel_signing_keys WHERE workspace_id = ? OR channel_id IN (SELECT id FROM channels WHERE workspace_id = ?)`, []any{ws, ws}},
		{&c.ChannelAckReconciliations, `SELECT COUNT(*) FROM channel_ack_reconciliations WHERE workspace_id = ? OR channel_id IN (SELECT id FROM channels WHERE workspace_id = ?)`, []any{ws, ws}},
		{&c.PendingDigests, `SELECT COUNT(*) FROM pending_digests WHERE channel_id IN (SELECT id FROM channels WHERE workspace_id = ?)`, []any{ws}},
		{&c.EscalationPolicies, `SELECT COUNT(*) FROM escalation_policies WHERE monitor_id IN (SELECT id FROM monitors WHERE workspace_id = ?)`, []any{ws}},
		{&c.EscalationSteps, `SELECT COUNT(*) FROM escalation_steps WHERE policy_id IN (SELECT ep.id FROM escalation_policies ep JOIN monitors m ON m.id = ep.monitor_id WHERE m.workspace_id = ?)`, []any{ws}},
		{&c.EscalationStepChannels, `SELECT COUNT(*) FROM escalation_step_channels WHERE step_id IN (SELECT es.id FROM escalation_steps es JOIN escalation_policies ep ON ep.id = es.policy_id JOIN monitors m ON m.id = ep.monitor_id WHERE m.workspace_id = ?) OR channel_id IN (SELECT id FROM channels WHERE workspace_id = ?)`, []any{ws, ws}},
		{&c.MaintenanceWindows, `SELECT COUNT(*) FROM maintenance_windows WHERE monitor_id IN (SELECT id FROM monitors WHERE workspace_id = ?)`, []any{ws}},
		{&c.Backfills, `SELECT COUNT(*) FROM backfills WHERE monitor_id IN (SELECT id FROM monitors WHERE workspace_id = ?)`, []any{ws}},
		{&c.Alerts, `SELECT COUNT(*) FROM alerts WHERE workspace_id = ?`, []any{ws}},
		{&c.DeliveryAttempts, `SELECT COUNT(*) FROM delivery_attempts WHERE alert_id IN (SELECT id FROM alerts WHERE workspace_id = ?) OR channel_id IN (SELECT id FROM channels WHERE workspace_id = ?)`, []any{ws, ws}},
		{&c.DeadLetters, `SELECT COUNT(*) FROM dead_letters WHERE alert_id IN (SELECT id FROM alerts WHERE workspace_id = ?) OR channel_id IN (SELECT id FROM channels WHERE workspace_id = ?)`, []any{ws, ws}},
		{&c.AlertEscalations, `SELECT COUNT(*) FROM alert_escalations WHERE alert_id IN (SELECT id FROM alerts WHERE workspace_id = ?) OR policy_id IN (SELECT ep.id FROM escalation_policies ep JOIN monitors m ON m.id = ep.monitor_id WHERE m.workspace_id = ?)`, []any{ws, ws}},
		{&c.SavedSearches, `SELECT COUNT(*) FROM saved_searches WHERE workspace_id = ?`, []any{ws}},
		{&c.MonitorTemplates, `SELECT COUNT(*) FROM monitor_templates WHERE workspace_id = ?`, []any{ws}},
		{&c.APITokens, `SELECT COUNT(*) FROM api_tokens WHERE workspace_id = ?`, []any{ws}},
		{&c.WorkspaceUsageRollups, `SELECT COUNT(*) FROM workspace_usage_rollups WHERE workspace_id = ?`, []any{ws}},
		{&c.WorkspaceNotificationUsageRollup, `SELECT COUNT(*) FROM workspace_notification_usage_rollups WHERE workspace_id = ?`, []any{ws}},
		{&c.WorkspaceQuotas, `SELECT COUNT(*) FROM workspace_quotas WHERE workspace_id = ?`, []any{ws}},
		{&c.WorkspaceQueryBudgetState, `SELECT COUNT(*) FROM workspace_query_budget_state WHERE workspace_id = ?`, []any{ws}},
		{&c.IdempotencyKeys, `SELECT COUNT(*) FROM idempotency_keys WHERE workspace_id = ?`, []any{ws}},
	}
	for _, q := range queries {
		if err := s.db.QueryRowContext(ctx, q.query, q.args...).Scan(q.dest); err != nil {
			return WorkspaceDeletionCounts{}, fmt.Errorf("count workspace rows: %w", err)
		}
	}
	return c, nil
}

// DeleteWorkspace implements bounded, resumable deletion of a workspace and
// every row belonging to it on SQLite.
func (s *SQLite) DeleteWorkspace(ctx context.Context, id workspace.ID, opts WorkspaceDeletionOptions) (*WorkspaceDeletionResult, error) {
	if !workspace.Valid(string(id)) {
		return nil, workspace.ErrInvalidID
	}
	ws := string(id)
	batchSize := normalizeWorkspaceDeleteBatchSize(opts.BatchSize)

	currentCounts, err := s.sqliteCountWorkspaceRows(ctx, ws)
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

	existing, existingErr := s.GetWorkspaceDeletion(ctx, id)
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
	channelIDs, err := s.sqliteListWorkspaceChannelIDs(ctx, ws)
	if err != nil {
		return nil, err
	}
	resetCompleted := existing != nil && existing.Completed && currentCounts.Total() > 0
	monitorsDisabled, channelsDisabled, tokensRevoked, err := s.sqliteStopWorkspacePhase(ctx, ws, opts.Actor, batchSize, resetCompleted)
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
			hasMore, err := s.sqliteHasRemainingAlerts(ctx, ws)
			if err != nil {
				return nil, err
			}
			if hasMore {
				res, err := s.GetWorkspaceDeletion(ctx, id)
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
		deletedInBatch, err := s.sqliteDeleteWorkspaceAlertBatch(ctx, ws, batchSize)
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
	if err := s.sqliteFinalizeWorkspaceDeletion(ctx, ws); err != nil {
		return nil, err
	}

	res, err := s.GetWorkspaceDeletion(ctx, id)
	if err != nil {
		return nil, err
	}
	res.MonitorsDisabled = monitorsDisabled
	res.ChannelsDisabled = channelsDisabled
	res.TokensRevoked = tokensRevoked
	return res, nil
}

func (s *SQLite) sqliteListWorkspaceChannelIDs(ctx context.Context, ws string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM channels WHERE workspace_id = ? ORDER BY id ASC`, ws)
	if err != nil {
		return nil, fmt.Errorf("list workspace channel ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
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

func (s *SQLite) sqliteHasRemainingAlerts(ctx context.Context, ws string) (bool, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM alerts WHERE workspace_id = ?`, ws).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check remaining workspace alerts: %w", err)
	}
	return count > 0, nil
}

func (s *SQLite) sqliteStopWorkspacePhase(ctx context.Context, ws, actor string, batchSize int, resetCompleted bool) (monitorsDisabled, channelsDisabled, tokensRevoked int64, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("begin workspace stop phase: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	nowStr := sqliteTimeString(time.Now().UTC())
	if resetCompleted {
		if _, err := tx.ExecContext(ctx, `DELETE FROM workspace_deletions WHERE workspace_id = ?`, ws); err != nil {
			return 0, 0, 0, fmt.Errorf("reset workspace deletion row: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workspace_deletions (workspace_id, status, actor, batch_size, started_at, updated_at)
		VALUES (?, 'stopping', ?, ?, ?, ?)
		ON CONFLICT (workspace_id) DO UPDATE SET
			status = 'stopping',
			actor = CASE WHEN excluded.actor <> '' THEN excluded.actor ELSE workspace_deletions.actor END,
			batch_size = excluded.batch_size,
			updated_at = excluded.updated_at`,
		ws, actor, batchSize, nowStr, nowStr,
	); err != nil {
		return 0, 0, 0, fmt.Errorf("upsert workspace deletion stopping state: %w", err)
	}

	var prevCountsJSON string
	if err := tx.QueryRowContext(ctx, `SELECT counts_json FROM workspace_deletions WHERE workspace_id = ?`, ws).Scan(&prevCountsJSON); err != nil {
		return 0, 0, 0, fmt.Errorf("read workspace deletion counts: %w", err)
	}
	counts := unmarshalDeletionCounts(prevCountsJSON)

	res, err := tx.ExecContext(ctx, `UPDATE monitors SET enabled = 0 WHERE workspace_id = ? AND enabled = 1`, ws)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("disable workspace monitors: %w", err)
	}
	monitorsDisabled, _ = res.RowsAffected()

	res, err = tx.ExecContext(ctx, `UPDATE channels SET enabled = 0 WHERE workspace_id = ? AND enabled = 1`, ws)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("disable workspace channels: %w", err)
	}
	channelsDisabled, _ = res.RowsAffected()

	res, err = tx.ExecContext(ctx, `UPDATE api_tokens SET revoked_at = ? WHERE workspace_id = ? AND revoked_at IS NULL`, nowStr, ws)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("revoke workspace api tokens: %w", err)
	}
	tokensRevoked, _ = res.RowsAffected()

	res, err = tx.ExecContext(ctx, `DELETE FROM pending_digests WHERE channel_id IN (SELECT id FROM channels WHERE workspace_id = ?)`, ws)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("clear workspace pending digests: %w", err)
	}
	digestsCleared, _ := res.RowsAffected()
	counts.PendingDigests += digestsCleared

	res, err = tx.ExecContext(ctx, `
		DELETE FROM alert_escalations
		WHERE policy_id IN (
			SELECT ep.id FROM escalation_policies ep
			JOIN monitors m ON m.id = ep.monitor_id
			WHERE m.workspace_id = ?
		) OR alert_id IN (
			SELECT id FROM alerts WHERE workspace_id = ?
		)`, ws, ws)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("cancel workspace alert escalations: %w", err)
	}
	escCleared, _ := res.RowsAffected()
	counts.AlertEscalations += escCleared

	if _, err := tx.ExecContext(ctx, `
		UPDATE workspace_deletions
		SET status = 'deleting_alerts',
		    alert_escalations_deleted = alert_escalations_deleted + ?,
		    counts_json = ?,
		    updated_at = ?
		WHERE workspace_id = ?`,
		escCleared, marshalDeletionCounts(counts), nowStr, ws,
	); err != nil {
		return 0, 0, 0, fmt.Errorf("advance workspace deletion to deleting_alerts: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, 0, fmt.Errorf("commit workspace stop phase: %w", err)
	}
	return monitorsDisabled, channelsDisabled, tokensRevoked, nil
}

func (s *SQLite) sqliteDeleteWorkspaceAlertBatch(ctx context.Context, ws string, batchSize int) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin workspace alert batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `SELECT id FROM alerts WHERE workspace_id = ? ORDER BY id ASC LIMIT ?`, ws, batchSize)
	if err != nil {
		return 0, fmt.Errorf("select workspace alert batch: %w", err)
	}
	var ids []int64
	var maxID int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan workspace alert id: %w", err)
		}
		ids = append(ids, id)
		if id > maxID {
			maxID = id
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("iterate workspace alert batch: %w", err)
	}
	_ = rows.Close()

	if len(ids) == 0 {
		return 0, nil
	}

	var prevCountsJSON string
	if err := tx.QueryRowContext(ctx, `SELECT counts_json FROM workspace_deletions WHERE workspace_id = ?`, ws).Scan(&prevCountsJSON); err != nil {
		return 0, fmt.Errorf("read workspace deletion counts: %w", err)
	}
	counts := unmarshalDeletionCounts(prevCountsJSON)

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	idArgs := make([]any, 0, len(ids)+1)
	for _, id := range ids {
		idArgs = append(idArgs, id)
	}
	idAndWSArgs := append(append([]any{}, idArgs...), ws)

	res, err := tx.ExecContext(ctx, `DELETE FROM delivery_attempts WHERE alert_id IN (`+placeholders+`) AND EXISTS (SELECT 1 FROM alerts WHERE id = delivery_attempts.alert_id AND workspace_id = ?)`, idAndWSArgs...) // #nosec G202 -- placeholders contains only ? markers; values are bound in idAndWSArgs
	if err != nil {
		return 0, fmt.Errorf("delete alert batch delivery attempts: %w", err)
	}
	delivDel, _ := res.RowsAffected()

	res, err = tx.ExecContext(ctx, `DELETE FROM dead_letters WHERE alert_id IN (`+placeholders+`) AND EXISTS (SELECT 1 FROM alerts WHERE id = dead_letters.alert_id AND workspace_id = ?)`, idAndWSArgs...) // #nosec G202 -- placeholders contains only ? markers; values are bound in idAndWSArgs
	if err != nil {
		return 0, fmt.Errorf("delete alert batch dead letters: %w", err)
	}
	deadDel, _ := res.RowsAffected()

	res, err = tx.ExecContext(ctx, `DELETE FROM alert_escalations WHERE alert_id IN (`+placeholders+`) AND EXISTS (SELECT 1 FROM alerts WHERE id = alert_escalations.alert_id AND workspace_id = ?)`, idAndWSArgs...) // #nosec G202 -- placeholders contains only ? markers; values are bound in idAndWSArgs
	if err != nil {
		return 0, fmt.Errorf("delete alert batch escalations: %w", err)
	}
	escDel, _ := res.RowsAffected()

	wsAndIDArgs := append([]any{ws}, idArgs...)
	res, err = tx.ExecContext(ctx, `DELETE FROM alerts WHERE workspace_id = ? AND id IN (`+placeholders+`)`, wsAndIDArgs...) // #nosec G202 -- placeholders contains only ? markers; values are bound in wsAndIDArgs
	if err != nil {
		return 0, fmt.Errorf("delete workspace alert batch: %w", err)
	}
	alertsDel, _ := res.RowsAffected()

	counts.Alerts += alertsDel
	counts.DeliveryAttempts += delivDel
	counts.DeadLetters += deadDel
	counts.AlertEscalations += escDel

	nowStr := sqliteTimeString(time.Now().UTC())
	if _, err := tx.ExecContext(ctx, `
		UPDATE workspace_deletions
		SET status = 'deleting_alerts',
		    batches_completed = batches_completed + 1,
		    last_alert_id = ?,
		    alerts_deleted = alerts_deleted + ?,
		    delivery_attempts_deleted = delivery_attempts_deleted + ?,
		    dead_letters_deleted = dead_letters_deleted + ?,
		    alert_escalations_deleted = alert_escalations_deleted + ?,
		    counts_json = ?,
		    updated_at = ?
		WHERE workspace_id = ?`,
		maxID, alertsDel, delivDel, deadDel, escDel, marshalDeletionCounts(counts), nowStr, ws,
	); err != nil {
		return 0, fmt.Errorf("update workspace alert batch progress: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit workspace alert batch: %w", err)
	}
	return alertsDel, nil
}

func (s *SQLite) sqliteFinalizeWorkspaceDeletion(ctx context.Context, ws string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin workspace finalize deletion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var prevCountsJSON string
	if err := tx.QueryRowContext(ctx, `SELECT counts_json FROM workspace_deletions WHERE workspace_id = ?`, ws).Scan(&prevCountsJSON); err != nil {
		return fmt.Errorf("load workspace deletion progress: %w", err)
	}
	counts := unmarshalDeletionCounts(prevCountsJSON)

	execCount := func(dest *int64, query string, args ...any) error {
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		*dest += n
		return nil
	}

	steps := []struct {
		name  string
		dest  *int64
		query string
		args  []any
	}{
		{"delivery_attempts", &counts.DeliveryAttempts, `DELETE FROM delivery_attempts WHERE channel_id IN (SELECT id FROM channels WHERE workspace_id = ?)`, []any{ws}},
		{"dead_letters", &counts.DeadLetters, `DELETE FROM dead_letters WHERE channel_id IN (SELECT id FROM channels WHERE workspace_id = ?)`, []any{ws}},
		{"alert_inhibitions", &counts.AlertInhibitions, `DELETE FROM alert_inhibitions WHERE source_rule_id IN (SELECT r.id FROM rules r JOIN monitors m ON m.id = r.monitor_id WHERE m.workspace_id = ?) OR target_rule_id IN (SELECT r.id FROM rules r JOIN monitors m ON m.id = r.monitor_id WHERE m.workspace_id = ?)`, []any{ws, ws}},
		{"rule_absence_state", &counts.RuleAbsenceState, `DELETE FROM rule_absence_state WHERE rule_id IN (SELECT r.id FROM rules r JOIN monitors m ON m.id = r.monitor_id WHERE m.workspace_id = ?)`, []any{ws}},
		{"escalation_step_channels", &counts.EscalationStepChannels, `DELETE FROM escalation_step_channels WHERE step_id IN (SELECT es.id FROM escalation_steps es JOIN escalation_policies ep ON ep.id = es.policy_id JOIN monitors m ON m.id = ep.monitor_id WHERE m.workspace_id = ?) OR channel_id IN (SELECT id FROM channels WHERE workspace_id = ?)`, []any{ws, ws}},
		{"escalation_steps", &counts.EscalationSteps, `DELETE FROM escalation_steps WHERE policy_id IN (SELECT ep.id FROM escalation_policies ep JOIN monitors m ON m.id = ep.monitor_id WHERE m.workspace_id = ?)`, []any{ws}},
		{"escalation_policies", &counts.EscalationPolicies, `DELETE FROM escalation_policies WHERE monitor_id IN (SELECT id FROM monitors WHERE workspace_id = ?)`, []any{ws}},
		{"maintenance_windows", &counts.MaintenanceWindows, `DELETE FROM maintenance_windows WHERE monitor_id IN (SELECT id FROM monitors WHERE workspace_id = ?)`, []any{ws}},
		{"backfills", &counts.Backfills, `DELETE FROM backfills WHERE monitor_id IN (SELECT id FROM monitors WHERE workspace_id = ?)`, []any{ws}},
		{"monitor_channels", &counts.MonitorChannels, `DELETE FROM monitor_channels WHERE monitor_id IN (SELECT id FROM monitors WHERE workspace_id = ?) OR channel_id IN (SELECT id FROM channels WHERE workspace_id = ?)`, []any{ws, ws}},
		{"rules", &counts.Rules, `DELETE FROM rules WHERE monitor_id IN (SELECT id FROM monitors WHERE workspace_id = ?)`, []any{ws}},
		{"monitors", &counts.Monitors, `DELETE FROM monitors WHERE workspace_id = ?`, []any{ws}},
		{"pending_digests", &counts.PendingDigests, `DELETE FROM pending_digests WHERE channel_id IN (SELECT id FROM channels WHERE workspace_id = ?)`, []any{ws}},
		{"channel_signing_keys", &counts.ChannelSigningKeys, `DELETE FROM channel_signing_keys WHERE workspace_id = ? OR channel_id IN (SELECT id FROM channels WHERE workspace_id = ?)`, []any{ws, ws}},
		{"channel_ack_reconciliations", &counts.ChannelAckReconciliations, `DELETE FROM channel_ack_reconciliations WHERE workspace_id = ? OR channel_id IN (SELECT id FROM channels WHERE workspace_id = ?)`, []any{ws, ws}},
		{"channels", &counts.Channels, `DELETE FROM channels WHERE workspace_id = ?`, []any{ws}},
		{"saved_searches", &counts.SavedSearches, `DELETE FROM saved_searches WHERE workspace_id = ?`, []any{ws}},
		{"monitor_templates", &counts.MonitorTemplates, `DELETE FROM monitor_templates WHERE workspace_id = ?`, []any{ws}},
	}

	for _, st := range steps {
		if err := execCount(st.dest, st.query, st.args...); err != nil {
			return fmt.Errorf("delete workspace %s: %w", st.name, err)
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE api_tokens SET rotated_from_id = NULL, successor_id = NULL WHERE workspace_id = ?`, ws); err != nil {
		return fmt.Errorf("unlink workspace api_tokens rotation refs: %w", err)
	}

	tailSteps := []struct {
		name  string
		dest  *int64
		query string
	}{
		{"api_tokens", &counts.APITokens, `DELETE FROM api_tokens WHERE workspace_id = ?`},
		{"workspace_usage_rollups", &counts.WorkspaceUsageRollups, `DELETE FROM workspace_usage_rollups WHERE workspace_id = ?`},
		{"workspace_notification_usage_rollups", &counts.WorkspaceNotificationUsageRollup, `DELETE FROM workspace_notification_usage_rollups WHERE workspace_id = ?`},
		{"workspace_query_budget_state", &counts.WorkspaceQueryBudgetState, `DELETE FROM workspace_query_budget_state WHERE workspace_id = ?`},
		{"workspace_quotas", &counts.WorkspaceQuotas, `DELETE FROM workspace_quotas WHERE workspace_id = ?`},
		{"idempotency_keys", &counts.IdempotencyKeys, `DELETE FROM idempotency_keys WHERE workspace_id = ?`},
		{"workspaces", &counts.Workspaces, `DELETE FROM workspaces WHERE id = ?`},
	}
	for _, st := range tailSteps {
		if err := execCount(st.dest, st.query, ws); err != nil {
			return fmt.Errorf("delete workspace %s: %w", st.name, err)
		}
	}

	nowStr := sqliteTimeString(time.Now().UTC())
	if _, err := tx.ExecContext(ctx, `
		UPDATE workspace_deletions
		SET status = 'completed',
		    alerts_deleted = ?,
		    delivery_attempts_deleted = ?,
		    dead_letters_deleted = ?,
		    alert_escalations_deleted = ?,
		    alert_dedup_deleted = ?,
		    monitors_deleted = ?,
		    rules_deleted = ?,
		    channels_deleted = ?,
		    saved_searches_deleted = ?,
		    monitor_templates_deleted = ?,
		    api_tokens_deleted = ?,
		    counts_json = ?,
		    updated_at = ?,
		    completed_at = ?
		WHERE workspace_id = ?`,
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
		nowStr,
		nowStr,
		ws,
	); err != nil {
		return fmt.Errorf("mark workspace deletion completed: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit workspace finalize deletion: %w", err)
	}
	return nil
}
