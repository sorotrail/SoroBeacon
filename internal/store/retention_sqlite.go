package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// WithAlertRetention sets the instance-wide fallback alert retention on SQLite.
func (s *SQLite) WithAlertRetention(d time.Duration) *SQLite {
	s.SetAlertRetention(d)
	return s
}

// SetAlertRetention updates the instance-wide fallback alert retention on SQLite.
func (s *SQLite) SetAlertRetention(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.alertRetention.Store(int64(d))
}

// InstanceAlertRetention returns the instance-wide fallback alert retention on SQLite.
func (s *SQLite) InstanceAlertRetention() time.Duration {
	return time.Duration(s.alertRetention.Load())
}

// GetWorkspaceRetentionPolicy returns the retention policy for ws, falling back
// to the store's instance-wide alert retention when no workspace-specific row
// exists.
func (s *SQLite) GetWorkspaceRetentionPolicy(ctx context.Context, ws workspace.ID) (WorkspaceRetentionPolicy, error) {
	if ws == "" {
		ws = workspaceID(ctx)
	}
	now := time.Now().UTC()
	var (
		seconds   int64
		updatedBy string
		updatedAt string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT retention_seconds, updated_by, updated_at
		FROM workspace_retention_policies
		WHERE workspace_id = ?`, string(ws)).Scan(&seconds, &updatedBy, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return resolvePolicyFields(ws, 0, false, "", nil, s.InstanceAlertRetention(), now), nil
	}
	if err != nil {
		return WorkspaceRetentionPolicy{}, mapSQLiteErr(err)
	}
	var ua *time.Time
	if updatedAt != "" {
		if t, perr := parseSQLiteTime(updatedAt); perr == nil {
			u := t.UTC()
			ua = &u
		}
	}
	return resolvePolicyFields(ws, seconds, true, updatedBy, ua, s.InstanceAlertRetention(), now), nil
}

// SetWorkspaceRetentionPolicy creates or updates a workspace's alert retention
// policy on SQLite. Shortening requires ConfirmShorten=true and records an
// audit_log entry.
func (s *SQLite) SetWorkspaceRetentionPolicy(ctx context.Context, in SetWorkspaceRetentionInput) (WorkspaceRetentionPolicy, error) {
	ws := normalizeRetentionWorkspace(ctx, in.WorkspaceID)
	d := in.Retention.Truncate(time.Second)
	if d < time.Second {
		return WorkspaceRetentionPolicy{}, ErrInvalidRetention
	}
	newSec := int64(d / time.Second)

	instRetention := in.InstanceRetention
	if instRetention <= 0 {
		instRetention = s.InstanceAlertRetention()
	}
	instSec := int64(instRetention.Truncate(time.Second) / time.Second)

	if err := s.EnsureWorkspace(ctx, ws); err != nil {
		return WorkspaceRetentionPolicy{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkspaceRetentionPolicy{}, mapSQLiteErr(err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		prevSec        int64
		prevConfigured bool
	)
	err = tx.QueryRowContext(ctx, `
		SELECT retention_seconds
		FROM workspace_retention_policies
		WHERE workspace_id = ?`, string(ws)).Scan(&prevSec)
	switch {
	case err == nil:
		prevConfigured = true
	case errors.Is(err, sql.ErrNoRows):
		prevConfigured = false
	default:
		return WorkspaceRetentionPolicy{}, mapSQLiteErr(err)
	}

	prevEffSec := prevSec
	if !prevConfigured {
		prevEffSec = instSec
	}
	shortened := (prevConfigured && newSec < prevSec) || (prevEffSec > 0 && newSec < prevEffSec)
	if shortened && !in.ConfirmShorten {
		return WorkspaceRetentionPolicy{}, ErrRetentionShortenUnconfirmed
	}

	actor := strings.TrimSpace(in.Actor)
	if actor == "" {
		actor = string(ws)
	}
	now := time.Now().UTC()
	nowStr := sqliteTimeString(now)

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workspace_retention_policies (workspace_id, retention_seconds, updated_by, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (workspace_id) DO UPDATE SET
			retention_seconds = excluded.retention_seconds,
			updated_by        = excluded.updated_by,
			updated_at        = excluded.updated_at`,
		string(ws), newSec, actor, nowStr); err != nil {
		return WorkspaceRetentionPolicy{}, mapSQLiteErr(err)
	}

	diffPayload, _ := json.Marshal(map[string]any{
		"workspace_id":               string(ws),
		"previous_retention_seconds": prevSec,
		"previous_effective_seconds": prevEffSec,
		"new_retention_seconds":      newSec,
		"new_effective_seconds":      newSec,
		"shortened":                  shortened,
		"confirm_shorten":            in.ConfirmShorten,
		"policy_source":              RetentionPolicySourceWorkspace,
	})
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_log (actor, action, target_type, target_id, diff) VALUES (?, ?, ?, ?, ?)`,
		actor, AuditActionUpdate, AuditTargetWorkspaceRetention, int64(0), string(diffPayload)); err != nil {
		return WorkspaceRetentionPolicy{}, mapSQLiteErr(err)
	}

	if err := tx.Commit(); err != nil {
		return WorkspaceRetentionPolicy{}, mapSQLiteErr(err)
	}

	return resolvePolicyFields(ws, newSec, true, actor, &now, instRetention, now), nil
}

// DeleteWorkspaceRetentionPolicy removes a workspace's explicit retention
// policy on SQLite so it reverts to the instance-wide ALERT_RETENTION fallback.
func (s *SQLite) DeleteWorkspaceRetentionPolicy(ctx context.Context, ws workspace.ID, confirmShorten bool, actor string) (WorkspaceRetentionPolicy, error) {
	if ws == "" {
		ws = workspaceID(ctx)
	}
	instRetention := s.InstanceAlertRetention()
	instSec := int64(instRetention.Truncate(time.Second) / time.Second)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkspaceRetentionPolicy{}, mapSQLiteErr(err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		prevSec        int64
		prevConfigured bool
	)
	err = tx.QueryRowContext(ctx, `
		SELECT retention_seconds
		FROM workspace_retention_policies
		WHERE workspace_id = ?`, string(ws)).Scan(&prevSec)
	switch {
	case err == nil:
		prevConfigured = true
	case errors.Is(err, sql.ErrNoRows):
		prevConfigured = false
	default:
		return WorkspaceRetentionPolicy{}, mapSQLiteErr(err)
	}

	shortened := prevConfigured && instSec > 0 && instSec < prevSec
	if shortened && !confirmShorten {
		return WorkspaceRetentionPolicy{}, ErrRetentionShortenUnconfirmed
	}

	if prevConfigured {
		if _, err := tx.ExecContext(ctx, `DELETE FROM workspace_retention_policies WHERE workspace_id = ?`, string(ws)); err != nil {
			return WorkspaceRetentionPolicy{}, mapSQLiteErr(err)
		}
		actorName := strings.TrimSpace(actor)
		if actorName == "" {
			actorName = string(ws)
		}
		fallbackSource := RetentionPolicySourceUnset
		if instSec > 0 {
			fallbackSource = RetentionPolicySourceInstance
		}
		diffPayload, _ := json.Marshal(map[string]any{
			"workspace_id":               string(ws),
			"previous_retention_seconds": prevSec,
			"previous_effective_seconds": prevSec,
			"new_retention_seconds":      int64(0),
			"new_effective_seconds":      instSec,
			"shortened":                  shortened,
			"confirm_shorten":            confirmShorten,
			"policy_source":              fallbackSource,
		})
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO audit_log (actor, action, target_type, target_id, diff) VALUES (?, ?, ?, ?, ?)`,
			actorName, AuditActionDelete, AuditTargetWorkspaceRetention, int64(0), string(diffPayload)); err != nil {
			return WorkspaceRetentionPolicy{}, mapSQLiteErr(err)
		}
	}

	if err := tx.Commit(); err != nil {
		return WorkspaceRetentionPolicy{}, mapSQLiteErr(err)
	}

	return resolvePolicyFields(ws, 0, false, "", nil, instRetention, time.Now().UTC()), nil
}

// ListWorkspaceRetentionPolicies returns the resolved retention policy for
// every known workspace on the SQLite instance, ordered by workspace_id.
func (s *SQLite) ListWorkspaceRetentionPolicies(ctx context.Context) ([]WorkspaceRetentionPolicy, error) {
	now := time.Now().UTC()
	instRetention := s.InstanceAlertRetention()
	rows, err := s.db.QueryContext(ctx, `
		WITH all_workspaces AS (
			SELECT id AS workspace_id FROM workspaces
			UNION
			SELECT workspace_id FROM workspace_retention_policies
		)
		SELECT w.workspace_id,
		       COALESCE(rp.retention_seconds, 0),
		       CASE WHEN rp.workspace_id IS NOT NULL THEN 1 ELSE 0 END,
		       COALESCE(rp.updated_by, ''),
		       COALESCE(rp.updated_at, '')
		FROM all_workspaces w
		LEFT JOIN workspace_retention_policies rp ON rp.workspace_id = w.workspace_id
		ORDER BY w.workspace_id ASC`)
	if err != nil {
		return nil, mapSQLiteErr(err)
	}
	defer func() { _ = rows.Close() }()

	var out []WorkspaceRetentionPolicy
	for rows.Next() {
		var (
			wsID       string
			sec        int64
			configured int64
			updatedBy  string
			updatedAt  string
		)
		if err := rows.Scan(&wsID, &sec, &configured, &updatedBy, &updatedAt); err != nil {
			return nil, mapSQLiteErr(err)
		}
		var ua *time.Time
		if updatedAt != "" {
			if t, perr := parseSQLiteTime(updatedAt); perr == nil {
				u := t.UTC()
				ua = &u
			}
		}
		out = append(out, resolvePolicyFields(workspace.ID(wsID), sec, configured != 0, updatedBy, ua, instRetention, now))
	}
	return out, rows.Err()
}

// ExpiredAlertsForWorkspace returns up to limit alerts in ws older than cutoff,
// oldest first, matching DeleteExpiredAlertsForWorkspace's ordering.
func (s *SQLite) ExpiredAlertsForWorkspace(ctx context.Context, ws workspace.ID, cutoff time.Time, limit int) ([]Alert, error) {
	if limit <= 0 {
		limit = DefaultPruneBatch
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, monitor_id, rule_id, event_id, payload, enrichment, created_at, ledger, retracted_at, inhibited_by_rule_id, suppressed, suppression_reason, acknowledged_at, acknowledged_by, network, federated, federation_origin, source_monitor_id, source_rule_id, rule_version, runbook_url, response_guidance
		   FROM alerts
		  WHERE workspace_id = ? AND created_at < ?
		  ORDER BY created_at ASC, id ASC
		  LIMIT ?`,
		string(ws), sqliteTimeString(cutoff.UTC()), limit)
	if err != nil {
		return nil, mapSQLiteErr(err)
	}
	defer func() { _ = rows.Close() }()
	var out []Alert
	for rows.Next() {
		a, err := scanSQLiteAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteExpiredAlertsForWorkspace removes up to limit alerts in ws older than
// cutoff on SQLite. Child rows in delivery_attempts, alert_escalations, and
// dead_letters follow via ON DELETE CASCADE (enabled by _pragma=foreign_keys(1)),
// while alert uniqueness on SQLite lives directly on alerts(rule_id, event_id).
func (s *SQLite) DeleteExpiredAlertsForWorkspace(ctx context.Context, ws workspace.ID, cutoff time.Time, limit int) (int64, error) {
	if limit <= 0 {
		limit = DefaultPruneBatch
	}
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM alerts
		WHERE workspace_id = ? AND id IN (
			SELECT id FROM alerts
			WHERE workspace_id = ? AND created_at < ?
			ORDER BY created_at ASC, id ASC
			LIMIT ?
		)`, string(ws), string(ws), sqliteTimeString(cutoff.UTC()), limit)
	if err != nil {
		return 0, mapSQLiteErr(err)
	}
	return res.RowsAffected()
}

// workspaceAlertCutoff resolves the active read-time retention cutoff for the
// tenant workspace in ctx on SQLite.
func (s *SQLite) workspaceAlertCutoff(ctx context.Context) (time.Time, WorkspaceRetentionPolicy, bool) {
	ws, scoped := tenantWorkspace(ctx)
	if !scoped {
		return time.Time{}, WorkspaceRetentionPolicy{}, false
	}
	policy, err := s.GetWorkspaceRetentionPolicy(ctx, ws)
	if err != nil || policy.EffectiveRetentionSeconds <= 0 || policy.PrunedBefore == nil {
		return time.Time{}, policy, false
	}
	return *policy.PrunedBefore, policy, true
}
