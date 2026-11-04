package store

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// DefaultQueryCostBudgetWindow is the fallback refill window (60s) when
// QueryCostBudget > 0 and QueryCostRefillPerSec is not explicitly set.
const DefaultQueryCostBudgetWindow = 60.0

func resolveQuotaWorkspace(ctx context.Context, targetWorkspaceID string) string {
	ws := strings.TrimSpace(targetWorkspaceID)
	if tenantWS, scoped := tenantWorkspace(ctx); scoped {
		ws = string(tenantWS)
	} else if ws == "" {
		ws = string(workspaceID(ctx))
	}
	if ws == "" {
		ws = string(workspace.Default)
	}
	return ws
}

func effectiveRefillRate(budget, refillPerSec float64) float64 {
	if refillPerSec > 0 {
		return refillPerSec
	}
	if budget > 0 {
		return budget / DefaultQueryCostBudgetWindow
	}
	return 0
}

func validateWorkspaceQuota(q *WorkspaceQuota) error {
	if q.MaxMonitors < 0 || q.MaxRules < 0 || q.MaxChannels < 0 || q.MaxSubscriptions < 0 ||
		q.QueryCostBudget < 0 || q.QueryCostRefillPerSec < 0 {
		return &ConstraintError{Kind: ConstraintCheck, Constraint: "workspace_quotas_non_negative"}
	}
	return nil
}

func computeRefilledTokens(currentTokens, budget, refillPerSec float64, updatedAt, now time.Time) float64 {
	if budget <= 0 {
		return 0
	}
	rate := effectiveRefillRate(budget, refillPerSec)
	elapsed := now.Sub(updatedAt).Seconds()
	if elapsed > 0 && rate > 0 {
		currentTokens += elapsed * rate
	}
	if currentTokens > budget {
		currentTokens = budget
	}
	if currentTokens < 0 {
		currentTokens = 0
	}
	return currentTokens
}

func computeRetryAfter(deficit, refillPerSec float64) time.Duration {
	if deficit <= 0 || refillPerSec <= 0 {
		return time.Second
	}
	secs := int(math.Ceil(deficit / refillPerSec))
	if secs < 1 {
		secs = 1
	}
	return time.Duration(secs) * time.Second
}

// SafeGetWorkspaceQuota calls st.GetWorkspaceQuota while tolerating test fakes
// that embed a nil store.Store.
func SafeGetWorkspaceQuota(ctx context.Context, st Quotas, workspaceID string) (q WorkspaceQuota, err error) {
	ws := resolveQuotaWorkspace(ctx, workspaceID)
	if st == nil {
		return WorkspaceQuota{WorkspaceID: ws}, nil
	}
	defer func() {
		if r := recover(); r != nil {
			q = WorkspaceQuota{WorkspaceID: ws}
			err = nil
		}
	}()
	return st.GetWorkspaceQuota(ctx, workspaceID)
}

// SafeSetWorkspaceQuota calls st.SetWorkspaceQuota while tolerating test fakes
// that embed a nil store.Store.
func SafeSetWorkspaceQuota(ctx context.Context, st Quotas, input WorkspaceQuota) (q WorkspaceQuota, err error) {
	if st == nil {
		return input, nil
	}
	defer func() {
		if r := recover(); r != nil {
			q = input
			err = nil
		}
	}()
	return st.SetWorkspaceQuota(ctx, input)
}

// SafeListWorkspaceQuotas calls st.ListWorkspaceQuotas while tolerating test
// fakes that embed a nil store.Store.
func SafeListWorkspaceQuotas(ctx context.Context, st Quotas) (out []WorkspaceQuota, err error) {
	if st == nil {
		return nil, nil
	}
	defer func() {
		if r := recover(); r != nil {
			out = nil
			err = nil
		}
	}()
	return st.ListWorkspaceQuotas(ctx)
}

// SafeConsumeWorkspaceQueryBudget calls st.ConsumeWorkspaceQueryBudget while
// tolerating test fakes that embed a nil store.Store.
func SafeConsumeWorkspaceQueryBudget(ctx context.Context, st Quotas, workspaceID string, cost float64, now time.Time) (dec WorkspaceQueryBudgetDecision, err error) {
	ws := resolveQuotaWorkspace(ctx, workspaceID)
	if st == nil {
		return WorkspaceQueryBudgetDecision{WorkspaceID: ws, Allowed: true}, nil
	}
	defer func() {
		if r := recover(); r != nil {
			dec = WorkspaceQueryBudgetDecision{WorkspaceID: ws, Allowed: true}
			err = nil
		}
	}()
	return st.ConsumeWorkspaceQueryBudget(ctx, workspaceID, cost, now)
}

// SafeReconcileWorkspaceQueryBudget calls st.ReconcileWorkspaceQueryBudget while
// tolerating test fakes that embed a nil store.Store.
func SafeReconcileWorkspaceQueryBudget(ctx context.Context, st Quotas, workspaceID string, deltaCost float64, now time.Time) (remaining float64, err error) {
	if st == nil {
		return 0, nil
	}
	defer func() {
		if r := recover(); r != nil {
			remaining = 0
			err = nil
		}
	}()
	return st.ReconcileWorkspaceQueryBudget(ctx, workspaceID, deltaCost, now)
}

// --- Postgres implementation ---

func (p *Postgres) GetWorkspaceQuota(ctx context.Context, targetWorkspaceID string) (WorkspaceQuota, error) {
	ws := resolveQuotaWorkspace(ctx, targetWorkspaceID)
	q := WorkspaceQuota{WorkspaceID: ws}
	err := p.pool.QueryRow(ctx, `
		SELECT workspace_id, max_monitors, max_rules, max_channels, max_subscriptions,
		       query_cost_budget, query_cost_refill_per_sec, updated_at
		FROM workspace_quotas
		WHERE workspace_id = $1`, ws,
	).Scan(&q.WorkspaceID, &q.MaxMonitors, &q.MaxRules, &q.MaxChannels, &q.MaxSubscriptions,
		&q.QueryCostBudget, &q.QueryCostRefillPerSec, &q.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return WorkspaceQuota{WorkspaceID: ws}, nil
		}
		return q, mapErr(err)
	}
	return q, nil
}

func (p *Postgres) SetWorkspaceQuota(ctx context.Context, q WorkspaceQuota) (WorkspaceQuota, error) {
	ws := resolveQuotaWorkspace(ctx, q.WorkspaceID)
	q.WorkspaceID = ws
	if err := validateWorkspaceQuota(&q); err != nil {
		return q, err
	}
	if q.QueryCostBudget > 0 && q.QueryCostRefillPerSec <= 0 {
		q.QueryCostRefillPerSec = effectiveRefillRate(q.QueryCostBudget, 0)
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return q, mapErr(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	if _, err := tx.Exec(ctx,
		`INSERT INTO workspaces (id) VALUES ($1) ON CONFLICT (id) DO NOTHING`, ws); err != nil {
		return q, mapErr(err)
	}

	now := time.Now().UTC()
	if err := tx.QueryRow(ctx, `
		INSERT INTO workspace_quotas (
			workspace_id, max_monitors, max_rules, max_channels, max_subscriptions,
			query_cost_budget, query_cost_refill_per_sec, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (workspace_id) DO UPDATE SET
			max_monitors              = EXCLUDED.max_monitors,
			max_rules                 = EXCLUDED.max_rules,
			max_channels              = EXCLUDED.max_channels,
			max_subscriptions         = EXCLUDED.max_subscriptions,
			query_cost_budget         = EXCLUDED.query_cost_budget,
			query_cost_refill_per_sec = EXCLUDED.query_cost_refill_per_sec,
			updated_at                = EXCLUDED.updated_at
		RETURNING updated_at`,
		ws, q.MaxMonitors, q.MaxRules, q.MaxChannels, q.MaxSubscriptions,
		q.QueryCostBudget, q.QueryCostRefillPerSec, now,
	).Scan(&q.UpdatedAt); err != nil {
		return q, mapErr(err)
	}

	if q.QueryCostBudget > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workspace_query_budget_state (workspace_id, tokens, updated_at)
			VALUES ($1, $2, $3)
			ON CONFLICT (workspace_id) DO UPDATE SET
				tokens     = LEAST(workspace_query_budget_state.tokens, EXCLUDED.tokens),
				updated_at = EXCLUDED.updated_at`,
			ws, q.QueryCostBudget, now,
		); err != nil {
			return q, mapErr(err)
		}
	} else {
		if _, err := tx.Exec(ctx,
			`DELETE FROM workspace_query_budget_state WHERE workspace_id = $1`, ws); err != nil {
			return q, mapErr(err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return q, mapErr(err)
	}
	return q, nil
}

func (p *Postgres) DeleteWorkspaceQuota(ctx context.Context, targetWorkspaceID string) error {
	ws := resolveQuotaWorkspace(ctx, targetWorkspaceID)
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return mapErr(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	if _, err := tx.Exec(ctx, `DELETE FROM workspace_query_budget_state WHERE workspace_id = $1`, ws); err != nil {
		return mapErr(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM workspace_quotas WHERE workspace_id = $1`, ws); err != nil {
		return mapErr(err)
	}
	return mapErr(tx.Commit(ctx))
}

func (p *Postgres) ListWorkspaceQuotas(ctx context.Context) ([]WorkspaceQuota, error) {
	q := `
		SELECT workspace_id, max_monitors, max_rules, max_channels, max_subscriptions,
		       query_cost_budget, query_cost_refill_per_sec, updated_at
		FROM workspace_quotas`
	var args []any
	if ws, scoped := tenantWorkspace(ctx); scoped {
		q += ` WHERE workspace_id = $1`
		args = append(args, string(ws))
	}
	q += ` ORDER BY workspace_id ASC`
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var out []WorkspaceQuota
	for rows.Next() {
		var item WorkspaceQuota
		if err := rows.Scan(
			&item.WorkspaceID, &item.MaxMonitors, &item.MaxRules, &item.MaxChannels,
			&item.MaxSubscriptions, &item.QueryCostBudget, &item.QueryCostRefillPerSec, &item.UpdatedAt,
		); err != nil {
			return nil, mapErr(err)
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (p *Postgres) ConsumeWorkspaceQueryBudget(ctx context.Context, targetWorkspaceID string, cost float64, now time.Time) (WorkspaceQueryBudgetDecision, error) {
	ws := resolveQuotaWorkspace(ctx, targetWorkspaceID)
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return WorkspaceQueryBudgetDecision{}, mapErr(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	var budget, refillPerSec float64
	err = tx.QueryRow(ctx, `
		SELECT query_cost_budget, query_cost_refill_per_sec
		FROM workspace_quotas
		WHERE workspace_id = $1
		FOR UPDATE`, ws,
	).Scan(&budget, &refillPerSec)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return WorkspaceQueryBudgetDecision{WorkspaceID: ws, Allowed: true, Configured: false}, nil
		}
		return WorkspaceQueryBudgetDecision{}, mapErr(err)
	}
	if budget <= 0 {
		return WorkspaceQueryBudgetDecision{WorkspaceID: ws, Allowed: true, Configured: false}, nil
	}
	rate := effectiveRefillRate(budget, refillPerSec)

	var tokens float64
	var updatedAt time.Time
	err = tx.QueryRow(ctx, `
		SELECT tokens, updated_at
		FROM workspace_query_budget_state
		WHERE workspace_id = $1
		FOR UPDATE`, ws,
	).Scan(&tokens, &updatedAt)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return WorkspaceQueryBudgetDecision{}, mapErr(err)
		}
		tokens = budget
		updatedAt = now
	} else {
		tokens = computeRefilledTokens(tokens, budget, rate, updatedAt, now)
	}

	if cost <= 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workspace_query_budget_state (workspace_id, tokens, updated_at)
			VALUES ($1, $2, $3)
			ON CONFLICT (workspace_id) DO UPDATE SET tokens = EXCLUDED.tokens, updated_at = EXCLUDED.updated_at`,
			ws, tokens, now,
		); err != nil {
			return WorkspaceQueryBudgetDecision{}, mapErr(err)
		}
		if err := tx.Commit(ctx); err != nil {
			return WorkspaceQueryBudgetDecision{}, mapErr(err)
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
		if _, err := tx.Exec(ctx, `
			INSERT INTO workspace_query_budget_state (workspace_id, tokens, updated_at)
			VALUES ($1, $2, $3)
			ON CONFLICT (workspace_id) DO UPDATE SET tokens = EXCLUDED.tokens, updated_at = EXCLUDED.updated_at`,
			ws, tokens, now,
		); err != nil {
			return WorkspaceQueryBudgetDecision{}, mapErr(err)
		}
		if err := tx.Commit(ctx); err != nil {
			return WorkspaceQueryBudgetDecision{}, mapErr(err)
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
	if _, err := tx.Exec(ctx, `
		INSERT INTO workspace_query_budget_state (workspace_id, tokens, updated_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id) DO UPDATE SET tokens = EXCLUDED.tokens, updated_at = EXCLUDED.updated_at`,
		ws, tokens, now,
	); err != nil {
		return WorkspaceQueryBudgetDecision{}, mapErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkspaceQueryBudgetDecision{}, mapErr(err)
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

func (p *Postgres) ReconcileWorkspaceQueryBudget(ctx context.Context, targetWorkspaceID string, deltaCost float64, now time.Time) (float64, error) {
	ws := resolveQuotaWorkspace(ctx, targetWorkspaceID)
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, mapErr(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	var budget, refillPerSec float64
	err = tx.QueryRow(ctx, `
		SELECT query_cost_budget, query_cost_refill_per_sec
		FROM workspace_quotas
		WHERE workspace_id = $1
		FOR UPDATE`, ws,
	).Scan(&budget, &refillPerSec)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, mapErr(err)
	}
	if budget <= 0 {
		return 0, nil
	}
	rate := effectiveRefillRate(budget, refillPerSec)

	var tokens float64
	var updatedAt time.Time
	err = tx.QueryRow(ctx, `
		SELECT tokens, updated_at
		FROM workspace_query_budget_state
		WHERE workspace_id = $1
		FOR UPDATE`, ws,
	).Scan(&tokens, &updatedAt)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, mapErr(err)
		}
		tokens = budget
	} else {
		tokens = computeRefilledTokens(tokens, budget, rate, updatedAt, now)
	}

	tokens -= deltaCost
	if tokens > budget {
		tokens = budget
	}
	if tokens < 0 {
		tokens = 0
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workspace_query_budget_state (workspace_id, tokens, updated_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id) DO UPDATE SET tokens = EXCLUDED.tokens, updated_at = EXCLUDED.updated_at`,
		ws, tokens, now,
	); err != nil {
		return 0, mapErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, mapErr(err)
	}
	return tokens, nil
}

func (p *Postgres) checkMonitorQuota(ctx context.Context, tx pgx.Tx, ws workspace.ID, delta int64) error {
	if delta <= 0 {
		return nil
	}
	var maxMonitors int64
	err := tx.QueryRow(ctx,
		`SELECT max_monitors FROM workspace_quotas WHERE workspace_id = $1 FOR UPDATE`, string(ws),
	).Scan(&maxMonitors)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return mapErr(err)
	}
	if maxMonitors <= 0 {
		return nil
	}
	var current int64
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM monitors WHERE workspace_id = $1 AND deleted_at IS NULL`, ws,
	).Scan(&current); err != nil {
		return mapErr(err)
	}
	if current+delta > maxMonitors {
		if p.metrics != nil {
			p.metrics.RecordWorkspaceQuotaRejection(string(ws), QuotaResourceMonitors)
		}
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

func (p *Postgres) checkRuleQuota(ctx context.Context, tx pgx.Tx, ws workspace.ID, delta int64) error {
	if delta <= 0 {
		return nil
	}
	var maxRules int64
	err := tx.QueryRow(ctx,
		`SELECT max_rules FROM workspace_quotas WHERE workspace_id = $1 FOR UPDATE`, string(ws),
	).Scan(&maxRules)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return mapErr(err)
	}
	if maxRules <= 0 {
		return nil
	}
	var current int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*)
		FROM rules r
		JOIN monitors m ON m.id = r.monitor_id
		WHERE m.workspace_id = $1 AND m.deleted_at IS NULL`, ws,
	).Scan(&current); err != nil {
		return mapErr(err)
	}
	if current+delta > maxRules {
		if p.metrics != nil {
			p.metrics.RecordWorkspaceQuotaRejection(string(ws), QuotaResourceRules)
		}
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

func (p *Postgres) checkChannelQuota(ctx context.Context, tx pgx.Tx, ws workspace.ID, delta int64) error {
	if delta <= 0 {
		return nil
	}
	var maxChannels int64
	err := tx.QueryRow(ctx,
		`SELECT max_channels FROM workspace_quotas WHERE workspace_id = $1 FOR UPDATE`, string(ws),
	).Scan(&maxChannels)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return mapErr(err)
	}
	if maxChannels <= 0 {
		return nil
	}
	var current int64
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM channels WHERE workspace_id = $1`, ws,
	).Scan(&current); err != nil {
		return mapErr(err)
	}
	if current+delta > maxChannels {
		if p.metrics != nil {
			p.metrics.RecordWorkspaceQuotaRejection(string(ws), QuotaResourceChannels)
		}
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
