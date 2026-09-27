package store

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"time"
)

// The alerts table is range-partitioned by created_at in the Postgres
// backend, one partition per UTC month, named alerts_YYYY_MM, plus a DEFAULT
// partition that keeps a row from being lost when its month has no partition
// yet. SQLite has no partitioning and does not implement these interfaces.
const (
	alertPartitionTable   = "alerts"
	alertDefaultPartition = "alerts_default"
)

// PartitionEnsurer creates upcoming alerts partitions. Implemented by the
// Postgres backend; a backend without partitioning simply does not implement
// it, so callers type-assert rather than widen the Store interface.
type PartitionEnsurer interface {
	// EnsureAlertPartitions makes sure a partition exists for `months`
	// consecutive months starting at the month containing `from`. It is
	// idempotent and safe to call on every start and prune pass.
	EnsureAlertPartitions(ctx context.Context, from time.Time, months int) error
}

// PartitionPruner drops whole alerts partitions that are entirely older than
// a cutoff, returning how many alerts went with them.
type PartitionPruner interface {
	DropExpiredPartitions(ctx context.Context, cutoff time.Time) (alertsDropped int64, err error)
}

var alertPartitionPattern = regexp.MustCompile(`^alerts_(\d{4})_(\d{2})$`)

// monthStart truncates t to the first instant of its UTC month.
func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// alertPartitionName is the partition holding the month that contains t.
func alertPartitionName(t time.Time) string {
	m := monthStart(t)
	return fmt.Sprintf("alerts_%04d_%02d", m.Year(), int(m.Month()))
}

// alertPartitionBounds parses a partition name into its half-open [from, to)
// bounds. ok is false for the default partition or anything that is not a
// monthly partition, so an unexpected child is ignored rather than dropped.
func alertPartitionBounds(name string) (from, to time.Time, ok bool) {
	m := alertPartitionPattern.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, time.Time{}, false
	}
	var year, month int
	if _, err := fmt.Sscanf(m[1], "%d", &year); err != nil {
		return time.Time{}, time.Time{}, false
	}
	if _, err := fmt.Sscanf(m[2], "%d", &month); err != nil || month < 1 || month > 12 {
		return time.Time{}, time.Time{}, false
	}
	from = time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
	return from, from.AddDate(0, 1, 0), true
}

// alertPartitions lists the child partitions currently attached to alerts.
// The names come from the catalog and are only ever interpolated after being
// matched against alertPartitionPattern or compared to alertDefaultPartition.
func (p *Postgres) alertPartitions(ctx context.Context) ([]string, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT child.relname
		  FROM pg_inherits i
		  JOIN pg_class child ON child.oid = i.inhrelid
		  JOIN pg_class parent ON parent.oid = i.inhparent
		 WHERE parent.relname = $1`, alertPartitionTable)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// EnsureAlertPartitions creates the monthly partitions for [from, from+months).
// A month that already has a partition is skipped. When the default partition
// holds rows for a month about to get a real partition, the default is
// detached, the rows are moved into the new partition, and the default is
// reattached — Postgres refuses to create a partition that would overlap rows
// already in the default, so this is the one correct order.
func (p *Postgres) EnsureAlertPartitions(ctx context.Context, from time.Time, months int) error {
	if months <= 0 {
		return nil
	}
	existing, err := p.alertPartitions(ctx)
	if err != nil {
		return err
	}
	have := make(map[string]bool, len(existing))
	hasDefault := false
	for _, name := range existing {
		have[name] = true
		if name == alertDefaultPartition {
			hasDefault = true
		}
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	start := monthStart(from)
	for i := 0; i < months; i++ {
		begin := start.AddDate(0, i, 0)
		end := begin.AddDate(0, 1, 0)
		name := alertPartitionName(begin)
		if have[name] {
			continue
		}

		inDefault := false
		if hasDefault {
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM `+alertDefaultPartition+` WHERE created_at >= $1 AND created_at < $2)`,
				begin, end).Scan(&inDefault); err != nil {
				return err
			}
		}

		if !inDefault {
			if _, err := tx.Exec(ctx, createPartitionSQL(name, begin, end)); err != nil {
				return err
			}
			continue
		}

		if _, err := tx.Exec(ctx, `ALTER TABLE `+alertPartitionTable+` DETACH PARTITION `+alertDefaultPartition); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, createPartitionSQL(name, begin, end)); err != nil {
			return err
		}
		move := fmt.Sprintf(`
			INSERT INTO %s (id, monitor_id, rule_id, event_id, payload, ledger, retracted_at, created_at)
			SELECT id, monitor_id, rule_id, event_id, payload, ledger, retracted_at, created_at
			  FROM %s WHERE created_at >= '%s' AND created_at < '%s'`,
			name, alertDefaultPartition, begin.Format("2006-01-02"), end.Format("2006-01-02"))
		if _, err := tx.Exec(ctx, move); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			`DELETE FROM %s WHERE created_at >= '%s' AND created_at < '%s'`,
			alertDefaultPartition, begin.Format("2006-01-02"), end.Format("2006-01-02"))); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `ALTER TABLE `+alertPartitionTable+` ATTACH PARTITION `+alertDefaultPartition+` DEFAULT`); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// createPartitionSQL builds a CREATE TABLE ... PARTITION statement. Names are
// generated by alertPartitionName from validated year/month values and the
// bounds are formatted dates, never user input, so interpolation is safe here
// (Postgres does not accept bind parameters in DDL anyway).
func createPartitionSQL(name string, from, to time.Time) string {
	return fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`,
		name, alertPartitionTable, from.Format("2006-01-02"), to.Format("2006-01-02"))
}

// DropExpiredPartitions drops every monthly alerts partition whose whole range
// is older than cutoff, oldest first.
//
// Dropping a partition does not fire ON DELETE CASCADE, so the delivery
// attempts and dedup keys that referenced the dropped alerts are removed
// explicitly in the same transaction. That is the deliberate handling of the
// parent/child relationship: retention owns both sides of it here rather than
// relying on the FK, which a DROP cannot trigger.
func (p *Postgres) DropExpiredPartitions(ctx context.Context, cutoff time.Time) (int64, error) {
	names, err := p.alertPartitions(ctx)
	if err != nil {
		return 0, err
	}
	type expiredPartition struct {
		name     string
		from, to time.Time
	}
	var drops []expiredPartition
	for _, name := range names {
		from, to, ok := alertPartitionBounds(name)
		if !ok {
			continue
		}
		if !to.After(cutoff) {
			drops = append(drops, expiredPartition{name: name, from: from, to: to})
		}
	}
	if len(drops) == 0 {
		return 0, nil
	}
	sort.Slice(drops, func(i, j int) bool { return drops[i].from.Before(drops[j].from) })

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	var dropped int64
	for _, d := range drops {
		var n int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+d.name).Scan(&n); err != nil {
			return 0, err
		}
		// Delete by the dropped partition's own alert ids rather than by a
		// timestamp window: ids are globally unique, so this is exact even if
		// an alert's created_at was rewritten (which relocates the row but not
		// necessarily its dedup key).
		if _, err := tx.Exec(ctx,
			`DELETE FROM delivery_attempts WHERE alert_id IN (SELECT id FROM `+d.name+`)`); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM alert_dedup WHERE alert_id IN (SELECT id FROM `+d.name+`)`); err != nil {
			return 0, err
		}
		// DETACH first: Postgres keeps a per-partition foreign-key constraint
		// on delivery_attempts for every child, so a bare DROP is refused.
		// Detaching removes that dependency, and the detached table is then a
		// plain table that can be dropped.
		if _, err := tx.Exec(ctx, `ALTER TABLE `+alertPartitionTable+` DETACH PARTITION `+d.name); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `DROP TABLE `+d.name); err != nil {
			return 0, err
		}
		dropped += n
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return dropped, nil
}

var _ PartitionEnsurer = (*Postgres)(nil)
var _ PartitionPruner = (*Postgres)(nil)
