package store

import (
	"context"
	"database/sql"
	"time"
)

// The SQLite half of the delivery budget. It mirrors delivery_budget_pg.go;
// the differences are the ones SQLite forces: ? placeholders, timestamps
// through sqliteTimeString, and no row lock — a transaction that writes
// first takes SQLite's single write lock, which gives the same
// serialisation FOR UPDATE gives Postgres.

func scanSQLiteBudget(monitorID int64, limit sql.NullInt64, start sql.NullString, used int, exhausted sql.NullString) (DeliveryBudget, error) {
	b := DeliveryBudget{MonitorID: monitorID, Used: used}
	if limit.Valid {
		l := int(limit.Int64)
		b.Limit = &l
	}
	var err error
	if start.Valid {
		if b.WindowStart, err = parseSQLiteTime(start.String); err != nil {
			return b, err
		}
	}
	if b.ExhaustedAt, err = parseSQLiteTimePtr(exhausted); err != nil {
		return b, err
	}
	return b, nil
}

func (s *SQLite) GetDeliveryBudget(ctx context.Context, monitorID int64, now time.Time) (*DeliveryBudget, error) {
	var (
		limit     sql.NullInt64
		start     sql.NullString
		exhausted sql.NullString
		used      int
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT delivery_budget, budget_window_start, budget_used, budget_exhausted_at
		   FROM monitors WHERE id = ? AND workspace_id = ?`, monitorID, workspaceID(ctx),
	).Scan(&limit, &start, &used, &exhausted)
	if err != nil {
		return nil, mapSQLiteErr(err)
	}
	b, err := scanSQLiteBudget(monitorID, limit, start, used, exhausted)
	if err != nil {
		return nil, err
	}
	b = rollBudget(b, now)
	return &b, nil
}

func (s *SQLite) SetDeliveryBudget(ctx context.Context, monitorID int64, limit *int) error {
	if limit != nil && *limit <= 0 {
		return ErrInvalidDeliveryBudget
	}
	var arg any
	if limit != nil {
		arg = *limit
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE monitors SET
		        delivery_budget     = ?1,
		        budget_used         = CASE WHEN ?1 IS NULL THEN 0 ELSE budget_used END,
		        budget_window_start = CASE WHEN ?1 IS NULL THEN NULL ELSE budget_window_start END,
		        budget_exhausted_at = CASE WHEN ?1 IS NULL OR budget_used < ?1
		                                   THEN NULL ELSE budget_exhausted_at END
		  WHERE id = ?2 AND workspace_id = ?3`, arg, monitorID, workspaceID(ctx))
	if err != nil {
		return mapSQLiteErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLite) ConsumeDelivery(ctx context.Context, monitorID int64, now time.Time) (BudgetDecision, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BudgetDecision{}, mapSQLiteErr(err)
	}
	defer func() { _ = tx.Rollback() }()

	// A no-op write first, so the transaction holds the write lock before it
	// reads; reading first would let two consumers both see the last unit.
	res, err := tx.ExecContext(ctx, `UPDATE monitors SET budget_used = budget_used WHERE id = ?`, monitorID)
	if err != nil {
		return BudgetDecision{}, mapSQLiteErr(err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return BudgetDecision{}, err
	} else if n == 0 {
		return BudgetDecision{}, ErrNotFound
	}

	var (
		limit     sql.NullInt64
		start     sql.NullString
		exhausted sql.NullString
		used      int
	)
	if err := tx.QueryRowContext(ctx,
		`SELECT delivery_budget, budget_window_start, budget_used, budget_exhausted_at FROM monitors WHERE id = ?`, monitorID,
	).Scan(&limit, &start, &used, &exhausted); err != nil {
		return BudgetDecision{}, mapSQLiteErr(err)
	}
	cur, err := scanSQLiteBudget(monitorID, limit, start, used, exhausted)
	if err != nil {
		return BudgetDecision{}, err
	}
	if cur.Limit == nil {
		return BudgetDecision{Allowed: true}, nil
	}
	next, decision := decideDelivery(cur, now)
	if _, err := tx.ExecContext(ctx,
		`UPDATE monitors SET budget_window_start = ?, budget_used = ?, budget_exhausted_at = ? WHERE id = ?`,
		sqliteTimeString(next.WindowStart), next.Used, sqliteNullTime(next.ExhaustedAt), monitorID,
	); err != nil {
		return BudgetDecision{}, mapSQLiteErr(err)
	}
	if err := tx.Commit(); err != nil {
		return BudgetDecision{}, mapSQLiteErr(err)
	}
	return decision, nil
}

func (s *SQLite) ResetDeliveryBudget(ctx context.Context, monitorID int64, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE monitors SET budget_window_start = ?, budget_used = 0, budget_exhausted_at = NULL WHERE id = ? AND workspace_id = ?`,
		sqliteTimeString(budgetWindowStart(now)), monitorID, workspaceID(ctx))
	if err != nil {
		return mapSQLiteErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
