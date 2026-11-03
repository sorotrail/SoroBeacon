package store

import (
	"context"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

func (s *SQLite) RecordWorkspaceUsageBatch(ctx context.Context, deltas []WorkspaceUsageDelta) error {
	if len(deltas) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return mapSQLiteErr(err)
	}
	defer func() { _ = tx.Rollback() }()

	nowStr := sqliteTimeString(time.Now().UTC())
	for _, raw := range deltas {
		d := normalizeUsageDelta(raw)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO workspaces (id) VALUES (?) ON CONFLICT (id) DO NOTHING`,
			d.WorkspaceID,
		); err != nil {
			return mapSQLiteErr(err)
		}

		startStr := sqliteTimeString(d.PeriodStart)
		endStr := sqliteTimeString(d.PeriodEnd)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO workspace_usage_rollups (
				workspace_id, period_start, period_end, alerts_raised, rule_evaluations, api_requests, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (workspace_id, period_start) DO UPDATE SET
				period_end       = excluded.period_end,
				alerts_raised    = workspace_usage_rollups.alerts_raised + excluded.alerts_raised,
				rule_evaluations = workspace_usage_rollups.rule_evaluations + excluded.rule_evaluations,
				api_requests     = workspace_usage_rollups.api_requests + excluded.api_requests,
				updated_at       = excluded.updated_at`,
			d.WorkspaceID, startStr, endStr, d.AlertsRaised, d.RuleEvaluations, d.APIRequests, nowStr,
		); err != nil {
			return mapSQLiteErr(err)
		}

		for _, n := range d.Notifications {
			if n.Count <= 0 {
				continue
			}
			chType := strings.TrimSpace(n.ChannelType)
			if chType == "" {
				chType = "unknown"
			}
			outcome := strings.TrimSpace(n.Outcome)
			if outcome == "" {
				outcome = "unknown"
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO workspace_notification_usage_rollups (
					workspace_id, period_start, period_end, channel_type, outcome, count, updated_at
				) VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (workspace_id, period_start, channel_type, outcome) DO UPDATE SET
					period_end = excluded.period_end,
					count      = workspace_notification_usage_rollups.count + excluded.count,
					updated_at = excluded.updated_at`,
				d.WorkspaceID, startStr, endStr, chType, outcome, n.Count, nowStr,
			); err != nil {
				return mapSQLiteErr(err)
			}
		}
	}
	return mapSQLiteErr(tx.Commit())
}

func (s *SQLite) GetWorkspaceStorageCounts(ctx context.Context, targetWorkspaceID string) (WorkspaceStorageCounts, error) {
	ws := strings.TrimSpace(targetWorkspaceID)
	if tenantWS, scoped := tenantWorkspace(ctx); scoped {
		ws = string(tenantWS)
	} else if ws == "" {
		ws = string(workspaceID(ctx))
	}
	var c WorkspaceStorageCounts
	err := s.db.QueryRowContext(ctx, `
		SELECT
			(SELECT count(*) FROM monitors WHERE workspace_id = ? AND deleted_at IS NULL),
			(SELECT count(*) FROM rules r JOIN monitors m ON m.id = r.monitor_id WHERE m.workspace_id = ? AND m.deleted_at IS NULL),
			(SELECT count(*) FROM channels WHERE workspace_id = ?),
			(SELECT count(*) FROM alerts WHERE workspace_id = ?),
			(SELECT count(*) FROM delivery_attempts da JOIN alerts a ON a.id = da.alert_id WHERE a.workspace_id = ?),
			(SELECT count(*) FROM dead_letters dl JOIN alerts a ON a.id = dl.alert_id WHERE a.workspace_id = ?)`,
		ws, ws, ws, ws, ws, ws,
	).Scan(&c.Monitors, &c.Rules, &c.Channels, &c.Alerts, &c.DeliveryAttempts, &c.DeadLetters)
	if err != nil {
		return c, mapSQLiteErr(err)
	}
	return c, nil
}

func (s *SQLite) discoverUsageWorkspaces(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM workspaces WHERE id <> ''
		UNION
		SELECT DISTINCT workspace_id FROM monitors WHERE workspace_id <> ''
		UNION
		SELECT DISTINCT workspace_id FROM channels WHERE workspace_id <> ''
		UNION
		SELECT DISTINCT workspace_id FROM alerts WHERE workspace_id <> ''
		UNION
		SELECT DISTINCT workspace_id FROM workspace_usage_rollups WHERE workspace_id <> ''
		UNION
		SELECT DISTINCT workspace_id FROM workspace_notification_usage_rollups WHERE workspace_id <> ''
		ORDER BY 1`)
	if err != nil {
		return nil, mapSQLiteErr(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var ws string
		if err := rows.Scan(&ws); err != nil {
			return nil, err
		}
		out = append(out, ws)
	}
	return out, rows.Err()
}

func (s *SQLite) ListWorkspaceUsage(ctx context.Context, f WorkspaceUsageFilter) ([]WorkspaceUsageSummary, error) {
	workspaces, err := resolveUsageTargetWorkspaces(ctx, f, s.discoverUsageWorkspaces)
	if err != nil {
		return nil, err
	}
	if len(workspaces) == 0 {
		return []WorkspaceUsageSummary{}, nil
	}

	wsPlaceholders := placeholders(len(workspaces))
	baseArgs := make([]any, 0, len(workspaces)+2)
	for _, ws := range workspaces {
		baseArgs = append(baseArgs, ws)
	}

	timeFilter := ""
	if !f.From.IsZero() {
		timeFilter += ` AND period_start >= ?`
		baseArgs = append(baseArgs, sqliteTimeString(f.From.UTC()))
	}
	if !f.To.IsZero() {
		timeFilter += ` AND period_start < ?`
		baseArgs = append(baseArgs, sqliteTimeString(f.To.UTC()))
	}

	pRows, err := s.db.QueryContext(ctx, `
		SELECT workspace_id, period_start, period_end, alerts_raised, rule_evaluations, api_requests, updated_at
		FROM workspace_usage_rollups
		WHERE workspace_id IN (`+wsPlaceholders+`)`+timeFilter+`
		ORDER BY workspace_id ASC, period_start ASC`, baseArgs...)
	if err != nil {
		return nil, mapSQLiteErr(err)
	}
	defer func() { _ = pRows.Close() }()

	periodsByWS := make(map[string][]WorkspaceUsagePeriod, len(workspaces))
	for pRows.Next() {
		var pr WorkspaceUsagePeriod
		var startStr, endStr, updatedStr string
		if err := pRows.Scan(&pr.WorkspaceID, &startStr, &endStr, &pr.AlertsRaised, &pr.RuleEvaluations, &pr.APIRequests, &updatedStr); err != nil {
			return nil, err
		}
		if pr.PeriodStart, err = parseSQLiteTime(startStr); err != nil {
			return nil, err
		}
		if pr.PeriodEnd, err = parseSQLiteTime(endStr); err != nil {
			return nil, err
		}
		if pr.UpdatedAt, err = parseSQLiteTime(updatedStr); err != nil {
			return nil, err
		}
		periodsByWS[pr.WorkspaceID] = append(periodsByWS[pr.WorkspaceID], pr)
	}
	if err := pRows.Err(); err != nil {
		return nil, err
	}

	nRows, err := s.db.QueryContext(ctx, `
		SELECT workspace_id, period_start, channel_type, outcome, count
		FROM workspace_notification_usage_rollups
		WHERE workspace_id IN (`+wsPlaceholders+`)`+timeFilter+`
		ORDER BY workspace_id ASC, period_start ASC, channel_type ASC, outcome ASC`, baseArgs...)
	if err != nil {
		return nil, mapSQLiteErr(err)
	}
	defer func() { _ = nRows.Close() }()

	notifsByWSPeriod := make(map[string]map[time.Time][]WorkspaceNotificationUsage, len(workspaces))
	for nRows.Next() {
		var ws, startStr string
		var n WorkspaceNotificationUsage
		if err := nRows.Scan(&ws, &startStr, &n.ChannelType, &n.Outcome, &n.Count); err != nil {
			return nil, err
		}
		pStart, err := parseSQLiteTime(startStr)
		if err != nil {
			return nil, err
		}
		key := pStart.UTC()
		if notifsByWSPeriod[ws] == nil {
			notifsByWSPeriod[ws] = make(map[time.Time][]WorkspaceNotificationUsage)
		}
		notifsByWSPeriod[ws][key] = append(notifsByWSPeriod[ws][key], n)
	}
	if err := nRows.Err(); err != nil {
		return nil, err
	}

	storageByWS := make(map[string]WorkspaceStorageCounts, len(workspaces))
	for _, ws := range workspaces {
		counts, err := s.GetWorkspaceStorageCounts(workspace.WithSystem(ctx), ws)
		if err != nil {
			return nil, err
		}
		storageByWS[ws] = counts
	}

	return buildWorkspaceUsageSummaries(workspaces, f, periodsByWS, notifsByWSPeriod, storageByWS), nil
}
