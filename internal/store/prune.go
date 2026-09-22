package store

import (
	"context"
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
	tag, err := p.pool.Exec(ctx, `
		DELETE FROM alerts
		WHERE id IN (
			SELECT id FROM alerts
			WHERE created_at < $1
			ORDER BY created_at ASC, id ASC
			LIMIT $2
		)`, cutoff, limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// AlertPruner is the persistence slice the retention loop needs.
type AlertPruner interface {
	DeleteExpiredAlerts(ctx context.Context, cutoff time.Time, limit int) (int64, error)
}

// RunAlertPruner deletes alerts older than retention in batches until
// ctx is cancelled. Unset/zero retention is a no-op so upgrades never
// start deleting history. One pass runs immediately, then on interval
// (DefaultPruneInterval when interval is not positive).
func RunAlertPruner(ctx context.Context, st AlertPruner, retention, interval time.Duration, batch int, log *slog.Logger) {
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
	)

	pruneOnce := func() {
		cutoff := time.Now().UTC().Add(-retention)
		var deleted int64
		for {
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
		log.Info("alert retention prune", "deleted", deleted, "cutoff", cutoff)
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
