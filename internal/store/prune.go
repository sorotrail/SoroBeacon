package store

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// DefaultPruneBatch is the number of expired alerts deleted per SQL
// statement. Bounded so a backlog never holds a long lock or blocks
// ingestion.
const DefaultPruneBatch = 1000

// DefaultPruneInterval is how often the pruner looks for expired alerts
// when ALERT_RETENTION is set.
const DefaultPruneInterval = time.Hour

// DeleteExpiredAlerts deletes up to limit alerts whose created_at is
// strictly before cutoff. delivery_attempts rows follow via ON DELETE
// CASCADE. limit is clamped to DefaultPruneBatch when it is not
// positive.
func (p *Postgres) DeleteExpiredAlerts(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if limit <= 0 {
		limit = DefaultPruneBatch
	}
	// One statement so the dedup keys of deleted alerts cannot outlive them:
	// a stranded dedup row would stop that event from ever alerting again.
	// The composite FK cascades delivery_attempts for the deleted rows.
	var deleted int64
	err := p.pool.QueryRow(ctx, `
		WITH expired AS (
			SELECT id, created_at FROM alerts
			WHERE created_at < $1
			ORDER BY created_at ASC, id ASC
			LIMIT $2
		),
		cleared_dedup AS (
			DELETE FROM alert_dedup d
			USING expired e
			WHERE d.alert_id = e.id
			RETURNING d.rule_id
		),
		deleted_alerts AS (
			DELETE FROM alerts a
			USING expired e
			WHERE a.id = e.id AND a.created_at = e.created_at
			RETURNING a.id
		)
		SELECT count(*) FROM deleted_alerts`, cutoff, limit).Scan(&deleted)
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// AlertPruner is the persistence slice the retention loop needs.
type AlertPruner interface {
	DeleteExpiredAlerts(ctx context.Context, cutoff time.Time, limit int) (int64, error)
}

// AlertArchiveSource is the extra slice the retention loop needs when an
// archiver is configured: read the batch before it is deleted. Both store
// backends implement it alongside DeleteExpiredAlerts.
type AlertArchiveSource interface {
	AlertPruner
	ExpiredAlerts(ctx context.Context, cutoff time.Time, limit int) ([]Alert, error)
}

// AlertArchiver persists one batch of expired alerts before retention deletes
// them. It is defined here, not in internal/archive, so the store does not
// depend on any archive backend; internal/archive implements this interface.
//
// Archive must be idempotent — re-running with the batch that is still
// un-deleted must overwrite rather than duplicate — and a non-nil error
// blocks the delete, which is the whole ordering guarantee.
type AlertArchiver interface {
	Archive(ctx context.Context, alerts []Alert) error
}

func storeMetrics(st any) *metrics.Metrics {
	switch s := st.(type) {
	case *Postgres:
		return s.metrics
	case *SQLite:
		return s.metrics
	default:
		return nil
	}
}

// PruneWorkspaceAlertsOnce executes one per-workspace alert retention pass
// across all workspaces on a store implementing RetentionPolicies.
//
// Whole monthly partitions are dropped only when every workspace's effective
// retention horizon has passed (via EffectiveRetentionCutoff). Expired rows
// belonging to workspaces with shorter retention policies are deleted in
// bounded batches per workspace, emitting structured slog records and
// Prometheus metrics that report which workspace policy applied.
func PruneWorkspaceAlertsOnce(ctx context.Context, st AlertPruner, retention time.Duration, batch int, arch AlertArchiver, log *slog.Logger) (int64, error) {
	if batch <= 0 {
		batch = DefaultPruneBatch
	}
	if log == nil {
		log = slog.Default()
	}
	rp, ok := st.(RetentionPolicies)
	if !ok {
		return 0, errors.New("store does not implement RetentionPolicies")
	}

	now := time.Now().UTC()
	policies, err := rp.ListWorkspaceRetentionPolicies(ctx)
	if err != nil {
		log.Error("list workspace retention policies failed", "error", err)
		return 0, err
	}

	wsRetentions := make(map[string]time.Duration, len(policies))
	for _, pol := range policies {
		effDur := pol.EffectiveDuration()
		if !pol.Configured && retention > 0 {
			effDur = retention
		}
		wsRetentions[pol.WorkspaceID] = effDur
	}

	var deleted int64
	if mgr, ok := st.(PartitionLifecycleManager); ok {
		plan, err := mgr.MaintainAlertPartitions(ctx, PartitionLifecycleOptions{
			Now:                 now,
			Retention:           retention,
			WorkspaceRetentions: wsRetentions,
			Archiver:            arch,
			Log:                 log,
		})
		if err != nil {
			log.Error("alert partition maintenance failed", "error", err)
			return 0, err
		}
		if plan != nil {
			deleted += plan.AlertsDropped
		}
	} else {
		if pe, ok := st.(PartitionEnsurer); ok {
			if err := pe.EnsureAlertPartitions(ctx, now, DefaultPartitionRunwayMonths); err != nil {
				log.Error("ensure alert partitions failed", "error", err)
				return 0, err
			}
		}
		if arch == nil {
			if effCutoff, canDrop := EffectiveRetentionCutoff(now, retention, wsRetentions); canDrop {
				if pp, ok := st.(PartitionPruner); ok {
					n, err := pp.DropExpiredPartitions(ctx, effCutoff)
					if err != nil {
						log.Error("drop expired alert partitions failed", "error", err, "cutoff", effCutoff)
						return 0, err
					}
					deleted += n
				}
			}
		}
	}

	m := storeMetrics(st)
	for _, pol := range policies {
		effDur := pol.EffectiveDuration()
		policySource := pol.PolicySource
		if !pol.Configured && retention > 0 {
			effDur = retention
			policySource = RetentionPolicySourceInstance
		}
		m.SetWorkspaceRetentionSeconds(pol.WorkspaceID, policySource, effDur)
		if effDur <= 0 {
			continue
		}
		wsCutoff := now.Add(-effDur)
		var wsDeleted int64
		for {
			if arch != nil {
				expired, err := rp.ExpiredAlertsForWorkspace(ctx, workspace.ID(pol.WorkspaceID), wsCutoff, batch)
				if err != nil {
					log.Error("alert retention read failed",
						"error", err,
						"workspace_id", pol.WorkspaceID,
						"policy_source", policySource,
						"cutoff", wsCutoff,
					)
					return deleted, err
				}
				if len(expired) == 0 {
					break
				}
				if err := arch.Archive(ctx, expired); err != nil {
					log.Error("alert archive failed; retention delete skipped",
						"error", err,
						"workspace_id", pol.WorkspaceID,
						"policy_source", policySource,
						"cutoff", wsCutoff,
						"batch", len(expired),
					)
					return deleted, err
				}
			}
			n, err := rp.DeleteExpiredAlertsForWorkspace(ctx, workspace.ID(pol.WorkspaceID), wsCutoff, batch)
			if err != nil {
				log.Error("alert retention prune failed",
					"error", err,
					"workspace_id", pol.WorkspaceID,
					"policy_source", policySource,
					"cutoff", wsCutoff,
				)
				return deleted, err
			}
			wsDeleted += n
			deleted += n
			if n < int64(batch) {
				break
			}
		}
		m.RecordWorkspaceAlertsPruned(pol.WorkspaceID, policySource, wsDeleted)
		log.Info("workspace alert retention prune",
			"workspace_id", pol.WorkspaceID,
			"policy_source", policySource,
			"retention", effDur.String(),
			"deleted", wsDeleted,
			"cutoff", wsCutoff,
			"archiving", arch != nil,
		)
	}

	var summaryCutoff time.Time
	if retention > 0 {
		summaryCutoff = now.Add(-retention)
	}
	log.Info("alert retention prune", "deleted", deleted, "cutoff", summaryCutoff, "archiving", arch != nil)
	return deleted, nil
}

// RunAlertPruner deletes alerts older than retention in batches until
// ctx is cancelled. Unset/zero retention is a no-op unless at least one
// workspace has configured a per-workspace retention policy. One pass runs
// immediately, then on interval (DefaultPruneInterval when interval is not
// positive).
//
// arch, when non-nil, is asked to persist each batch first; the delete for
// that batch is skipped if the archive fails, so archiving turns retention
// from destruction into tiering. A nil arch is byte-for-byte the previous
// behaviour.
func RunAlertPruner(ctx context.Context, st AlertPruner, retention, interval time.Duration, batch int, arch AlertArchiver, log *slog.Logger) {
	_, hasWorkspacePolicies := st.(RetentionPolicies)
	if retention <= 0 && !hasWorkspacePolicies {
		return
	}
	if batch <= 0 {
		batch = DefaultPruneBatch
	}
	if interval <= 0 {
		interval = DefaultPruneInterval
	}
	if log == nil {
		log = slog.Default()
	}
	log.Info("alert retention pruner started",
		"retention", retention.String(),
		"interval", interval.String(),
		"batch", batch,
		"archiving", arch != nil,
	)

	pruneOnce := func() {
		if hasWorkspacePolicies {
			_, _ = PruneWorkspaceAlertsOnce(ctx, st, retention, batch, arch, log)
			return
		}
		now := time.Now().UTC()
		cutoff := now.Add(-retention)
		var deleted int64
		// Keep the upcoming partition window open before anything is written
		// into it, and drop whole expired partitions when archiving is off.
		if mgr, ok := st.(PartitionLifecycleManager); ok {
			plan, err := mgr.MaintainAlertPartitions(ctx, PartitionLifecycleOptions{
				Now:       now,
				Retention: retention,
				Archiver:  arch,
				Log:       log,
			})
			if err != nil {
				log.Error("alert partition maintenance failed", "error", err, "cutoff", cutoff)
				return
			}
			if plan != nil {
				deleted += plan.AlertsDropped
			}
		} else {
			if pe, ok := st.(PartitionEnsurer); ok {
				if err := pe.EnsureAlertPartitions(ctx, now, DefaultPartitionRunwayMonths); err != nil {
					log.Error("ensure alert partitions failed", "error", err)
					return
				}
			}
			if arch == nil {
				if pp, ok := st.(PartitionPruner); ok {
					n, err := pp.DropExpiredPartitions(ctx, cutoff)
					if err != nil {
						log.Error("drop expired alert partitions failed", "error", err, "cutoff", cutoff)
						return
					}
					deleted += n
				}
			}
		}
		for {
			if arch != nil {
				src, ok := st.(AlertArchiveSource)
				if !ok {
					log.Error("alert archiver configured but the store cannot read expired alerts",
						"error", errors.New("store does not implement ExpiredAlerts"))
					return
				}
				expired, err := src.ExpiredAlerts(ctx, cutoff, batch)
				if err != nil {
					log.Error("alert retention read failed", "error", err, "cutoff", cutoff)
					return
				}
				if len(expired) == 0 {
					break
				}
				if err := arch.Archive(ctx, expired); err != nil {
					// The ordering is the guarantee: nothing may be deleted
					// until its archive write has succeeded.
					log.Error("alert archive failed; retention delete skipped",
						"error", err, "cutoff", cutoff, "batch", len(expired))
					return
				}
			}
			n, err := st.DeleteExpiredAlerts(ctx, cutoff, batch)
			if err != nil {
				log.Error("alert retention prune failed", "error", err, "cutoff", cutoff)
				return
			}
			deleted += n
			if n < int64(batch) {
				break
			}
		}
		log.Info("alert retention prune", "deleted", deleted, "cutoff", cutoff, "archiving", arch != nil)
	}

	pruneOnce()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("alert retention pruner stopped")
			return
		case <-t.C:
			pruneOnce()
		}
	}
}

// DeleteExpiredEvents removes up to limit archived events ingested strictly
// before cutoff. Unlike alerts there is nothing to archive first or cascade
// after: the rows being deleted *are* the event archive, read only by
// replay. limit is clamped to DefaultPruneBatch when it is not positive.
func (p *Postgres) DeleteExpiredEvents(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if limit <= 0 {
		limit = DefaultPruneBatch
	}
	var deleted int64
	err := p.pool.QueryRow(ctx, `
		WITH expired AS (
			SELECT id FROM event_archive
			WHERE ingested_at < $1
			ORDER BY ingested_at ASC, id ASC
			LIMIT $2
		),
		deleted_events AS (
			DELETE FROM event_archive e
			USING expired x
			WHERE e.id = x.id
			RETURNING e.id
		)
		SELECT count(*) FROM deleted_events`, cutoff, limit).Scan(&deleted)
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// DeleteExpiredEvents is the SQLite counterpart; see the Postgres method.
func (s *SQLite) DeleteExpiredEvents(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if limit <= 0 {
		limit = DefaultPruneBatch
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM event_archive WHERE id IN (
			SELECT id FROM event_archive WHERE ingested_at < ?
			ORDER BY ingested_at ASC, id ASC LIMIT ?
		)`, sqliteTimeString(cutoff.UTC()), limit)
	if err != nil {
		return 0, mapSQLiteErr(err)
	}
	return res.RowsAffected()
}

// RunEventArchivePruner deletes archived events older than retention in
// batches until ctx is cancelled. Unlike alert retention there is no
// archive-first phase — the rows being deleted are the archive — so a
// non-positive retention is the only off switch, and it is what keeps the
// replay corpus bounded rather than a second copy of the chain. One pass
// runs immediately, then on interval (DefaultPruneInterval when interval
// is not positive).
func RunEventArchivePruner(ctx context.Context, st EventArchive, retention, interval time.Duration, batch int, log *slog.Logger) {
	if retention <= 0 {
		return
	}
	if batch <= 0 {
		batch = DefaultPruneBatch
	}
	if interval <= 0 {
		interval = DefaultPruneInterval
	}
	if log == nil {
		log = slog.Default()
	}
	log.Info("event archive pruner started",
		"retention", retention.String(),
		"interval", interval.String(),
		"batch", batch,
	)

	pruneOnce := func() {
		cutoff := time.Now().UTC().Add(-retention)
		var deleted int64
		for {
			n, err := st.DeleteExpiredEvents(ctx, cutoff, batch)
			if err != nil {
				log.Error("event archive prune failed", "error", err, "cutoff", cutoff)
				return
			}
			deleted += n
			if n < int64(batch) {
				break
			}
		}
		log.Info("event archive prune", "deleted", deleted, "cutoff", cutoff)
	}

	pruneOnce()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("event archive pruner stopped")
			return
		case <-t.C:
			pruneOnce()
		}
	}
}

// RunDeadLetterPruner removes dead letters older than retention in batches until
// ctx is cancelled. Non-positive retention is a no-op.
func RunDeadLetterPruner(ctx context.Context, st DeadLetters, retention, interval time.Duration, batch int, log *slog.Logger) {
	if retention <= 0 {
		return
	}
	if batch <= 0 {
		batch = DefaultPruneBatch
	}
	if interval <= 0 {
		interval = DefaultPruneInterval
	}
	if log == nil {
		log = slog.Default()
	}
	log.Info("dead letter pruner started",
		"retention", retention.String(),
		"interval", interval.String(),
		"batch", batch,
	)

	pruneOnce := func() {
		cutoff := time.Now().UTC().Add(-retention)
		var deleted int64
		for {
			n, err := st.DeleteExpiredDeadLetters(ctx, cutoff, batch)
			if err != nil {
				log.Error("dead letter prune failed", "error", err, "cutoff", cutoff)
				return
			}
			deleted += n
			if n < int64(batch) {
				break
			}
		}
		if deleted > 0 {
			log.Info("dead letter prune", "deleted", deleted, "cutoff", cutoff)
		}
	}

	pruneOnce()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("dead letter pruner stopped")
			return
		case <-t.C:
			pruneOnce()
		}
	}
}
