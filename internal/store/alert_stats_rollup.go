package store

import (
	"context"
	"database/sql"
	"time"
)

// AlertStatRollup is one bucket of pre-computed alert statistics for a
// (workspace, monitor, rule, severity) tuple.
type AlertStatRollup struct {
	WorkspaceID       string     `json:"workspace_id"`
	MonitorID         int64      `json:"monitor_id"`
	RuleID            int64      `json:"rule_id"`
	Severity          string     `json:"severity"`
	TotalCount        int64      `json:"total_count"`
	ActiveCount       int64      `json:"active_count"`
	AcknowledgedCount int64      `json:"acknowledged_count"`
	SuppressedCount   int64      `json:"suppressed_count"`
	InhibitedCount    int64      `json:"inhibited_count"`
	RetractedCount    int64      `json:"retracted_count"`
	LastAlertAt       *time.Time `json:"last_alert_at,omitempty"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// AlertStatisticsSummary aggregates alert counts by severity, by state, by
// monitor, and by rule for a workspace.
type AlertStatisticsSummary struct {
	Total      int64            `json:"total"`
	ByState    map[string]int64 `json:"by_state"`
	BySeverity map[string]int64 `json:"by_severity"`
	ByMonitor  map[int64]int64  `json:"by_monitor"`
	ByRule     map[int64]int64  `json:"by_rule"`
	Exact      bool             `json:"exact"`
	UpdatedAt  time.Time        `json:"updated_at"`
}

func newAlertStatisticsSummary() AlertStatisticsSummary {
	return AlertStatisticsSummary{
		ByState:    map[string]int64{"active": 0, "acknowledged": 0, "suppressed": 0, "inhibited": 0, "retracted": 0},
		BySeverity: make(map[string]int64),
		ByMonitor:  make(map[int64]int64),
		ByRule:     make(map[int64]int64),
		Exact:      true,
	}
}

func accumulateRollup(sum *AlertStatisticsSummary, r AlertStatRollup) {
	sum.Total += r.TotalCount
	sum.ByState["active"] += r.ActiveCount
	sum.ByState["acknowledged"] += r.AcknowledgedCount
	sum.ByState["suppressed"] += r.SuppressedCount
	sum.ByState["inhibited"] += r.InhibitedCount
	sum.ByState["retracted"] += r.RetractedCount
	sum.BySeverity[r.Severity] += r.TotalCount
	if r.MonitorID > 0 {
		sum.ByMonitor[r.MonitorID] += r.TotalCount
	}
	if r.RuleID > 0 {
		sum.ByRule[r.RuleID] += r.TotalCount
	}
	if r.UpdatedAt.After(sum.UpdatedAt) {
		sum.UpdatedAt = r.UpdatedAt
	}
}

// RecomputeAlertStatistics rebuilds alert_stat_rollups for the caller's
// workspace from the raw alerts table and returns the resulting summary.
func (p *Postgres) RecomputeAlertStatistics(ctx context.Context) (AlertStatisticsSummary, error) {
	ws := string(workspaceID(ctx))
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return AlertStatisticsSummary{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM alert_stat_rollups WHERE workspace_id = $1`, ws); err != nil {
		return AlertStatisticsSummary{}, err
	}

	const insertQ = `
		INSERT INTO alert_stat_rollups (
			workspace_id, monitor_id, rule_id, severity,
			total_count, active_count, acknowledged_count, suppressed_count, inhibited_count, retracted_count,
			last_alert_at, updated_at
		)
		SELECT
			workspace_id,
			COALESCE(monitor_id, 0),
			COALESCE(rule_id, 0),
			COALESCE(NULLIF(severity, ''), 'info'),
			COUNT(*),
			COUNT(*) FILTER (WHERE retracted_at IS NULL AND acknowledged_at IS NULL AND NOT COALESCE(suppressed, FALSE) AND inhibited_by_rule_id IS NULL),
			COUNT(*) FILTER (WHERE retracted_at IS NULL AND acknowledged_at IS NOT NULL),
			COUNT(*) FILTER (WHERE retracted_at IS NULL AND COALESCE(suppressed, FALSE)),
			COUNT(*) FILTER (WHERE retracted_at IS NULL AND inhibited_by_rule_id IS NOT NULL),
			COUNT(*) FILTER (WHERE retracted_at IS NOT NULL),
			MAX(created_at),
			now()
		FROM alerts
		WHERE workspace_id = $1
		GROUP BY workspace_id, COALESCE(monitor_id, 0), COALESCE(rule_id, 0), COALESCE(NULLIF(severity, ''), 'info')`

	if _, err := tx.Exec(ctx, insertQ, ws); err != nil {
		return AlertStatisticsSummary{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AlertStatisticsSummary{}, err
	}
	return p.readAlertStatisticsRollup(ctx)
}

func (p *Postgres) readAlertStatisticsRollup(ctx context.Context) (AlertStatisticsSummary, error) {
	ws := string(workspaceID(ctx))
	rows, err := p.queryRows(ctx, `
		SELECT workspace_id, monitor_id, rule_id, severity,
		       total_count, active_count, acknowledged_count, suppressed_count, inhibited_count, retracted_count,
		       last_alert_at, updated_at
		FROM alert_stat_rollups
		WHERE workspace_id = $1`, ws)
	if err != nil {
		return AlertStatisticsSummary{}, err
	}
	defer rows.Close()

	sum := newAlertStatisticsSummary()
	for rows.Next() {
		var r AlertStatRollup
		if err := rows.Scan(
			&r.WorkspaceID, &r.MonitorID, &r.RuleID, &r.Severity,
			&r.TotalCount, &r.ActiveCount, &r.AcknowledgedCount, &r.SuppressedCount, &r.InhibitedCount, &r.RetractedCount,
			&r.LastAlertAt, &r.UpdatedAt,
		); err != nil {
			return AlertStatisticsSummary{}, err
		}
		accumulateRollup(&sum, r)
	}
	return sum, rows.Err()
}

// GetAlertStatistics reads the pre-computed alert statistics for the caller's
// workspace, falling back to a live recomputation if the rollup cannot be read.
func (p *Postgres) GetAlertStatistics(ctx context.Context) (AlertStatisticsSummary, error) {
	sum, err := p.readAlertStatisticsRollup(ctx)
	if err != nil {
		return p.liveAlertStatistics(ctx)
	}
	return sum, nil
}

func (p *Postgres) liveAlertStatistics(ctx context.Context) (AlertStatisticsSummary, error) {
	ws := string(workspaceID(ctx))
	rows, err := p.queryRows(ctx, `
		SELECT
			COALESCE(monitor_id, 0),
			COALESCE(rule_id, 0),
			COALESCE(NULLIF(severity, ''), 'info'),
			COUNT(*),
			COUNT(*) FILTER (WHERE retracted_at IS NULL AND acknowledged_at IS NULL AND NOT COALESCE(suppressed, FALSE) AND inhibited_by_rule_id IS NULL),
			COUNT(*) FILTER (WHERE retracted_at IS NULL AND acknowledged_at IS NOT NULL),
			COUNT(*) FILTER (WHERE retracted_at IS NULL AND COALESCE(suppressed, FALSE)),
			COUNT(*) FILTER (WHERE retracted_at IS NULL AND inhibited_by_rule_id IS NOT NULL),
			COUNT(*) FILTER (WHERE retracted_at IS NOT NULL),
			MAX(created_at)
		FROM alerts
		WHERE workspace_id = $1
		GROUP BY COALESCE(monitor_id, 0), COALESCE(rule_id, 0), COALESCE(NULLIF(severity, ''), 'info')`, ws)
	if err != nil {
		return AlertStatisticsSummary{}, err
	}
	defer rows.Close()

	sum := newAlertStatisticsSummary()
	sum.UpdatedAt = time.Now().UTC()
	for rows.Next() {
		var r AlertStatRollup
		r.WorkspaceID = ws
		if err := rows.Scan(
			&r.MonitorID, &r.RuleID, &r.Severity,
			&r.TotalCount, &r.ActiveCount, &r.AcknowledgedCount, &r.SuppressedCount, &r.InhibitedCount, &r.RetractedCount,
			&r.LastAlertAt,
		); err != nil {
			return AlertStatisticsSummary{}, err
		}
		accumulateRollup(&sum, r)
	}
	return sum, rows.Err()
}

// VerifyAlertStatistics compares the maintained rollup against a full live
// recomputation over alerts for the caller's workspace. When any bucket has
// drifted, it repairs the rollup via RecomputeAlertStatistics and returns
// matched == false.
func (p *Postgres) VerifyAlertStatistics(ctx context.Context) (matched bool, err error) {
	maintained, err := p.readAlertStatisticsRollup(ctx)
	if err != nil {
		return false, err
	}
	live, err := p.liveAlertStatistics(ctx)
	if err != nil {
		return false, err
	}
	if summariesEqual(maintained, live) {
		return true, nil
	}
	_, err = p.RecomputeAlertStatistics(ctx)
	return false, err
}

// RecomputeAlertStatistics rebuilds alert_stat_rollups for the caller's
// workspace in the SQLite store.
func (s *SQLite) RecomputeAlertStatistics(ctx context.Context) (AlertStatisticsSummary, error) {
	ws := string(workspaceID(ctx))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AlertStatisticsSummary{}, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM alert_stat_rollups WHERE workspace_id = ?`, ws); err != nil {
		return AlertStatisticsSummary{}, err
	}

	const insertQ = `
		INSERT INTO alert_stat_rollups (
			workspace_id, monitor_id, rule_id, severity,
			total_count, active_count, acknowledged_count, suppressed_count, inhibited_count, retracted_count,
			last_alert_at, updated_at
		)
		SELECT
			workspace_id,
			COALESCE(monitor_id, 0),
			COALESCE(rule_id, 0),
			CASE WHEN severity IS NULL OR severity = '' THEN 'info' ELSE severity END,
			COUNT(*),
			SUM(CASE WHEN retracted_at IS NULL AND acknowledged_at IS NULL AND COALESCE(suppressed, 0) = 0 AND inhibited_by_rule_id IS NULL THEN 1 ELSE 0 END),
			SUM(CASE WHEN retracted_at IS NULL AND acknowledged_at IS NOT NULL THEN 1 ELSE 0 END),
			SUM(CASE WHEN retracted_at IS NULL AND COALESCE(suppressed, 0) != 0 THEN 1 ELSE 0 END),
			SUM(CASE WHEN retracted_at IS NULL AND inhibited_by_rule_id IS NOT NULL THEN 1 ELSE 0 END),
			SUM(CASE WHEN retracted_at IS NOT NULL THEN 1 ELSE 0 END),
			MAX(created_at),
			strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		FROM alerts
		WHERE workspace_id = ?
		GROUP BY workspace_id, COALESCE(monitor_id, 0), COALESCE(rule_id, 0), CASE WHEN severity IS NULL OR severity = '' THEN 'info' ELSE severity END`

	if _, err := tx.ExecContext(ctx, insertQ, ws); err != nil {
		return AlertStatisticsSummary{}, err
	}
	if err := tx.Commit(); err != nil {
		return AlertStatisticsSummary{}, err
	}
	return s.readAlertStatisticsRollup(ctx)
}

func (s *SQLite) readAlertStatisticsRollup(ctx context.Context) (AlertStatisticsSummary, error) {
	ws := string(workspaceID(ctx))
	rows, err := s.db.QueryContext(ctx, `
		SELECT workspace_id, monitor_id, rule_id, severity,
		       total_count, active_count, acknowledged_count, suppressed_count, inhibited_count, retracted_count,
		       last_alert_at, updated_at
		FROM alert_stat_rollups
		WHERE workspace_id = ?`, ws)
	if err != nil {
		return AlertStatisticsSummary{}, err
	}
	defer func() { _ = rows.Close() }()

	sum := newAlertStatisticsSummary()
	for rows.Next() {
		var r AlertStatRollup
		var lastAlert sql.NullString
		var updated string
		if err := rows.Scan(
			&r.WorkspaceID, &r.MonitorID, &r.RuleID, &r.Severity,
			&r.TotalCount, &r.ActiveCount, &r.AcknowledgedCount, &r.SuppressedCount, &r.InhibitedCount, &r.RetractedCount,
			&lastAlert, &updated,
		); err != nil {
			return AlertStatisticsSummary{}, err
		}
		r.LastAlertAt, _ = parseSQLiteTimePtr(lastAlert)
		r.UpdatedAt, _ = parseSQLiteTime(updated)
		accumulateRollup(&sum, r)
	}
	return sum, rows.Err()
}

// GetAlertStatistics reads the pre-computed alert statistics in SQLite, falling
// back to live recomputation if the rollup is unavailable.
func (s *SQLite) GetAlertStatistics(ctx context.Context) (AlertStatisticsSummary, error) {
	sum, err := s.readAlertStatisticsRollup(ctx)
	if err != nil {
		return s.liveAlertStatistics(ctx)
	}
	return sum, nil
}

func (s *SQLite) liveAlertStatistics(ctx context.Context) (AlertStatisticsSummary, error) {
	ws := string(workspaceID(ctx))
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			COALESCE(monitor_id, 0),
			COALESCE(rule_id, 0),
			CASE WHEN severity IS NULL OR severity = '' THEN 'info' ELSE severity END,
			COUNT(*),
			COALESCE(SUM(CASE WHEN retracted_at IS NULL AND acknowledged_at IS NULL AND COALESCE(suppressed, 0) = 0 AND inhibited_by_rule_id IS NULL THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN retracted_at IS NULL AND acknowledged_at IS NOT NULL THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN retracted_at IS NULL AND COALESCE(suppressed, 0) != 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN retracted_at IS NULL AND inhibited_by_rule_id IS NOT NULL THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN retracted_at IS NOT NULL THEN 1 ELSE 0 END), 0),
			MAX(created_at)
		FROM alerts
		WHERE workspace_id = ?
		GROUP BY COALESCE(monitor_id, 0), COALESCE(rule_id, 0), CASE WHEN severity IS NULL OR severity = '' THEN 'info' ELSE severity END`, ws)
	if err != nil {
		return AlertStatisticsSummary{}, err
	}
	defer func() { _ = rows.Close() }()

	sum := newAlertStatisticsSummary()
	sum.UpdatedAt = time.Now().UTC()
	for rows.Next() {
		var r AlertStatRollup
		var lastAlert sql.NullString
		r.WorkspaceID = ws
		if err := rows.Scan(
			&r.MonitorID, &r.RuleID, &r.Severity,
			&r.TotalCount, &r.ActiveCount, &r.AcknowledgedCount, &r.SuppressedCount, &r.InhibitedCount, &r.RetractedCount,
			&lastAlert,
		); err != nil {
			return AlertStatisticsSummary{}, err
		}
		r.LastAlertAt, _ = parseSQLiteTimePtr(lastAlert)
		accumulateRollup(&sum, r)
	}
	return sum, rows.Err()
}

// VerifyAlertStatistics compares the maintained SQLite rollup against a live
// recomputation and repairs any drift.
func (s *SQLite) VerifyAlertStatistics(ctx context.Context) (matched bool, err error) {
	maintained, err := s.readAlertStatisticsRollup(ctx)
	if err != nil {
		return false, err
	}
	live, err := s.liveAlertStatistics(ctx)
	if err != nil {
		return false, err
	}
	if summariesEqual(maintained, live) {
		return true, nil
	}
	_, err = s.RecomputeAlertStatistics(ctx)
	return false, err
}

func summariesEqual(a, b AlertStatisticsSummary) bool {
	if a.Total != b.Total {
		return false
	}
	for k, v := range a.ByState {
		if b.ByState[k] != v {
			return false
		}
	}
	if len(a.BySeverity) != len(b.BySeverity) || len(a.ByMonitor) != len(b.ByMonitor) || len(a.ByRule) != len(b.ByRule) {
		return false
	}
	for k, v := range a.BySeverity {
		if b.BySeverity[k] != v {
			return false
		}
	}
	for k, v := range a.ByMonitor {
		if b.ByMonitor[k] != v {
			return false
		}
	}
	for k, v := range a.ByRule {
		if b.ByRule[k] != v {
			return false
		}
	}
	return true
}

