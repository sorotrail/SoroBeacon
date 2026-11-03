package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/metrics"
)

const (
	// DefaultPartitionRunwayMonths is the default number of consecutive monthly
	// partitions starting from the current UTC month that lifecycle automation
	// keeps open (current month + two future months).
	DefaultPartitionRunwayMonths = 3

	// DefaultPartitionCheckInterval is how often the background partition
	// lifecycle loop inspects and maintains alert partitions.
	DefaultPartitionCheckInterval = 6 * time.Hour

	// DefaultPartitionMaxDrops caps the number of expired monthly partitions
	// detached and dropped in a single maintenance transaction so backlog catch-up
	// stays bounded.
	DefaultPartitionMaxDrops = 3

	// DefaultPartitionLockTimeout bounds how long partition DDL waits on table
	// locks before yielding to active alert ingestion or long-running queries.
	DefaultPartitionLockTimeout = 2 * time.Second

	// DefaultPartitionAuditActor is the audit_log actor recorded when lifecycle
	// automation drops an expired partition.
	DefaultPartitionAuditActor = "system:partition_lifecycle"

	// AuditTargetAlertPartition is the audit_log target_type for partition drops.
	AuditTargetAlertPartition = "alert_partition"
)

// PartitionDropRecord describes a single monthly partition planned for or
// removed by retention maintenance.
type PartitionDropRecord struct {
	Name          string    `json:"partition"`
	From          time.Time `json:"from"`
	To            time.Time `json:"to"`
	AlertsDropped int64     `json:"alerts_dropped"`
	Cutoff        time.Time `json:"cutoff"`
}

// PartitionMaintenancePlan reports what a maintenance pass inspected, planned,
// or executed.
type PartitionMaintenancePlan struct {
	CheckedAt     time.Time             `json:"checked_at"`
	DryRun        bool                  `json:"dry_run"`
	RunwayMonths  int                   `json:"runway_months"`
	ToCreate      []string              `json:"to_create,omitempty"`
	Created       []string              `json:"created,omitempty"`
	ToDrop        []PartitionDropRecord `json:"to_drop,omitempty"`
	Dropped       []PartitionDropRecord `json:"dropped,omitempty"`
	AlertsDropped int64                 `json:"alerts_dropped"`
	YieldedDrops  bool                  `json:"yielded_drops,omitempty"`
}

// PartitionLifecycleOptions configures one-shot or periodic alert partition
// lifecycle maintenance.
type PartitionLifecycleOptions struct {
	Now                 time.Time
	Clock               func() time.Time
	RunwayMonths        int
	CheckInterval       time.Duration
	Retention           time.Duration
	WorkspaceRetentions map[string]time.Duration
	MaxDropsPerCycle    int
	LockTimeout         time.Duration
	DryRun              bool
	Actor               string
	Archiver            AlertArchiver
	ShouldYield         func(context.Context) bool
	Metrics             *metrics.Metrics
	Log                 *slog.Logger
}

func (o PartitionLifecycleOptions) now() time.Time {
	if o.Clock != nil {
		return o.Clock().UTC()
	}
	if !o.Now.IsZero() {
		return o.Now.UTC()
	}
	return time.Now().UTC()
}

func (o PartitionLifecycleOptions) runwayMonths() int {
	if o.RunwayMonths > 0 {
		return o.RunwayMonths
	}
	return DefaultPartitionRunwayMonths
}

func (o PartitionLifecycleOptions) checkInterval() time.Duration {
	if o.CheckInterval > 0 {
		return o.CheckInterval
	}
	return DefaultPartitionCheckInterval
}

func (o PartitionLifecycleOptions) maxDrops() int {
	if o.MaxDropsPerCycle > 0 {
		return o.MaxDropsPerCycle
	}
	return DefaultPartitionMaxDrops
}

func (o PartitionLifecycleOptions) lockTimeout() time.Duration {
	if o.LockTimeout > 0 {
		return o.LockTimeout
	}
	return DefaultPartitionLockTimeout
}

func (o PartitionLifecycleOptions) actor() string {
	if strings.TrimSpace(o.Actor) != "" {
		return strings.TrimSpace(o.Actor)
	}
	return DefaultPartitionAuditActor
}

func (o PartitionLifecycleOptions) logger() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.Default()
}

// PartitionLifecycleManager is implemented by backends that support full
// partition lifecycle planning, dry-run inspection, ahead-of-time creation,
// and retention-safe partition dropping.
type PartitionLifecycleManager interface {
	MaintainAlertPartitions(ctx context.Context, opts PartitionLifecycleOptions) (*PartitionMaintenancePlan, error)
}

// ComputePartitionRunway returns how many consecutive monthly partitions
// starting at the UTC month containing `from` exist in `existing`. If the
// current month's partition is missing, runway is 0.
func ComputePartitionRunway(existing []string, from time.Time) int {
	have := make(map[string]bool, len(existing))
	for _, name := range existing {
		have[name] = true
	}
	const maxRunwayScan = 1200
	runway := 0
	for i := 0; i < maxRunwayScan; i++ {
		name := alertPartitionName(addUTCMonths(from, i))
		if !have[name] {
			break
		}
		runway++
	}
	return runway
}

// EffectiveRetentionCutoff returns the conservative timestamp cutoff before
// which a whole monthly partition is eligible to be dropped.
//
// Because a monthly partition is shared across every workspace, dropping it
// removes alerts for all tenants at once. Therefore:
//   - If any workspace in workspaceRetentions has a non-positive retention
//     (meaning that workspace retains alerts indefinitely), whole-partition
//     dropping is disabled and ok is false.
//   - Otherwise the effective retention window is the maximum positive
//     duration across globalRetention and all workspaceRetentions.
//   - If neither globalRetention nor workspaceRetentions specifies a positive
//     duration, ok is false.
func EffectiveRetentionCutoff(now time.Time, globalRetention time.Duration, workspaceRetentions map[string]time.Duration) (cutoff time.Time, ok bool) {
	effective := globalRetention
	for _, wsRetention := range workspaceRetentions {
		if wsRetention <= 0 {
			return time.Time{}, false
		}
		if wsRetention > effective {
			effective = wsRetention
		}
	}
	if effective <= 0 {
		return time.Time{}, false
	}
	return now.UTC().Add(-effective), true
}

// ExpiredAlertPartitions returns the monthly partitions in `existing` whose
// entire half-open interval [from, to) is at or before `cutoff`, ordered
// oldest first. The default partition and any non-monthly child tables are
// never returned.
func ExpiredAlertPartitions(existing []string, cutoff time.Time) []PartitionDropRecord {
	var drops []PartitionDropRecord
	cutoffUTC := cutoff.UTC()
	for _, name := range existing {
		from, to, ok := alertPartitionBounds(name)
		if !ok {
			continue
		}
		if !to.After(cutoffUTC) {
			drops = append(drops, PartitionDropRecord{
				Name:   name,
				From:   from,
				To:     to,
				Cutoff: cutoffUTC,
			})
		}
	}
	sort.Slice(drops, func(i, j int) bool { return drops[i].From.Before(drops[j].From) })
	return drops
}

func partitionTargetID(from time.Time) int64 {
	m := monthStart(from)
	return int64(m.Year()*100 + int(m.Month()))
}

func formatLockTimeoutSQL(d time.Duration) string {
	ms := d.Milliseconds()
	if ms <= 0 {
		ms = 1
	}
	return fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", ms)
}

func (p *Postgres) ensureAlertPartitions(ctx context.Context, from time.Time, months int, lockTimeout time.Duration) ([]string, error) {
	if months <= 0 {
		return nil, nil
	}
	existing, err := p.alertPartitions(ctx)
	if err != nil {
		p.metrics.RecordAlertPartitionError("inspect")
		return nil, err
	}
	have := make(map[string]bool, len(existing))
	hasDefault := false
	for _, name := range existing {
		have[name] = true
		if name == alertDefaultPartition {
			hasDefault = true
		}
	}

	required := requiredAlertPartitions(from, months)
	var missing []string
	for _, name := range required {
		if !have[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		p.metrics.SetAlertPartitionRunway(ComputePartitionRunway(existing, from))
		return nil, nil
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		p.metrics.RecordAlertPartitionError("create")
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	if lockTimeout > 0 {
		if _, err := tx.Exec(ctx, formatLockTimeoutSQL(lockTimeout)); err != nil {
			p.metrics.RecordAlertPartitionError("create")
			return nil, err
		}
	}

	var created []string
	for _, name := range required {
		begin, end, _ := alertPartitionBounds(name)
		if have[name] {
			continue
		}

		inDefault := false
		if hasDefault {
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM `+alertDefaultPartition+` WHERE created_at >= $1 AND created_at < $2)`,
				begin, end).Scan(&inDefault); err != nil {
				p.metrics.RecordAlertPartitionError("create")
				return nil, err
			}
		}

		if !inDefault {
			if _, err := tx.Exec(ctx, createPartitionSQL(name, begin, end)); err != nil {
				p.metrics.RecordAlertPartitionError("create")
				return nil, err
			}
			created = append(created, name)
			have[name] = true
			continue
		}

		if _, err := tx.Exec(ctx, `ALTER TABLE `+alertPartitionTable+` DETACH PARTITION `+alertDefaultPartition); err != nil {
			p.metrics.RecordAlertPartitionError("create")
			return nil, err
		}
		if _, err := tx.Exec(ctx, createPartitionSQL(name, begin, end)); err != nil {
			p.metrics.RecordAlertPartitionError("create")
			return nil, err
		}
		move := fmt.Sprintf(`
			INSERT INTO %s (id, monitor_id, rule_id, event_id, payload, ledger, retracted_at, created_at)
			SELECT id, monitor_id, rule_id, event_id, payload, ledger, retracted_at, created_at
			  FROM %s WHERE created_at >= '%s' AND created_at < '%s'`,
			name, alertDefaultPartition, begin.Format("2006-01-02"), end.Format("2006-01-02"))
		if _, err := tx.Exec(ctx, move); err != nil {
			p.metrics.RecordAlertPartitionError("create")
			return nil, err
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			`DELETE FROM %s WHERE created_at >= '%s' AND created_at < '%s'`,
			alertDefaultPartition, begin.Format("2006-01-02"), end.Format("2006-01-02"))); err != nil {
			p.metrics.RecordAlertPartitionError("create")
			return nil, err
		}
		if _, err := tx.Exec(ctx, `ALTER TABLE `+alertPartitionTable+` ATTACH PARTITION `+alertDefaultPartition+` DEFAULT`); err != nil {
			p.metrics.RecordAlertPartitionError("create")
			return nil, err
		}
		created = append(created, name)
		have[name] = true
	}

	if err := tx.Commit(ctx); err != nil {
		p.metrics.RecordAlertPartitionError("create")
		return nil, err
	}

	p.metrics.RecordAlertPartitionsCreated(len(created))
	afterNames := make([]string, 0, len(have))
	for name := range have {
		afterNames = append(afterNames, name)
	}
	p.metrics.SetAlertPartitionRunway(ComputePartitionRunway(afterNames, from))
	return created, nil
}

func (p *Postgres) dropExpiredPartitions(ctx context.Context, cutoff time.Time, maxDrops int, actor string, lockTimeout time.Duration) (int64, []PartitionDropRecord, error) {
	names, err := p.alertPartitions(ctx)
	if err != nil {
		p.metrics.RecordAlertPartitionError("inspect")
		return 0, nil, err
	}
	drops := ExpiredAlertPartitions(names, cutoff)
	if len(drops) == 0 {
		return 0, nil, nil
	}
	if maxDrops > 0 && len(drops) > maxDrops {
		drops = drops[:maxDrops]
	}
	if strings.TrimSpace(actor) == "" {
		actor = DefaultPartitionAuditActor
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		p.metrics.RecordAlertPartitionError("drop")
		return 0, nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	if lockTimeout > 0 {
		if _, err := tx.Exec(ctx, formatLockTimeoutSQL(lockTimeout)); err != nil {
			p.metrics.RecordAlertPartitionError("drop")
			return 0, nil, err
		}
	}

	var totalDropped int64
	droppedRecords := make([]PartitionDropRecord, 0, len(drops))
	for _, d := range drops {
		var n int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+d.Name).Scan(&n); err != nil {
			p.metrics.RecordAlertPartitionError("drop")
			return 0, nil, err
		}
		// Delete by the dropped partition's own alert ids rather than by a
		// timestamp window: ids are globally unique, so this is exact even if
		// an alert's created_at was rewritten (which relocates the row but not
		// necessarily its dedup key).
		if _, err := tx.Exec(ctx,
			`DELETE FROM delivery_attempts WHERE alert_id IN (SELECT id FROM `+d.Name+`)`); err != nil {
			p.metrics.RecordAlertPartitionError("drop")
			return 0, nil, err
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM alert_dedup WHERE alert_id IN (SELECT id FROM `+d.Name+`)`); err != nil {
			p.metrics.RecordAlertPartitionError("drop")
			return 0, nil, err
		}
		// DETACH first: Postgres keeps a per-partition foreign-key constraint
		// on delivery_attempts for every child, so a bare DROP is refused.
		// Detaching removes that dependency, and the detached table is then a
		// plain table that can be dropped.
		if _, err := tx.Exec(ctx, `ALTER TABLE `+alertPartitionTable+` DETACH PARTITION `+d.Name); err != nil {
			p.metrics.RecordAlertPartitionError("drop")
			return 0, nil, err
		}
		if _, err := tx.Exec(ctx, `DROP TABLE `+d.Name); err != nil {
			p.metrics.RecordAlertPartitionError("drop")
			return 0, nil, err
		}

		d.AlertsDropped = n
		diffPayload, err := json.Marshal(d)
		if err != nil {
			p.metrics.RecordAlertPartitionError("drop")
			return 0, nil, err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO audit_log (actor, action, target_type, target_id, diff) VALUES ($1, $2, $3, $4, $5)`,
			actor, AuditActionDelete, AuditTargetAlertPartition, partitionTargetID(d.From), diffPayload); err != nil {
			p.metrics.RecordAlertPartitionError("drop")
			return 0, nil, err
		}

		totalDropped += n
		droppedRecords = append(droppedRecords, d)
	}

	if err := tx.Commit(ctx); err != nil {
		p.metrics.RecordAlertPartitionError("drop")
		return 0, nil, err
	}
	p.metrics.RecordAlertPartitionsDropped(len(droppedRecords))
	return totalDropped, droppedRecords, nil
}

// MaintainAlertPartitions inspects, plans, and (unless DryRun is set) executes
// ahead-of-time partition creation and retention-safe partition drops.
func (p *Postgres) MaintainAlertPartitions(ctx context.Context, opts PartitionLifecycleOptions) (*PartitionMaintenancePlan, error) {
	now := opts.now()
	runwayRequested := opts.runwayMonths()
	lockTimeout := opts.lockTimeout()
	maxDrops := opts.maxDrops()
	actor := opts.actor()

	existing, err := p.alertPartitions(ctx)
	if err != nil {
		p.metrics.RecordAlertPartitionError("inspect")
		if opts.Metrics != nil && opts.Metrics != p.metrics {
			opts.Metrics.RecordAlertPartitionError("inspect")
		}
		return nil, err
	}

	have := make(map[string]bool, len(existing))
	for _, name := range existing {
		have[name] = true
	}

	required := requiredAlertPartitions(now, runwayRequested)
	var toCreate []string
	for _, name := range required {
		if !have[name] {
			toCreate = append(toCreate, name)
		}
	}

	var toDrop []PartitionDropRecord
	cutoff, canDrop := EffectiveRetentionCutoff(now, opts.Retention, opts.WorkspaceRetentions)
	if canDrop && opts.Archiver == nil {
		toDrop = ExpiredAlertPartitions(existing, cutoff)
		if maxDrops > 0 && len(toDrop) > maxDrops {
			toDrop = toDrop[:maxDrops]
		}
	}

	plan := &PartitionMaintenancePlan{
		CheckedAt:    now,
		DryRun:       opts.DryRun,
		RunwayMonths: ComputePartitionRunway(existing, now),
		ToCreate:     toCreate,
		ToDrop:       toDrop,
	}

	if opts.DryRun {
		for i := range plan.ToDrop {
			var n int64
			if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM `+plan.ToDrop[i].Name).Scan(&n); err != nil {
				p.metrics.RecordAlertPartitionError("inspect")
				if opts.Metrics != nil && opts.Metrics != p.metrics {
					opts.Metrics.RecordAlertPartitionError("inspect")
				}
				return nil, err
			}
			plan.ToDrop[i].AlertsDropped = n
			plan.AlertsDropped += n
		}
		p.metrics.SetAlertPartitionRunway(plan.RunwayMonths)
		if opts.Metrics != nil && opts.Metrics != p.metrics {
			opts.Metrics.SetAlertPartitionRunway(plan.RunwayMonths)
		}
		return plan, nil
	}

	if len(toCreate) > 0 {
		created, err := p.ensureAlertPartitions(ctx, now, runwayRequested, lockTimeout)
		if err != nil {
			if opts.Metrics != nil && opts.Metrics != p.metrics {
				opts.Metrics.RecordAlertPartitionError("create")
			}
			return plan, err
		}
		plan.Created = created
		if opts.Metrics != nil && opts.Metrics != p.metrics {
			opts.Metrics.RecordAlertPartitionsCreated(len(created))
		}
	}

	if len(toDrop) > 0 {
		if opts.ShouldYield != nil && opts.ShouldYield(ctx) {
			plan.YieldedDrops = true
		} else {
			alertsDropped, dropped, err := p.dropExpiredPartitions(ctx, cutoff, maxDrops, actor, lockTimeout)
			if err != nil {
				if opts.Metrics != nil && opts.Metrics != p.metrics {
					opts.Metrics.RecordAlertPartitionError("drop")
				}
				return plan, err
			}
			plan.Dropped = dropped
			plan.AlertsDropped = alertsDropped
			if opts.Metrics != nil && opts.Metrics != p.metrics {
				opts.Metrics.RecordAlertPartitionsDropped(len(dropped))
			}
		}
	}

	updated, err := p.alertPartitions(ctx)
	if err != nil {
		p.metrics.RecordAlertPartitionError("inspect")
		if opts.Metrics != nil && opts.Metrics != p.metrics {
			opts.Metrics.RecordAlertPartitionError("inspect")
		}
		return plan, err
	}
	plan.RunwayMonths = ComputePartitionRunway(updated, now)
	p.metrics.SetAlertPartitionRunway(plan.RunwayMonths)
	if opts.Metrics != nil && opts.Metrics != p.metrics {
		opts.Metrics.SetAlertPartitionRunway(plan.RunwayMonths)
	}
	if plan.RunwayMonths < runwayRequested {
		p.metrics.RecordAlertPartitionError("runway")
		if opts.Metrics != nil && opts.Metrics != p.metrics {
			opts.Metrics.RecordAlertPartitionError("runway")
		}
		return plan, fmt.Errorf("alert partition runway %d is below required %d months", plan.RunwayMonths, runwayRequested)
	}
	return plan, nil
}

// MaintainAlertPartitionsOnce executes a single partition maintenance pass
// against any backend implementing PartitionLifecycleManager or PartitionEnsurer.
// Backends without partitioning (such as SQLite) return a nil plan and nil error.
func MaintainAlertPartitionsOnce(ctx context.Context, st any, opts PartitionLifecycleOptions) (*PartitionMaintenancePlan, error) {
	log := opts.logger()
	if mgr, ok := st.(PartitionLifecycleManager); ok {
		plan, err := mgr.MaintainAlertPartitions(ctx, opts)
		if err != nil {
			log.Error("alert partition maintenance failed",
				"error", err,
				"dry_run", opts.DryRun,
				"runway_requested", opts.runwayMonths(),
			)
			return plan, err
		}
		if plan != nil && (len(plan.Created) > 0 || len(plan.Dropped) > 0 || opts.DryRun) {
			log.Info("alert partition maintenance completed",
				"dry_run", plan.DryRun,
				"runway_months", plan.RunwayMonths,
				"created", len(plan.Created),
				"dropped", len(plan.Dropped),
				"alerts_dropped", plan.AlertsDropped,
				"yielded_drops", plan.YieldedDrops,
			)
		}
		return plan, nil
	}

	pe, hasEnsurer := st.(PartitionEnsurer)
	rc, hasChecker := st.(PartitionRunwayChecker)
	if !hasEnsurer && !hasChecker {
		return nil, nil
	}

	now := opts.now()
	runwayRequested := opts.runwayMonths()
	plan := &PartitionMaintenancePlan{
		CheckedAt: now,
		DryRun:    opts.DryRun,
	}

	if !opts.DryRun && hasEnsurer {
		if err := pe.EnsureAlertPartitions(ctx, now, runwayRequested); err != nil {
			opts.Metrics.RecordAlertPartitionError("create")
			log.Error("alert partition maintenance failed",
				"operation", "create",
				"error", err,
				"runway_requested", runwayRequested,
			)
			return plan, err
		}
	}

	if hasChecker {
		if err := rc.CheckAlertPartitionRunway(ctx, now, runwayRequested); err != nil {
			opts.Metrics.RecordAlertPartitionError("runway")
			log.Error("alert partition maintenance failed",
				"operation", "runway",
				"error", err,
				"runway_requested", runwayRequested,
			)
			return plan, err
		}
	}
	plan.RunwayMonths = runwayRequested
	opts.Metrics.SetAlertPartitionRunway(runwayRequested)

	if !opts.DryRun && opts.Archiver == nil {
		if cutoff, ok := EffectiveRetentionCutoff(now, opts.Retention, opts.WorkspaceRetentions); ok {
			if opts.ShouldYield != nil && opts.ShouldYield(ctx) {
				plan.YieldedDrops = true
			} else if pp, ok := st.(PartitionPruner); ok {
				n, err := pp.DropExpiredPartitions(ctx, cutoff)
				if err != nil {
					opts.Metrics.RecordAlertPartitionError("drop")
					log.Error("alert partition maintenance failed",
						"operation", "drop",
						"error", err,
						"cutoff", cutoff,
					)
					return plan, err
				}
				plan.AlertsDropped = n
			}
		}
	}

	return plan, nil
}

// RunAlertPartitionLifecycle periodically maintains future alert partitions and
// reports partition runway metrics until ctx is cancelled. It runs even when
// ALERT_RETENTION is unset so long-lived deployments never cross a month
// boundary without a partition in place.
func RunAlertPartitionLifecycle(ctx context.Context, st any, opts PartitionLifecycleOptions) {
	if _, ok := st.(PartitionLifecycleManager); !ok {
		if _, ok := st.(PartitionEnsurer); !ok {
			if _, ok := st.(PartitionRunwayChecker); !ok {
				return
			}
		}
	}
	log := opts.logger()
	interval := opts.checkInterval()
	log.Info("alert partition lifecycle started",
		"runway_months", opts.runwayMonths(),
		"interval", interval.String(),
		"dry_run", opts.DryRun,
	)

	_, _ = MaintainAlertPartitionsOnce(ctx, st, opts)

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("alert partition lifecycle stopped")
			return
		case <-t.C:
			_, _ = MaintainAlertPartitionsOnce(ctx, st, opts)
		}
	}
}

var _ PartitionLifecycleManager = (*Postgres)(nil)
