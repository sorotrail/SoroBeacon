package store

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// DefaultUsagePeriodDuration is the bucket width for durable workspace usage rollups.
const DefaultUsagePeriodDuration = time.Hour

// TruncateUsagePeriod returns the UTC hour boundaries [start, end) containing t.
func TruncateUsagePeriod(t time.Time) (start, end time.Time) {
	if t.IsZero() {
		t = time.Now()
	}
	start = t.UTC().Truncate(DefaultUsagePeriodDuration)
	return start, start.Add(DefaultUsagePeriodDuration)
}

// SafeRecordWorkspaceUsageBatch calls st.RecordWorkspaceUsageBatch while
// tolerating partial test fakes that embed a nil store.Store without stubbing Usage.
func SafeRecordWorkspaceUsageBatch(ctx context.Context, st Usage, deltas []WorkspaceUsageDelta) (err error) {
	if st == nil || len(deltas) == 0 {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			err = nil
		}
	}()
	return st.RecordWorkspaceUsageBatch(ctx, deltas)
}

// SafeGetWorkspaceStorageCounts calls st.GetWorkspaceStorageCounts while
// tolerating partial test fakes that embed a nil store.Store.
func SafeGetWorkspaceStorageCounts(ctx context.Context, st Usage, workspaceID string) (counts WorkspaceStorageCounts, err error) {
	if st == nil {
		return WorkspaceStorageCounts{}, nil
	}
	defer func() {
		if r := recover(); r != nil {
			counts = WorkspaceStorageCounts{}
			err = nil
		}
	}()
	return st.GetWorkspaceStorageCounts(ctx, workspaceID)
}

// SafeListWorkspaceUsage calls st.ListWorkspaceUsage, recovering as an empty
// default summary if st is a test fake that embeds Store as nil without
// implementing Usage.
func SafeListWorkspaceUsage(ctx context.Context, st Usage, f WorkspaceUsageFilter) (summaries []WorkspaceUsageSummary, err error) {
	if st == nil {
		return nil, nil
	}
	defer func() {
		if r := recover(); r != nil {
			ws := f.WorkspaceID
			if ws == "" {
				if cid, ok := workspace.From(ctx); ok && cid != "" {
					ws = string(cid)
				} else {
					ws = string(workspace.Default)
				}
			}
			summaries = []WorkspaceUsageSummary{{
				WorkspaceID:   ws,
				Notifications: []WorkspaceNotificationUsage{},
				Periods:       []WorkspaceUsagePeriod{},
			}}
			err = nil
		}
	}()
	return st.ListWorkspaceUsage(ctx, f)
}

func normalizeUsageDelta(d WorkspaceUsageDelta) WorkspaceUsageDelta {
	ws := strings.TrimSpace(d.WorkspaceID)
	if ws == "" {
		ws = string(workspace.Default)
	}
	start, defaultEnd := TruncateUsagePeriod(d.PeriodStart)
	end := d.PeriodEnd.UTC()
	if end.IsZero() || !end.After(start) {
		end = defaultEnd
	}
	d.WorkspaceID = ws
	d.PeriodStart = start
	d.PeriodEnd = end
	return d
}

// resolveUsageTargetWorkspaces determines which workspaces ListWorkspaceUsage
// should return for ctx and filter f.
func resolveUsageTargetWorkspaces(ctx context.Context, f WorkspaceUsageFilter, discover func(context.Context) ([]string, error)) ([]string, error) {
	if ws, scoped := tenantWorkspace(ctx); scoped {
		target := string(ws)
		if f.WorkspaceID != "" && f.WorkspaceID != target {
			return []string{}, nil
		}
		return []string{target}, nil
	}
	if ws := strings.TrimSpace(f.WorkspaceID); ws != "" {
		return []string{ws}, nil
	}
	list, err := discover(ctx)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return []string{string(workspace.Default)}, nil
	}
	return list, nil
}

type notifKey struct {
	channelType string
	outcome     string
}

func buildWorkspaceUsageSummaries(
	workspaces []string,
	f WorkspaceUsageFilter,
	periodsByWS map[string][]WorkspaceUsagePeriod,
	notifsByWSPeriod map[string]map[time.Time][]WorkspaceNotificationUsage,
	storageByWS map[string]WorkspaceStorageCounts,
) []WorkspaceUsageSummary {
	var fromPtr, toPtr *time.Time
	if !f.From.IsZero() {
		u := f.From.UTC()
		fromPtr = &u
	}
	if !f.To.IsZero() {
		u := f.To.UTC()
		toPtr = &u
	}

	out := make([]WorkspaceUsageSummary, 0, len(workspaces))
	for _, ws := range workspaces {
		periods := periodsByWS[ws]
		if periods == nil {
			periods = []WorkspaceUsagePeriod{}
		}
		notifTotals := make(map[notifKey]int64)
		var alertsTotal, ruleEvalsTotal, apiReqsTotal int64

		for i := range periods {
			p := &periods[i]
			pNotifs := notifsByWSPeriod[ws][p.PeriodStart.UTC()]
			if pNotifs == nil {
				pNotifs = []WorkspaceNotificationUsage{}
			}
			sort.Slice(pNotifs, func(a, b int) bool {
				if pNotifs[a].ChannelType != pNotifs[b].ChannelType {
					return pNotifs[a].ChannelType < pNotifs[b].ChannelType
				}
				return pNotifs[a].Outcome < pNotifs[b].Outcome
			})
			p.Notifications = pNotifs
			alertsTotal += p.AlertsRaised
			ruleEvalsTotal += p.RuleEvaluations
			apiReqsTotal += p.APIRequests
			for _, n := range pNotifs {
				notifTotals[notifKey{channelType: n.ChannelType, outcome: n.Outcome}] += n.Count
			}
		}

		aggNotifs := make([]WorkspaceNotificationUsage, 0, len(notifTotals))
		for k, count := range notifTotals {
			aggNotifs = append(aggNotifs, WorkspaceNotificationUsage{
				ChannelType: k.channelType,
				Outcome:     k.outcome,
				Count:       count,
			})
		}
		sort.Slice(aggNotifs, func(a, b int) bool {
			if aggNotifs[a].ChannelType != aggNotifs[b].ChannelType {
				return aggNotifs[a].ChannelType < aggNotifs[b].ChannelType
			}
			return aggNotifs[a].Outcome < aggNotifs[b].Outcome
		})

		out = append(out, WorkspaceUsageSummary{
			WorkspaceID:     ws,
			From:            fromPtr,
			To:              toPtr,
			AlertsRaised:    alertsTotal,
			RuleEvaluations: ruleEvalsTotal,
			APIRequests:     apiReqsTotal,
			Notifications:   aggNotifs,
			StoredRows:      storageByWS[ws],
			Periods:         periods,
		})
	}
	return out
}

func (p *Postgres) RecordWorkspaceUsageBatch(ctx context.Context, deltas []WorkspaceUsageDelta) error {
	if len(deltas) == 0 {
		return nil
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	for _, raw := range deltas {
		d := normalizeUsageDelta(raw)
		if err := p.EnsureWorkspace(ctx, workspace.ID(d.WorkspaceID)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO workspace_usage_rollups (
				workspace_id, period_start, period_end, alerts_raised, rule_evaluations, api_requests, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, now())
			ON CONFLICT (workspace_id, period_start) DO UPDATE SET
				period_end       = EXCLUDED.period_end,
				alerts_raised    = workspace_usage_rollups.alerts_raised + EXCLUDED.alerts_raised,
				rule_evaluations = workspace_usage_rollups.rule_evaluations + EXCLUDED.rule_evaluations,
				api_requests     = workspace_usage_rollups.api_requests + EXCLUDED.api_requests,
				updated_at       = now()`,
			d.WorkspaceID, d.PeriodStart, d.PeriodEnd, d.AlertsRaised, d.RuleEvaluations, d.APIRequests,
		); err != nil {
			return mapErr(err)
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
			if _, err := tx.Exec(ctx, `
				INSERT INTO workspace_notification_usage_rollups (
					workspace_id, period_start, period_end, channel_type, outcome, count, updated_at
				) VALUES ($1, $2, $3, $4, $5, $6, now())
				ON CONFLICT (workspace_id, period_start, channel_type, outcome) DO UPDATE SET
					period_end = EXCLUDED.period_end,
					count      = workspace_notification_usage_rollups.count + EXCLUDED.count,
					updated_at = now()`,
				d.WorkspaceID, d.PeriodStart, d.PeriodEnd, chType, outcome, n.Count,
			); err != nil {
				return mapErr(err)
			}
		}
	}
	return tx.Commit(ctx)
}

func (p *Postgres) GetWorkspaceStorageCounts(ctx context.Context, targetWorkspaceID string) (WorkspaceStorageCounts, error) {
	ws := strings.TrimSpace(targetWorkspaceID)
	if tenantWS, scoped := tenantWorkspace(ctx); scoped {
		ws = string(tenantWS)
	} else if ws == "" {
		ws = string(workspaceID(ctx))
	}
	var c WorkspaceStorageCounts
	err := p.pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM monitors WHERE workspace_id = $1 AND deleted_at IS NULL),
			(SELECT count(*) FROM rules r JOIN monitors m ON m.id = r.monitor_id WHERE m.workspace_id = $1 AND m.deleted_at IS NULL),
			(SELECT count(*) FROM channels WHERE workspace_id = $1),
			(SELECT count(*) FROM alerts WHERE workspace_id = $1),
			(SELECT count(*) FROM delivery_attempts da JOIN alerts a ON a.id = da.alert_id WHERE a.workspace_id = $1),
			(SELECT count(*) FROM dead_letters dl JOIN alerts a ON a.id = dl.alert_id WHERE a.workspace_id = $1)`,
		ws,
	).Scan(&c.Monitors, &c.Rules, &c.Channels, &c.Alerts, &c.DeliveryAttempts, &c.DeadLetters)
	if err != nil {
		return c, mapErr(err)
	}
	return c, nil
}

func (p *Postgres) discoverUsageWorkspaces(ctx context.Context) ([]string, error) {
	rows, err := p.pool.Query(ctx, `
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
		return nil, mapErr(err)
	}
	defer rows.Close()
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

func (p *Postgres) ListWorkspaceUsage(ctx context.Context, f WorkspaceUsageFilter) ([]WorkspaceUsageSummary, error) {
	workspaces, err := resolveUsageTargetWorkspaces(ctx, f, p.discoverUsageWorkspaces)
	if err != nil {
		return nil, err
	}
	if len(workspaces) == 0 {
		return []WorkspaceUsageSummary{}, nil
	}

	var (
		fromArg any
		toArg   any
	)
	if !f.From.IsZero() {
		fromArg = f.From.UTC()
	}
	if !f.To.IsZero() {
		toArg = f.To.UTC()
	}

	pRows, err := p.pool.Query(ctx, `
		SELECT workspace_id, period_start, period_end, alerts_raised, rule_evaluations, api_requests, updated_at
		FROM workspace_usage_rollups
		WHERE workspace_id = ANY($1)
		  AND ($2::timestamptz IS NULL OR period_start >= $2)
		  AND ($3::timestamptz IS NULL OR period_start < $3)
		ORDER BY workspace_id ASC, period_start ASC`,
		workspaces, fromArg, toArg)
	if err != nil {
		return nil, mapErr(err)
	}
	defer pRows.Close()

	periodsByWS := make(map[string][]WorkspaceUsagePeriod, len(workspaces))
	for pRows.Next() {
		var pr WorkspaceUsagePeriod
		if err := pRows.Scan(&pr.WorkspaceID, &pr.PeriodStart, &pr.PeriodEnd, &pr.AlertsRaised, &pr.RuleEvaluations, &pr.APIRequests, &pr.UpdatedAt); err != nil {
			return nil, err
		}
		pr.PeriodStart = pr.PeriodStart.UTC()
		pr.PeriodEnd = pr.PeriodEnd.UTC()
		pr.UpdatedAt = pr.UpdatedAt.UTC()
		periodsByWS[pr.WorkspaceID] = append(periodsByWS[pr.WorkspaceID], pr)
	}
	if err := pRows.Err(); err != nil {
		return nil, err
	}

	nRows, err := p.pool.Query(ctx, `
		SELECT workspace_id, period_start, channel_type, outcome, count
		FROM workspace_notification_usage_rollups
		WHERE workspace_id = ANY($1)
		  AND ($2::timestamptz IS NULL OR period_start >= $2)
		  AND ($3::timestamptz IS NULL OR period_start < $3)
		ORDER BY workspace_id ASC, period_start ASC, channel_type ASC, outcome ASC`,
		workspaces, fromArg, toArg)
	if err != nil {
		return nil, mapErr(err)
	}
	defer nRows.Close()

	notifsByWSPeriod := make(map[string]map[time.Time][]WorkspaceNotificationUsage, len(workspaces))
	for nRows.Next() {
		var ws string
		var pStart time.Time
		var n WorkspaceNotificationUsage
		if err := nRows.Scan(&ws, &pStart, &n.ChannelType, &n.Outcome, &n.Count); err != nil {
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
		counts, err := p.GetWorkspaceStorageCounts(workspace.WithSystem(ctx), ws)
		if err != nil {
			return nil, err
		}
		storageByWS[ws] = counts
	}

	return buildWorkspaceUsageSummaries(workspaces, f, periodsByWS, notifsByWSPeriod, storageByWS), nil
}
