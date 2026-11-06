package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Read-replica routing for the Postgres backend.
//
// A deployment may point REPLICA_DATABASE_URL at a streaming replica and have
// the read-only half of the API served from it, leaving the primary's
// connections for the poller's writes. Routing is opt-in: with the variable
// unset, Postgres holds one pool and every read and write goes to it, exactly
// as before this file existed.
//
// What is routed, and what is not
//
// A read is routable when a stale answer is harmless or merely visible, which
// rules out anything a client reads immediately after writing through this
// process. The explicit allowlist lives in ReplicaRoutedMethods below, and
// TestOnlyReplicaRoutedMethodsUseReplicaHelpers enforces that no other method
// in internal/store calls queryRows or queryRowFallback.
//
// Everything else stays on the primary, deliberately:
//
//	GetMonitor, ListMonitors, GetRule, ListRules, GetChannel,
//	ListChannels, ListChannelsForMonitor, GetSavedSearch, GetMonitorTemplate
//		read-after-write: all of these are read back by the same request (or
//		the next one) that just wrote the row, and a lagging replica would
//		show the caller its own edit missing.
//	GetAlert, ListDeliveryAttempts
//		show a just-delivered alert and its delivery attempts, which are
//		written and then immediately displayed.
//	GetIngestState, LedgerHashes
//		the poller's own bookkeeping. It reads these to decide what to
//		write next, so a stale row is not a display problem but a
//		correctness one — a lagging read can make the poller re-scan or
//		skip ledgers.
//	DueEscalations, ScheduleEscalation, AdvanceEscalation, CompleteEscalation
//		the escalation scheduler's state machine, which reads due steps right
//		after writes and acknowledgements.
//	ExpiredAlerts
//		read by the pruner to decide what to archive and delete. Archiving
//		and deleting must agree on the same row set, and a replica's view
//		of "expired" is a moving target.
//
// A caller that needs a routed read on the primary (the rules engine
// rebuilding a rule's match log, for example) uses PrimaryReader rather than
// turning routing off process-wide.
const (
	// poolPrimary and poolReplica are the closed set of values for the
	// `pool` label on sorobeacon_store_reads_total.
	poolPrimary = "primary"
	poolReplica = "replica"

	// DefaultMaxReplicaLag is the default replication lag ceiling beyond which
	// routed reads fall back to the primary pool (#572).
	DefaultMaxReplicaLag = 30 * time.Second
)

// ReplicaRoutedMethods returns the explicit, reviewed allowlist of store
// functions that may route read-only queries to the Postgres read replica via
// queryRows or queryRowFallback, mapped to the reason each can tolerate
// bounded replication lag (#572).
//
// Any new store method defaults to the primary pool and must be added here
// deliberately to use the replica; TestOnlyReplicaRoutedMethodsUseReplicaHelpers
// fails the build if an unlisted function calls queryRows or queryRowFallback.
func ReplicaRoutedMethods() map[string]string {
	return map[string]string{
		"ListMonitorsPage":            "dashboard and API paginated monitor search; read-after-write callers use GetMonitor/ListMonitors on the primary",
		"ListDeletedMonitors":         "dashboard and API trash listing; tolerates bounded replication lag",
		"GetChannelDeliveryStats":     "rolling window aggregate of delivery attempts for channel health panels",
		"ChannelDeliveryChangeKey":    "conditional ETag/change key check for channel delivery statistics",
		"GetChannelAckReconciliation": "read-only channel acknowledgement reconciliation summary",
		"ListAlerts":                  "dashboard and API alert history search (with cursor-anchor fallback to primary if cursor ID has not yet replicated)",
		"AlertChangeKey":              "conditional ETag/change key check for the alert listing",
		"LatestRetentionGap":          "operator status banner reporting the latest ledger retention gap",
		"ListArchivedEvents":          "historical raw event archive search",
		"CountArchivedEvents":         "historical raw event archive count",
		"GetStats":                    "headline dashboard and /api/v1/stats counters",
		"AlertCountsByDay":            "daily alert histogram on the overview dashboard",
		"GetMonitorStats":             "per-monitor aggregate statistics on the monitor detail page",
		"readAlertStatisticsRollup":   "pre-computed alert statistics rollup read backing GetAlertStatistics",
		"liveAlertStatistics":         "fallback live alert statistics aggregate backing GetAlertStatistics",
	}
}

// PrimaryOnlyCriticalMethods lists the store methods whose correctness depends
// on reading the primary's latest committed state (leader lease, ingest
// cursors, due escalations, retention pruning, and immediate read-after-write
// lookups), mapped to why routing them to a replica would be a bug (#572).
func PrimaryOnlyCriticalMethods() map[string]string {
	return map[string]string{
		"GetIngestState":          "poller ledger cursor must never read a lagging replica or it will re-scan or skip ledgers",
		"SetIngestState":          "writes poller ledger cursor on the primary",
		"LedgerHashes":            "reorg detection window must see the latest committed ledger hashes",
		"ListContractCursors":     "per-contract poller cursors must reflect the latest committed ledger",
		"DueEscalations":          "escalation scheduler must see acknowledgements and step advances committed moments earlier",
		"ScheduleEscalation":      "writes escalation state on the primary",
		"AdvanceEscalation":       "writes escalation step progress on the primary",
		"CompleteEscalation":      "completes escalation state on the primary",
		"AcknowledgeAlert":        "acknowledges an alert and cancels escalations in one primary transaction",
		"ExpiredAlerts":           "retention archiver and pruner must agree on the exact expired row set on the primary",
		"DeleteExpiredAlerts":     "deletes expired alerts on the primary",
		"CreateAlert":             "enforces alert cooldown and deduplication under primary lock",
		"GetAlert":                "read-after-write for newly created or acknowledged alerts",
		"ListDeliveryAttempts":    "read-after-write for manual delivery retries and alert detail view",
		"GetMonitor":              "read-after-write after monitor creation or update",
		"ListMonitors":            "poller and read-after-write monitor listing",
		"GetRule":                 "read-after-write after rule creation or update",
		"ListRules":               "poller rule evaluation loads rules from the primary",
		"GetChannel":              "read-after-write after channel creation or update",
		"ListChannels":            "dispatcher and read-after-write channel listing",
		"ListChannelsForMonitor":  "dispatcher loads target channels from the primary",
		"ActiveMaintenanceWindow": "delivery path suppression check must see newly created maintenance windows immediately",
	}
}

// PrimaryReader is implemented by a store that can serve a read from the
// primary explicitly, bypassing replica routing. A backend that does not route
// reads at all (SQLite) does not implement it, so callers type-assert rather
// than widen the Store interface — the same shape as PartitionEnsurer.
type PrimaryReader interface {
	// ListAlertsPrimary is ListAlerts served by the primary connection,
	// regardless of whether a replica is configured.
	ListAlertsPrimary(ctx context.Context, f AlertFilter) ([]Alert, error)
}

var _ PrimaryReader = (*Postgres)(nil)

// connectReplica opens the pool the routed reads run on, or leaves
// p.replica nil when no replica is configured.
//
// The replica is pinged at boot and a failure is fatal. That is the deliberate
// half of the contract: an operator who sets REPLICA_DATABASE_URL expects
// reads to be routed, so a replica that is misconfigured (wrong host, wrong
// credentials, a database that is not actually a replica) should fail startup
// loudly rather than quietly serve everything from the primary while the
// setting is believed to be doing work. The other half is runtime: once the
// process is up, a replica that goes away degrades to the primary through the
// per-query fallback below and is reported by
// sorobeacon_store_replica_fallbacks_total.
func (p *Postgres) connectReplica(ctx context.Context, settings PoolSettings) error {
	p.maxReplicaLag = settings.MaxReplicaLag
	if p.maxReplicaLag <= 0 {
		p.maxReplicaLag = DefaultMaxReplicaLag
	}
	if settings.ReplicaURL == "" {
		return nil
	}
	cfg, err := buildPoolConfig(settings.ReplicaURL, settings)
	if err != nil {
		return fmt.Errorf("connect postgres replica: %w", err)
	}
	replica, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connect postgres replica: %w", err)
	}
	if err := replica.Ping(ctx); err != nil {
		replica.Close()
		return fmt.Errorf("ping postgres replica: %w", err)
	}
	p.replica = replica
	p.metrics.SetReplicaEnabled(true)
	_, _ = p.SampleReplicaLag(ctx)
	return nil
}

// SampleReplicaLag queries pg_last_xact_replay_timestamp() on the read replica
// to measure how far behind the primary it is, updates the store's observed
// lag and the sorobeacon_store_replica_lag_seconds gauge, and returns the
// measured duration. When no replica is configured (or when the replica is not
// in recovery and pg_last_xact_replay_timestamp() returns NULL), lag is 0.
func (p *Postgres) SampleReplicaLag(ctx context.Context) (time.Duration, error) {
	if p.replica == nil {
		p.SetObservedReplicaLag(0)
		return 0, nil
	}
	var lagSeconds float64
	err := p.replica.QueryRow(ctx,
		`SELECT COALESCE(EXTRACT(EPOCH FROM (now() - pg_last_xact_replay_timestamp())), 0)`).Scan(&lagSeconds)
	if err != nil {
		return 0, fmt.Errorf("sample replica lag: %w", err)
	}
	if lagSeconds < 0 {
		lagSeconds = 0
	}
	d := time.Duration(lagSeconds * float64(time.Second))
	p.SetObservedReplicaLag(d)
	return d, nil
}

// SetObservedReplicaLag records the observed replication lag on p and updates
// the Prometheus gauge. Exposed so tests and background samplers can set the
// lag deterministically.
func (p *Postgres) SetObservedReplicaLag(d time.Duration) {
	if d < 0 {
		d = 0
	}
	p.replicaLagNanos.Store(int64(d))
	p.metrics.SetReplicaLag(d.Seconds())
}

// ObservedReplicaLag returns the most recently recorded replication lag.
func (p *Postgres) ObservedReplicaLag() time.Duration {
	return time.Duration(p.replicaLagNanos.Load())
}

// MaxReplicaLag returns the lag ceiling beyond which routed reads fall back to
// the primary pool.
func (p *Postgres) MaxReplicaLag() time.Duration {
	if p.maxReplicaLag <= 0 {
		return DefaultMaxReplicaLag
	}
	return p.maxReplicaLag
}

func (p *Postgres) replicaLagExceeded() bool {
	bound := p.MaxReplicaLag()
	return bound > 0 && p.ObservedReplicaLag() > bound
}

// cursorAnchorExistsOnReplica checks whether an alert ID used as a keyset
// pagination cursor exists on the replica in the caller's workspace. When a
// cursor minted against the primary has not yet replicated to the replica,
// resolving the cursor subquery on the replica yields NULL and an empty result
// set; checking anchor existence lets ListAlerts fall back to the primary so
// pagination never skips or truncates rows.
func (p *Postgres) cursorAnchorExistsOnReplica(ctx context.Context, afterID int64) bool {
	if p.replica == nil || afterID <= 0 {
		return true
	}
	var exists bool
	if ws, scoped := tenantWorkspace(ctx); scoped {
		if err := p.replica.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM alerts WHERE id = $1 AND workspace_id = $2)`,
			afterID, ws).Scan(&exists); err != nil {
			return false
		}
		return exists
	}
	if err := p.replica.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM alerts WHERE id = $1 AND workspace_id IS NOT NULL)`,
		afterID).Scan(&exists); err != nil {
		return false
	}
	return exists
}

// queryRows runs a read-only multi-row query, preferring the replica when one
// is configured and within MaxReplicaLag. A replica that cannot answer at all
// (or whose lag exceeds MaxReplicaLag) is transparently retried on the primary,
// so a replica outage or lag spike costs a fallback metric increment rather
// than a failed or excessively stale request.
func (p *Postgres) queryRows(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if p.replica == nil {
		p.metrics.RecordStoreRead(poolPrimary)
		return p.pool.Query(ctx, sql, args...)
	}
	if p.replicaLagExceeded() {
		p.metrics.RecordStoreRead(poolPrimary)
		p.metrics.RecordReplicaFallback()
		return p.pool.Query(ctx, sql, args...)
	}
	rows, err := p.replica.Query(ctx, sql, args...)
	if err == nil {
		p.metrics.RecordStoreRead(poolReplica)
		return rows, nil
	}
	if !replicaUnavailable(err) {
		return nil, err
	}
	p.metrics.RecordStoreRead(poolPrimary)
	p.metrics.RecordReplicaFallback()
	return p.pool.Query(ctx, sql, args...)
}

// queryRowFallback runs a single-row read the same way queryRows runs a
// multi-row one, taking the scan as a callback. The fallback has to happen at
// the scan, not at the call: pgx defers a QueryRow's error until the row is
// read, so a helper that returned a pgx.Row would see a success at the point
// where the replica decision has to be made and could never retry.
func (p *Postgres) queryRowFallback(ctx context.Context, scan func(pgx.Row) error, sql string, args ...any) error {
	if p.replica == nil {
		p.metrics.RecordStoreRead(poolPrimary)
		return scan(p.pool.QueryRow(ctx, sql, args...))
	}
	if p.replicaLagExceeded() {
		p.metrics.RecordStoreRead(poolPrimary)
		p.metrics.RecordReplicaFallback()
		return scan(p.pool.QueryRow(ctx, sql, args...))
	}
	err := scan(p.replica.QueryRow(ctx, sql, args...))
	if err == nil {
		p.metrics.RecordStoreRead(poolReplica)
		return nil
	}
	if !replicaUnavailable(err) {
		return err
	}
	p.metrics.RecordStoreRead(poolPrimary)
	p.metrics.RecordReplicaFallback()
	return scan(p.pool.QueryRow(ctx, sql, args...))
}

// replicaUnavailable reports whether err means the replica could not answer,
// as opposed to answering with an error. Only the former is worth replaying on
// the primary:
//
//   - context.Canceled / context.DeadlineExceeded is the caller's own budget
//     being spent, and the primary shares it. A replay would fail the same way
//     after one wasted round trip, and retrying a canceled request is wrong
//     regardless.
//   - a *pgconn.PgError is the replica's answer to the query, so the replica is
//     up and the statement (or its permissions, or the schema) is at fault. The
//     primary would return the same error, and falling back would hide a real
//     bug — a migration that has not been applied to the replica, say — behind
//     a silent success path that only some deployments exercise.
//
// Everything else is the replica being unreachable — a refused connection, a
// TLS failure, a connection dropped mid-query — which the primary need not
// share, so the read is retried there.
func replicaUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var pgErr *pgconn.PgError
	return !errors.As(err, &pgErr)
}
