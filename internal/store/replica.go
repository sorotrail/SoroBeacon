package store

import (
	"context"
	"errors"
	"fmt"

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
// process. Four reads are routed:
//
//	ListMonitorsPage    the dashboard's monitor list
//	ListAlerts          the dashboard's and the API's alert search
//	AlertCountsByDay    the alert chart's daily totals
//	GetStats            the dashboard's headline counters
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
)

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
	return nil
}

// queryRows runs a read-only multi-row query, preferring the replica. A
// replica that cannot answer at all is transparently retried once on the
// primary, so a replica outage costs latency and a metric line rather than a
// failed request.
func (p *Postgres) queryRows(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if p.replica == nil {
		p.metrics.RecordStoreRead(poolPrimary)
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
