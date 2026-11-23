package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// The SQLite half of escalation policy storage. It mirrors escalation.go
// statement for statement; the differences are the ones SQLite forces:
// positional ? placeholders, timestamps written through sqliteTimeString so
// the fixed layout keeps lexicographic order equal to chronological order,
// and JSON stored as TEXT.

func (s *SQLite) GetEscalationPolicyForMonitor(ctx context.Context, monitorID int64) (*EscalationPolicy, error) {
	return s.escalationPolicyWhere(ctx, "monitor_id = ?", monitorID)
}

func (s *SQLite) GetEscalationPolicy(ctx context.Context, policyID int64) (*EscalationPolicy, error) {
	return s.escalationPolicyWhere(ctx, "id = ?", policyID)
}

// escalationPolicyWhere loads a policy row plus its ordered steps. where is a
// package-internal literal, never caller input, so the concatenation is safe.
func (s *SQLite) escalationPolicyWhere(ctx context.Context, where string, arg any) (*EscalationPolicy, error) {
	var pol EscalationPolicy
	var createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, monitor_id, created_at, updated_at FROM escalation_policies WHERE `+where, arg,
	).Scan(&pol.ID, &pol.MonitorID, &createdAt, &updatedAt)
	if err != nil {
		return nil, mapSQLiteErr(err)
	}
	if pol.CreatedAt, err = parseSQLiteTime(createdAt); err != nil {
		return nil, err
	}
	if pol.UpdatedAt, err = parseSQLiteTime(updatedAt); err != nil {
		return nil, err
	}
	if pol.Steps, err = s.listEscalationSteps(ctx, pol.ID); err != nil {
		return nil, err
	}
	return &pol, nil
}

// listEscalationSteps returns the policy's steps in position order, each with
// its channel ids. It is a single LEFT JOIN so a step with no channels is
// still returned (the API rejects those, but a row could be edited by hand).
func (s *SQLite) listEscalationSteps(ctx context.Context, policyID int64) ([]EscalationStep, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.id, s.position, s.delay_seconds, c.channel_id
		   FROM escalation_steps s
		   LEFT JOIN escalation_step_channels c ON c.step_id = s.id
		  WHERE s.policy_id = ?
		  ORDER BY s.position, c.channel_id`, policyID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []EscalationStep
	index := map[int64]int{}
	for rows.Next() {
		var stepID int64
		var position int
		var delaySeconds int64
		var channelID *int64
		if err := rows.Scan(&stepID, &position, &delaySeconds, &channelID); err != nil {
			return nil, err
		}
		i, ok := index[stepID]
		if !ok {
			i = len(out)
			index[stepID] = i
			out = append(out, EscalationStep{Position: position, DelaySeconds: delaySeconds})
		}
		if channelID != nil {
			out[i].ChannelIDs = append(out[i].ChannelIDs, *channelID)
		}
	}
	return out, rows.Err()
}

// SetEscalationPolicy replaces the monitor's policy and its steps in one
// transaction: the steps are deleted and re-inserted so positions are dense
// and a removed step cannot survive as a gap. An existing policy is updated
// in place, which keeps its id stable for any in-flight escalations.
func (s *SQLite) SetEscalationPolicy(ctx context.Context, monitorID int64, steps []EscalationStep) (*EscalationPolicy, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }() // rollback after commit is a no-op

	now := sqliteTimeString(time.Now())
	var pol EscalationPolicy
	var createdAt, updatedAt string
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO escalation_policies (monitor_id, created_at, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT (monitor_id) DO UPDATE SET updated_at = excluded.updated_at
		 RETURNING id, monitor_id, created_at, updated_at`, monitorID, now, now,
	).Scan(&pol.ID, &pol.MonitorID, &createdAt, &updatedAt); err != nil {
		return nil, mapSQLiteErr(err)
	}
	if pol.CreatedAt, err = parseSQLiteTime(createdAt); err != nil {
		return nil, err
	}
	if pol.UpdatedAt, err = parseSQLiteTime(updatedAt); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM escalation_steps WHERE policy_id = ?`, pol.ID); err != nil {
		return nil, err
	}
	for i, st := range steps {
		channelIDs := uniqueIDs(st.ChannelIDs)
		var stepID int64
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO escalation_steps (policy_id, position, delay_seconds) VALUES (?, ?, ?) RETURNING id`,
			pol.ID, i, st.DelaySeconds,
		).Scan(&stepID); err != nil {
			return nil, err
		}
		for _, cid := range channelIDs {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO escalation_step_channels (step_id, channel_id) VALUES (?, ?)`,
				stepID, cid); err != nil {
				return nil, mapSQLiteErr(err)
			}
		}
		pol.Steps = append(pol.Steps, EscalationStep{Position: i, DelaySeconds: st.DelaySeconds, ChannelIDs: channelIDs})
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if pol.Steps == nil {
		pol.Steps = []EscalationStep{}
	}
	return &pol, nil
}

func (s *SQLite) DeleteEscalationPolicy(ctx context.Context, monitorID int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM escalation_policies WHERE monitor_id = ?`, monitorID)
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

// ScheduleEscalation persists the next due step for an alert. The alert id is
// the primary key, so re-dispatching the same alert replaces its pending
// schedule instead of stacking a second one.
func (s *SQLite) ScheduleEscalation(ctx context.Context, alertID, policyID int64, snapshot json.RawMessage, nextStep int, nextDue time.Time) error {
	// alert_created_at is read from the alert rather than passed in, matching
	// the Postgres statement, where the composite foreign key needs it.
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO alert_escalations (alert_id, alert_created_at, policy_id, alert_snapshot, next_step, next_due_at, updated_at)
		 SELECT a.id, a.created_at, ?, ?, ?, ?, ? FROM alerts a WHERE a.id = ?
		 ON CONFLICT (alert_id) DO UPDATE SET
		     policy_id      = excluded.policy_id,
		     alert_snapshot = excluded.alert_snapshot,
		     next_step      = excluded.next_step,
		     next_due_at    = excluded.next_due_at,
		     completed_at   = NULL,
		     updated_at     = excluded.updated_at`,
		policyID, string(jsonOrEmpty(snapshot)), nextStep,
		sqliteTimeString(nextDue), sqliteTimeString(time.Now()), alertID)
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

// DueEscalations returns escalations whose next step is due. Acknowledged
// alerts and finished escalations are filtered in SQL so a stopped escalation
// can never fire late, even under a backlog.
func (s *SQLite) DueEscalations(ctx context.Context, now time.Time, limit int) ([]EscalationRun, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT e.alert_id, e.policy_id, e.next_step, e.next_due_at, e.alert_snapshot
		   FROM alert_escalations e
		   JOIN alerts a ON a.id = e.alert_id
		   LEFT JOIN rules r ON r.id = a.rule_id
		  WHERE e.completed_at IS NULL
		    AND a.acknowledged_at IS NULL
		    AND (a.rule_id IS NULL OR (r.enabled = 1 AND r.version = a.rule_version))
		    AND e.next_due_at <= ?
		  ORDER BY e.next_due_at, e.alert_id
		  LIMIT ?`, sqliteTimeString(now), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []EscalationRun
	for rows.Next() {
		var r EscalationRun
		var nextDue, snapshot string
		if err := rows.Scan(&r.AlertID, &r.PolicyID, &r.NextStep, &nextDue, &snapshot); err != nil {
			return nil, err
		}
		if r.NextDueAt, err = parseSQLiteTime(nextDue); err != nil {
			return nil, err
		}
		r.Snapshot = json.RawMessage(snapshot)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *SQLite) AdvanceEscalation(ctx context.Context, alertID int64, nextStep int, nextDue time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE alert_escalations SET next_step = ?, next_due_at = ?, updated_at = ?
		  WHERE alert_id = ? AND completed_at IS NULL`,
		nextStep, sqliteTimeString(nextDue), sqliteTimeString(time.Now()), alertID)
	return err
}

func (s *SQLite) CompleteEscalation(ctx context.Context, alertID int64) error {
	now := sqliteTimeString(time.Now())
	_, err := s.db.ExecContext(ctx,
		`UPDATE alert_escalations SET completed_at = ?, updated_at = ? WHERE alert_id = ?`,
		now, now, alertID)
	return err
}

// AcknowledgeAlert stamps the alert and stops its escalation in one
// transaction, so a step cannot fire between the acknowledgement and the
// escalation being cleared. It is idempotent: acknowledging twice is not an
// error, but a missing alert is ErrNotFound.
//
// The alert is scoped to the caller's workspace, so another tenant's id is
// ErrNotFound rather than an acknowledgement that stops its paging. The
// escalation row has no workspace of its own; it is only reached once the
// scoped alert update has found a row in this workspace.
//
// actor is stored beside the timestamp (the column 0034 adds) but is not a
// condition of the update: re-acknowledging an already-acknowledged alert
// still finds the row and still stops any escalation still running, while
// leaving the original actor and time in place rather than rewriting who
// acknowledged it first.
func (s *SQLite) AcknowledgeAlert(ctx context.Context, alertID int64, actor string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // rollback after commit is a no-op

	now := sqliteTimeString(time.Now())
	ws := workspaceID(ctx)
	res, err := tx.ExecContext(ctx,
		`UPDATE alerts SET acknowledged_at = ?, acknowledged_by = ?
		 WHERE id = ? AND workspace_id = ? AND acknowledged_at IS NULL`, now, actor, alertID, ws)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM alerts WHERE id = ? AND workspace_id = ?`, alertID, ws).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE alert_escalations SET completed_at = ?, updated_at = ?
		  WHERE alert_id = ? AND completed_at IS NULL`, now, now, alertID); err != nil {
		return err
	}
	return tx.Commit()
}

// ListChannelsByIDs returns the enabled channels with the given ids, so an
// escalation step can resolve its channel set in one query. SQLite has no
// array parameter, so the id list is expanded into placeholders — the ids are
// int64 values from the step rows, never caller text.
func (s *SQLite) ListChannelsByIDs(ctx context.Context, ids []int64) ([]Channel, error) {
	ids = uniqueIDs(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	return s.queryChannels(ctx,
		`SELECT `+sqliteChannelCols+` FROM channels
		  WHERE id IN (`+placeholders+`) AND enabled = 1
		  ORDER BY id`, args...)
}
