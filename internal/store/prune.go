package store

import (
	"context"
	"errors"
	"log/slog"
	"time"
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

// RunAlertPruner deletes alerts older than retention in batches until
// ctx is cancelled. Unset/zero retention is a no-op so upgrades never
// start deleting history. One pass runs immediately, then on interval
// (DefaultPruneInterval when interval is not positive).
//
// arch, when non-nil, is asked to persist each batch first; the delete for
// that batch is skipped if the archive fails, so archiving turns retention
// from destruction into tiering. A nil arch is byte-for-byte the previous
// behaviour.
func RunAlertPruner(ctx context.Context, st AlertPruner, retention, interval time.Duration, batch int, arch AlertArchiver, log *slog.Logger) {
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
	log.Info("alert retention pruner started",
		"retention", retention.String(),
		"interval", interval.String(),
		"batch", batch,
		"archiving", arch != nil,
	)

	pruneOnce := func() {
		cutoff := time.Now().UTC().Add(-retention)
		// Keep the upcoming partition window open before anything is written
		// into it; on a partitioned backend this is what stops rows landing in
		// the default partition.
		if pe, ok := st.(PartitionEnsurer); ok {
			if err := pe.EnsureAlertPartitions(ctx, time.Now().UTC(), 3); err != nil {
				log.Error("ensure alert partitions failed", "error", err)
				return
			}
		}

		var deleted int64
		// A partitioned backend drops whole expired partitions first, which is
		// effectively free. It is skipped when archiving is on: a DROP cannot
		// archive, so those rows must go through the batched archive-then-delete
		// path above instead.
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
