package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

func (s *SQLite) GetWorkspaceQuota(ctx context.Context, targetWorkspaceID string) (WorkspaceQuota, error) {
	ws := resolveQuotaWorkspace(ctx, targetWorkspaceID)
	q := WorkspaceQuota{WorkspaceID: ws}
	var updatedStr string
	err := s.db.QueryRowContext(ctx, `
		SELECT workspace_id, max_monitors, max_rules, max_channels, max_subscriptions,
		       query_cost_budget, query_cost_refill_per_sec, updated_at
		FROM workspace_quotas
		WHERE workspace_id = ?`, ws,
	).Scan(&q.WorkspaceID, &q.MaxMonitors, &q.MaxRules, &q.MaxChannels, &q.MaxSubscriptions,
		&q.QueryCostBudget, &q.QueryCostRefillPerSec, &updatedStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkspaceQuota{WorkspaceID: ws}, nil
		}
		return q, mapSQLiteErr(err)
	}
	if q.UpdatedAt, err = parseSQLiteTime(updatedStr); err != nil {
		return q, err
	}
	return q, nil
}

func (s *SQLite) SetWorkspaceQuota(ctx context.Context, q WorkspaceQuota) (WorkspaceQuota, error) {
	ws := resolveQuotaWorkspace(ctx, q.WorkspaceID)
	q.WorkspaceID = ws
	if err := validateWorkspaceQuota(&q); err != nil {
		return q, err
	}
	if q.QueryCostBudget > 0 && q.QueryCostRefillPerSec <= 0 {
		q.QueryCostRefillPerSec = effectiveRefillRate(q.QueryCostBudget, 0)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return q, mapSQLiteErr(err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO workspaces (id) VALUES (?) ON CONFLICT (id) DO NOTHING`, ws); err != nil {
		return q, mapSQLiteErr(err)
	}

	now := time.Now().UTC()
	nowStr := sqliteTimeString(now)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workspace_quotas (
			workspace_id, max_monitors, max_rules, max_channels, max_subscriptions,
			query_cost_budget, query_cost_refill_per_sec, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (workspace_id) DO UPDATE SET
			max_monitors              = excluded.max_monitors,
			max_rules                 = excluded.max_rules,
			max_channels              = excluded.max_channels,
			max_subscriptions         = excluded.max_subscriptions,
			query_cost_budget         = excluded.query_cost_budget,
			query_cost_refill_per_sec = excluded.query_cost_refill_per_sec,
			updated_at                = excluded.updated_at`,
		ws, q.MaxMonitors, q.MaxRules, q.MaxChannels, q.MaxSubscriptions,
		q.QueryCostBudget, q.QueryCostRefillPerSec, nowStr,
	); err != nil {
		return q, mapSQLiteErr(err)
	}
	q.UpdatedAt = now

	if q.QueryCostBudget > 0 {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO workspace_query_budget_state (workspace_id, tokens, updated_at)
			VALUES (?, ?, ?)
			ON CONFLICT (workspace_id) DO UPDATE SET
				tokens     = MIN(workspace_query_budget_state.tokens, excluded.tokens),
				updated_at = excluded.updated_at`,
			ws, q.QueryCostBudget, nowStr,
		); err != nil {
			return q, mapSQLiteErr(err)
		}
	} else {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM workspace_query_budget_state WHERE workspace_id = ?`, ws); err != nil {
			return q, mapSQLiteErr(err)
		}
	}

	if err := tx.Commit(); err != nil {
		return q, mapSQLiteErr(err)
	}
	return q, nil
}

func (s *SQLite) DeleteWorkspaceQuota(ctx context.Context, targetWorkspaceID string) error {
	ws := resolveQuotaWorkspace(ctx, targetWorkspaceID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return mapSQLiteErr(err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM workspace_query_budget_state WHERE workspace_id = ?`, ws); err != nil {
		return mapSQLiteErr(err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM workspace_quotas WHERE workspace_id = ?`, ws); err != nil {
		return mapSQLiteErr(err)
	}
	return mapSQLiteErr(tx.Commit())
}

func (s *SQLite) ListWorkspaceQuotas(ctx context.Context) ([]WorkspaceQuota, error) {
	q := `
		SELECT workspace_id, max_monitors, max_rules, max_channels, max_subscriptions,
		       query_cost_budget, query_cost_refill_per_sec, updated_at
		FROM workspace_quotas`
	var args []any
	if ws, scoped := tenantWorkspace(ctx); scoped {
		q += ` WHERE workspace_id = ?`
		args = append(args, string(ws))
	}
	q += ` ORDER BY workspace_id ASC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, mapSQLiteErr(err)
	}
	defer func() { _ = rows.Close() }()

	var out []WorkspaceQuota
	for rows.Next() {
		var item WorkspaceQuota
		var updatedStr string
		if err := rows.Scan(
			&item.WorkspaceID, &item.MaxMonitors, &item.MaxRules, &item.MaxChannels,
			&item.MaxSubscriptions, &item.QueryCostBudget, &item.QueryCostRefillPerSec, &updatedStr,
		); err != nil {
			return nil, mapSQLiteErr(err)
		}
		if item.UpdatedAt, err = parseSQLiteTime(updatedStr); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *SQLite) ConsumeWorkspaceQueryBudget(ctx context.Context, targetWorkspaceID string, cost float64, now time.Time) (WorkspaceQueryBudgetDecision, error) {
	ws := resolveQuotaWorkspace(ctx, targetWorkspaceID)
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	nowStr := sqliteTimeString(now)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkspaceQueryBudgetDecision{}, mapSQLiteErr(err)
	}
	defer func() { _ = tx.Rollback() }()

	var budget, refillPerSec float64
	err = tx.QueryRowContext(ctx, `
		SELECT query_cost_budget, query_cost_refill_per_sec
		FROM workspace_quotas
		WHERE workspace_id = ?`, ws,
	).Scan(&budget, &refillPerSec)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkspaceQueryBudgetDecision{WorkspaceID: ws, Allowed: true, Configured: false}, nil
		}
		return WorkspaceQueryBudgetDecision{}, mapSQLiteErr(err)
	}
	if budget <= 0 {
		return WorkspaceQueryBudgetDecision{WorkspaceID: ws, Allowed: true, Configured: false}, nil
	}
	rate := effectiveRefillRate(budget, refillPerSec)

	var tokens float64
	var updatedStr string
	err = tx.QueryRowContext(ctx, `
		SELECT tokens, updated_at
		FROM workspace_query_budget_state
		WHERE workspace_id = ?`, ws,
	).Scan(&tokens, &updatedStr)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return WorkspaceQueryBudgetDecision{}, mapSQLiteErr(err)
		}
		tokens = budget
	} else {
		updatedAt, parseErr := parseSQLiteTime(updatedStr)
		if parseErr != nil {
			return WorkspaceQueryBudgetDecision{}, parseErr
		}
		tokens = computeRefilledTokens(tokens, budget, rate, updatedAt, now)
	}

	if cost <= 0 {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO workspace_query_budget_state (workspace_id, tokens, updated_at)
			VALUES (?, ?, ?)
			ON CONFLICT (workspace_id) DO UPDATE SET tokens = excluded.tokens, updated_at = excluded.updated_at`,
			ws, tokens, nowStr,
		); err != nil {
			return WorkspaceQueryBudgetDecision{}, mapSQLiteErr(err)
		}
		if err := tx.Commit(); err != nil {
			return WorkspaceQueryBudgetDecision{}, mapSQLiteErr(err)
		}
		return WorkspaceQueryBudgetDecision{
			WorkspaceID:  ws,
			Allowed:      true,
			Configured:   true,
			Limit:        budget,
			Remaining:    tokens,
			RefillPerSec: rate,
		}, nil
	}

	if tokens+1e-9 < cost {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO workspace_query_budget_state (workspace_id, tokens, updated_at)
			VALUES (?, ?, ?)
			ON CONFLICT (workspace_id) DO UPDATE SET tokens = excluded.tokens, updated_at = excluded.updated_at`,
			ws, tokens, nowStr,
		); err != nil {
			return WorkspaceQueryBudgetDecision{}, mapSQLiteErr(err)
		}
		if err := tx.Commit(); err != nil {
			return WorkspaceQueryBudgetDecision{}, mapSQLiteErr(err)
		}
		deficit := cost - tokens
		retryAfter := computeRetryAfter(deficit, rate)
		return WorkspaceQueryBudgetDecision{
			WorkspaceID:  ws,
			Allowed:      false,
			Configured:   true,
			Cost:         cost,
			Limit:        budget,
			Remaining:    tokens,
			RefillPerSec: rate,
			RetryAfter:   retryAfter,
		}, nil
	}

	tokens -= cost
	if tokens < 0 {
		tokens = 0
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workspace_query_budget_state (workspace_id, tokens, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT (workspace_id) DO UPDATE SET tokens = excluded.tokens, updated_at = excluded.updated_at`,
		ws, tokens, nowStr,
	); err != nil {
		return WorkspaceQueryBudgetDecision{}, mapSQLiteErr(err)
	}
	if err := tx.Commit(); err != nil {
		return WorkspaceQueryBudgetDecision{}, mapSQLiteErr(err)
	}
	return WorkspaceQueryBudgetDecision{
		WorkspaceID:  ws,
		Allowed:      true,
		Configured:   true,
		Cost:         cost,
		Limit:        budget,
		Remaining:    tokens,
		RefillPerSec: rate,
	}, nil
}

func (s *SQLite) ReconcileWorkspaceQueryBudget(ctx context.Context, targetWorkspaceID string, deltaCost float64, now time.Time) (float64, error) {
	ws := resolveQuotaWorkspace(ctx, targetWorkspaceID)
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	nowStr := sqliteTimeString(now)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, mapSQLiteErr(err)
	}
	defer func() { _ = tx.Rollback() }()

	var budget, refillPerSec float64
	err = tx.QueryRowContext(ctx, `
		SELECT query_cost_budget, query_cost_refill_per_sec
		FROM workspace_quotas
		WHERE workspace_id = ?`, ws,
	).Scan(&budget, &refillPerSec)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, mapSQLiteErr(err)
	}
	if budget <= 0 {
		return 0, nil
	}
	rate := effectiveRefillRate(budget, refillPerSec)

	var tokens float64
	var updatedStr string
	err = tx.QueryRowContext(ctx, `
		SELECT tokens, updated_at
		FROM workspace_query_budget_state
		WHERE workspace_id = ?`, ws,
	).Scan(&tokens, &updatedStr)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, mapSQLiteErr(err)
		}
		tokens = budget
	} else {
		updatedAt, parseErr := parseSQLiteTime(updatedStr)
		if parseErr != nil {
			return 0, parseErr
		}
		tokens = computeRefilledTokens(tokens, budget, rate, updatedAt, now)
	}

	tokens -= deltaCost
	if tokens > budget {
		tokens = budget
	}
	if tokens < 0 {
		tokens = 0
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workspace_query_budget_state (workspace_id, tokens, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT (workspace_id) DO UPDATE SET tokens = excluded.tokens, updated_at = excluded.updated_at`,
		ws, tokens, nowStr,
	); err != nil {
		return 0, mapSQLiteErr(err)
	}
	if err := tx.Commit(); err != nil {
		return 0, mapSQLiteErr(err)
	}
	return tokens, nil
}

func (s *SQLite) checkMonitorQuota(ctx context.Context, tx *sql.Tx, ws workspace.ID, delta int64) error {
	if delta <= 0 {
		return nil
	}
	var maxMonitors int64
	err := tx.QueryRowContext(ctx,
		`SELECT max_monitors FROM workspace_quotas WHERE workspace_id = ?`, string(ws),
	).Scan(&maxMonitors)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return mapSQLiteErr(err)
	}
	if maxMonitors <= 0 {
		return nil
	}
	var current int64
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM monitors WHERE workspace_id = ? AND deleted_at IS NULL`, ws,
	).Scan(&current); err != nil {
		return mapSQLiteErr(err)
	}
	if current+delta > maxMonitors {
		return &QuotaExceededError{
			WorkspaceID: string(ws),
			Resource:    QuotaResourceMonitors,
			Limit:       maxMonitors,
			Current:     current,
			Requested:   delta,
		}
	}
	return nil
}

func (s *SQLite) checkRuleQuota(ctx context.Context, tx *sql.Tx, ws workspace.ID, delta int64) error {
	if delta <= 0 {
		return nil
	}
	var maxRules int64
	err := tx.QueryRowContext(ctx,
		`SELECT max_rules FROM workspace_quotas WHERE workspace_id = ?`, string(ws),
	).Scan(&maxRules)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return mapSQLiteErr(err)
	}
	if maxRules <= 0 {
		return nil
	}
	var current int64
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*)
		FROM rules r
		JOIN monitors m ON m.id = r.monitor_id
		WHERE m.workspace_id = ? AND m.deleted_at IS NULL`, ws,
	).Scan(&current); err != nil {
		return mapSQLiteErr(err)
	}
	if current+delta > maxRules {
		return &QuotaExceededError{
			WorkspaceID: string(ws),
			Resource:    QuotaResourceRules,
			Limit:       maxRules,
			Current:     current,
			Requested:   delta,
		}
	}
	return nil
}

func (s *SQLite) checkChannelQuota(ctx context.Context, tx *sql.Tx, ws workspace.ID, delta int64) error {
	if delta <= 0 {
		return nil
	}
	var maxChannels int64
	err := tx.QueryRowContext(ctx,
		`SELECT max_channels FROM workspace_quotas WHERE workspace_id = ?`, string(ws),
	).Scan(&maxChannels)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return mapSQLiteErr(err)
	}
	if maxChannels <= 0 {
		return nil
	}
	var current int64
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM channels WHERE workspace_id = ?`, ws,
	).Scan(&current); err != nil {
		return mapSQLiteErr(err)
	}
	if current+delta > maxChannels {
		return &QuotaExceededError{
			WorkspaceID: string(ws),
			Resource:    QuotaResourceChannels,
			Limit:       maxChannels,
			Current:     current,
			Requested:   delta,
		}
	}
	return nil
}
