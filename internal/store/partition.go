package store

import (
	"context"
	"fmt"
	"regexp"
	"strings"
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

// PartitionRunwayChecker reports whether the requested upcoming alert
// partitions already exist. Backends without partitioning do not implement it.
type PartitionRunwayChecker interface {
	CheckAlertPartitionRunway(ctx context.Context, from time.Time, months int) error
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

// addUTCMonths returns the first instant of the UTC month that is `months`
// calendar months offset from the month containing `t`. Anchoring on day 1 of
// the UTC month before applying the month offset avoids Go's time.AddDate day
// normalization overflow across months of unequal length (for example,
// Jan 31 + 1 month overflowing Feb 28/29 into March) as well as DST and
// non-UTC timezone shifts.
func addUTCMonths(t time.Time, months int) time.Time {
	start := monthStart(t)
	return time.Date(start.Year(), start.Month()+time.Month(months), 1, 0, 0, 0, 0, time.UTC)
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
	return from, addUTCMonths(from, 1), true
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

// CheckAlertPartitionRunway inspects, but never creates, the monthly
// partitions needed from `from` through the requested runway.
func (p *Postgres) CheckAlertPartitionRunway(ctx context.Context, from time.Time, months int) error {
	if months <= 0 {
		return nil
	}
	existing, err := p.alertPartitions(ctx)
	if err != nil {
		p.metrics.RecordAlertPartitionError("inspect")
		return err
	}
	runway := ComputePartitionRunway(existing, from)
	p.metrics.SetAlertPartitionRunway(runway)
	have := make(map[string]bool, len(existing))
	for _, name := range existing {
		have[name] = true
	}
	var missing []string
	for _, name := range requiredAlertPartitions(from, months) {
		if !have[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		p.metrics.RecordAlertPartitionError("runway")
		return fmt.Errorf("missing alert partitions: %s", strings.Join(missing, ", "))
	}
	return nil
}

func requiredAlertPartitions(from time.Time, months int) []string {
	if months <= 0 {
		return nil
	}
	names := make([]string, 0, months)
	for i := 0; i < months; i++ {
		names = append(names, alertPartitionName(addUTCMonths(from, i)))
	}
	return names
}

// EnsureAlertPartitions creates the monthly partitions for [from, from+months).
// A month that already has a partition is skipped. When the default partition
// holds rows for a month about to get a real partition, the default is
// detached, the rows are moved into the new partition, and the default is
// reattached — Postgres refuses to create a partition that would overlap rows
// already in the default, so this is the one correct order.
func (p *Postgres) EnsureAlertPartitions(ctx context.Context, from time.Time, months int) error {
	_, err := p.ensureAlertPartitions(ctx, from, months, DefaultPartitionLockTimeout)
	return err
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
// is older than cutoff, oldest first, and records an audit_log entry for each
// dropped partition.
//
// Dropping a partition does not fire ON DELETE CASCADE, so the delivery
// attempts and dedup keys that referenced the dropped alerts are removed
// explicitly in the same transaction. That is the deliberate handling of the
// parent/child relationship: retention owns both sides of it here rather than
// relying on the FK, which a DROP cannot trigger.
func (p *Postgres) DropExpiredPartitions(ctx context.Context, cutoff time.Time) (int64, error) {
	dropped, _, err := p.dropExpiredPartitions(ctx, cutoff, 0, DefaultPartitionAuditActor, DefaultPartitionLockTimeout)
	return dropped, err
}

var _ PartitionEnsurer = (*Postgres)(nil)
var _ PartitionRunwayChecker = (*Postgres)(nil)
var _ PartitionPruner = (*Postgres)(nil)
