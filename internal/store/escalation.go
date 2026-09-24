package store

import (
	"context"
	"encoding/json"
	"time"
)

// Escalation policy storage. Policies are small (a monitor has at most one)
// and read on every alert, so they are fetched as a whole with their steps
// rather than paged.

func (p *Postgres) GetEscalationPolicyForMonitor(ctx context.Context, monitorID int64) (*EscalationPolicy, error) {
	return p.escalationPolicyWhere(ctx, "monitor_id = $1", monitorID)
}

func (p *Postgres) GetEscalationPolicy(ctx context.Context, policyID int64) (*EscalationPolicy, error) {
	return p.escalationPolicyWhere(ctx, "id = $1", policyID)
}

// escalationPolicyWhere loads a policy row plus its ordered steps. where is a
// package-internal literal, never caller input, so the concatenation is safe.
func (p *Postgres) escalationPolicyWhere(ctx context.Context, where string, arg any) (*EscalationPolicy, error) {
	var pol EscalationPolicy
	err := p.pool.QueryRow(ctx,
		`SELECT id, monitor_id, created_at, updated_at FROM escalation_policies WHERE `+where, arg,
	).Scan(&pol.ID, &pol.MonitorID, &pol.CreatedAt, &pol.UpdatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	pol.Steps, err = p.listEscalationSteps(ctx, pol.ID)
	if err != nil {
		return nil, err
	}
	return &pol, nil
}

// listEscalationSteps returns the policy's steps in position order, each with
// its channel ids. It is a single LEFT JOIN so a step with no channels is
// still returned (the API rejects those, but a row could be edited by hand).
func (p *Postgres) listEscalationSteps(ctx context.Context, policyID int64) ([]EscalationStep, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT s.id, s.position, s.delay_seconds, c.channel_id
		   FROM escalation_steps s
		   LEFT JOIN escalation_step_channels c ON c.step_id = s.id
		  WHERE s.policy_id = $1
		  ORDER BY s.position, c.channel_id`, policyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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
func (p *Postgres) SetEscalationPolicy(ctx context.Context, monitorID int64, steps []EscalationStep) (*EscalationPolicy, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	var pol EscalationPolicy
	if err := tx.QueryRow(ctx,
		`INSERT INTO escalation_policies (monitor_id) VALUES ($1)
		 ON CONFLICT (monitor_id) DO UPDATE SET updated_at = now()
		 RETURNING id, monitor_id, created_at, updated_at`, monitorID,
	).Scan(&pol.ID, &pol.MonitorID, &pol.CreatedAt, &pol.UpdatedAt); err != nil {
		return nil, mapErr(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM escalation_steps WHERE policy_id = $1`, pol.ID); err != nil {
		return nil, err
	}
	for i, st := range steps {
		channelIDs := uniqueIDs(st.ChannelIDs)
		var stepID int64
		if err := tx.QueryRow(ctx,
			`INSERT INTO escalation_steps (policy_id, position, delay_seconds) VALUES ($1, $2, $3) RETURNING id`,
			pol.ID, i, st.DelaySeconds,
		).Scan(&stepID); err != nil {
			return nil, err
		}
		for _, cid := range channelIDs {
			if _, err := tx.Exec(ctx,
				`INSERT INTO escalation_step_channels (step_id, channel_id) VALUES ($1, $2)`,
				stepID, cid); err != nil {
				return nil, mapErr(err)
			}
		}
		pol.Steps = append(pol.Steps, EscalationStep{Position: i, DelaySeconds: st.DelaySeconds, ChannelIDs: channelIDs})
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if pol.Steps == nil {
		pol.Steps = []EscalationStep{}
	}
	return &pol, nil
}

func (p *Postgres) DeleteEscalationPolicy(ctx context.Context, monitorID int64) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM escalation_policies WHERE monitor_id = $1`, monitorID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ScheduleEscalation persists the next due step for an alert. The alert id is
// the primary key, so re-dispatching the same alert replaces its pending
// schedule instead of stacking a second one.
func (p *Postgres) ScheduleEscalation(ctx context.Context, alertID, policyID int64, snapshot json.RawMessage, nextStep int, nextDue time.Time) error {
	var id int64
	return mapErr(p.pool.QueryRow(ctx,
		`INSERT INTO alert_escalations (alert_id, policy_id, alert_snapshot, next_step, next_due_at)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (alert_id) DO UPDATE SET
		     policy_id      = EXCLUDED.policy_id,
		     alert_snapshot = EXCLUDED.alert_snapshot,
		     next_step      = EXCLUDED.next_step,
		     next_due_at    = EXCLUDED.next_due_at,
		     completed_at   = NULL,
		     updated_at     = now()
		 RETURNING alert_id`,
		alertID, policyID, jsonOrEmpty(snapshot), nextStep, nextDue.UTC()).Scan(&id))
}

// DueEscalations returns escalations whose next step is due. Acknowledged
// alerts and finished escalations are filtered in SQL so a stopped escalation
// can never fire late, even under a backlog.
func (p *Postgres) DueEscalations(ctx context.Context, now time.Time, limit int) ([]EscalationRun, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := p.pool.Query(ctx,
		`SELECT e.alert_id, e.policy_id, e.next_step, e.next_due_at, e.alert_snapshot
		   FROM alert_escalations e
		   JOIN alerts a ON a.id = e.alert_id
		  WHERE e.completed_at IS NULL
		    AND a.acknowledged_at IS NULL
		    AND e.next_due_at <= $1
		  ORDER BY e.next_due_at, e.alert_id
		  LIMIT $2`, now.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EscalationRun
	for rows.Next() {
		var r EscalationRun
		if err := rows.Scan(&r.AlertID, &r.PolicyID, &r.NextStep, &r.NextDueAt, &r.Snapshot); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *Postgres) AdvanceEscalation(ctx context.Context, alertID int64, nextStep int, nextDue time.Time) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE alert_escalations SET next_step = $2, next_due_at = $3, updated_at = now()
		  WHERE alert_id = $1 AND completed_at IS NULL`, alertID, nextStep, nextDue.UTC())
	return err
}

func (p *Postgres) CompleteEscalation(ctx context.Context, alertID int64) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE alert_escalations SET completed_at = now(), updated_at = now() WHERE alert_id = $1`, alertID)
	return err
}

// AcknowledgeAlert stamps the alert and stops its escalation in one
// transaction, so a step cannot fire between the acknowledgement and the
// escalation being cleared. It is idempotent: acknowledging twice is not an
// error, but a missing alert is ErrNotFound.
func (p *Postgres) AcknowledgeAlert(ctx context.Context, alertID int64) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	tag, err := tx.Exec(ctx,
		`UPDATE alerts SET acknowledged_at = now() WHERE id = $1 AND acknowledged_at IS NULL`, alertID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM alerts WHERE id = $1)`, alertID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE alert_escalations SET completed_at = now(), updated_at = now()
		  WHERE alert_id = $1 AND completed_at IS NULL`, alertID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
