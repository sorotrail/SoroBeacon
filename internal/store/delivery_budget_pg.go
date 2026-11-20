package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// loadDeliveryBudgetPG reads a monitor's budget inside tx, locking the row so
// concurrent consumers serialise on it.
func loadDeliveryBudgetPG(ctx context.Context, tx pgx.Tx, monitorID int64) (DeliveryBudget, error) {
	b := DeliveryBudget{MonitorID: monitorID}
	var start *time.Time
	err := tx.QueryRow(ctx,
		`SELECT delivery_budget, budget_window_start, budget_used, budget_exhausted_at
		   FROM monitors WHERE id = $1 FOR UPDATE`, monitorID,
	).Scan(&b.Limit, &start, &b.Used, &b.ExhaustedAt)
	if err != nil {
		return b, mapErr(err)
	}
	if start != nil {
		b.WindowStart = start.UTC()
	}
	return b, nil
}

func (p *Postgres) GetDeliveryBudget(ctx context.Context, monitorID int64, now time.Time) (*DeliveryBudget, error) {
	b := DeliveryBudget{MonitorID: monitorID}
	var start *time.Time
	err := p.pool.QueryRow(ctx,
		`SELECT delivery_budget, budget_window_start, budget_used, budget_exhausted_at
		   FROM monitors WHERE id = $1 AND workspace_id = $2`, monitorID, workspaceID(ctx),
	).Scan(&b.Limit, &start, &b.Used, &b.ExhaustedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	if start != nil {
		b.WindowStart = start.UTC()
	}
	b = rollBudget(b, now)
	return &b, nil
}

func (p *Postgres) SetDeliveryBudget(ctx context.Context, monitorID int64, limit *int) error {
	if limit != nil && *limit <= 0 {
		return ErrInvalidDeliveryBudget
	}
	// Clearing the budget also clears the counter, so re-enabling later starts
	// from a clean window rather than inheriting a stale spent count.
	// Raising the limit above what is used un-exhausts; the CASE keeps an
	// exhaustion that the new limit still covers.
	tag, err := p.pool.Exec(ctx,
		`UPDATE monitors SET
		        delivery_budget     = $2::integer,
		        budget_used         = CASE WHEN $2::integer IS NULL THEN 0 ELSE budget_used END,
		        budget_window_start = CASE WHEN $2::integer IS NULL THEN NULL ELSE budget_window_start END,
		        budget_exhausted_at = CASE WHEN $2::integer IS NULL OR budget_used < $2::integer
		                                   THEN NULL ELSE budget_exhausted_at END
		  WHERE id = $1 AND workspace_id = $3`, monitorID, limit, workspaceID(ctx))
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) ConsumeDelivery(ctx context.Context, monitorID int64, now time.Time) (BudgetDecision, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return BudgetDecision{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	cur, err := loadDeliveryBudgetPG(ctx, tx, monitorID)
	if err != nil {
		return BudgetDecision{}, err
	}
	if cur.Limit == nil {
		// Unlimited: no write, so the common case costs one locked read.
		return BudgetDecision{Allowed: true}, nil
	}
	next, decision := decideDelivery(cur, now)
	if _, err := tx.Exec(ctx,
		`UPDATE monitors SET budget_window_start = $2, budget_used = $3, budget_exhausted_at = $4 WHERE id = $1`,
		monitorID, next.WindowStart, next.Used, next.ExhaustedAt,
	); err != nil {
		return BudgetDecision{}, mapErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return BudgetDecision{}, err
	}
	return decision, nil
}

func (p *Postgres) ResetDeliveryBudget(ctx context.Context, monitorID int64, now time.Time) error {
	tag, err := p.pool.Exec(ctx,
		`UPDATE monitors SET budget_window_start = $2, budget_used = 0, budget_exhausted_at = NULL WHERE id = $1 AND workspace_id = $3`,
		monitorID, budgetWindowStart(now), workspaceID(ctx))
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
