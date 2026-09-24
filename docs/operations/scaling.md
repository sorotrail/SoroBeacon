# Capacity and scaling characteristics

This page describes what one instance of SoroBeacon can handle and what
happens as the number of monitored contracts grows. The answer depends on how
contracts are grouped into RPC requests — a detail that lives in the code but
was previously undocumented.

## Contract grouping into RPC requests

The Stellar JSON-RPC `getEvents` method has hard limits per request:

- **Maximum 5 filters** per request
- **Maximum 5 contract IDs** per filter

SoroBeacon's poller groups all watched contract IDs into batches that respect
these limits. The function [`groupFilters`][] in
`internal/poller/rpcsource.go` packs contracts sequentially:

- Each group contains at most `MaxFiltersPerRequest` (= 5) filters
- Each filter contains at most `MaxContractIDsPerFilter` (= 5) contract IDs
- Therefore, a single `getEvents` call can handle at most **25 contract IDs**

If you have N monitored contracts, the poller will make `ceil(N / 25)` RPC
requests per poll interval. For example:

| Monitored contracts | RPC calls per poll |
|---|---|
| 1 – 25 | 1 |
| 26 – 50 | 2 |
| 51 – 75 | 3 |
| 100 | 4 |
| 200 | 8 |
| 500 | 20 |

**What this means in practice:** Adding contracts has a step-wise effect on RPC
load. The first 25 cost no more than one; the next 25 add a second request, and
so on. Beyond 25 per poll, every additional 25 contracts increases RPC load by
one request per poll cycle.

## Poll interval trade-offs

The [`POLL_INTERVAL`][] environment variable controls how often the poller calls
`getEvents`. The default is `5s`.

| Poll interval | RPC load per cycle | Approximate RPC load per minute |
|---|---|---|
| `1s` | 12× the load at `5s` | ~12× |
| `5s` (default) | 1× | 1× |
| `30s` | 0.17× | 0.17× |
| `5m` | 0.083× | 0.083× |

Shorter intervals increase RPC load linearly but reduce alert latency — the time
between a contract emitting an event and the alert being dispatched. Longer
intervals reduce RPC load but increase the delay before an alert arrives.

The poller backs off exponentially on RPC failures, capped at 10× the poll
interval, then recovers automatically.

## When ingestion falls behind

The poller tracks two metrics that surface via `/api/v1/stats` and `/metrics`:

- **`lag`** — the difference between the chain's latest ledger and the last
  ledger the poller has processed. A growing lag means the poller can't keep up.
- **`poll_duration`** — how long each poll cycle takes. If this approaches or
  exceeds the poll interval, the next poll will start late, and the lag will
  grow.

### First settings to adjust when falling behind

1. **Increase `POLL_INTERVAL`** — from `5s` to `30s` or `5m` reduces RPC load
   proportionally. This is the easiest knob.
2. **Reduce the number of monitored contracts** — group contracts across
   monitors so no single monitor watches too many. Remember: 25 contracts per
   `getEvents` call is the hard limit.
3. **Check the RPC endpoint** — a slow or throttled RPC will naturally increase
   poll duration. Consider a dedicated self-hosted node or a different provider.

The metric `poll_lag` (exposed on `/metrics`) reports the current lag in ledger
units. If it's steadily increasing, one of the above adjustments is needed.

## SQLite: the single-node backend

`DATABASE_URL=sqlite:///path/to/sorobeacon.db` removes the Postgres server from
the deployment entirely: one file holds the monitors, alerts and checkpoints.
It is meant for exactly the case it sounds like — one instance watching a few
contracts on a small VPS or a Raspberry Pi.

What to expect:

- **Writes serialise.** SQLite permits one writer at a time. The store takes
  the write lock for the whole write transaction (`BEGIN IMMEDIATE` on a single
  connection), so concurrent writers queue instead of failing. Throughput is
  bounded by that one writer; it is not a fit for a high alert volume.
- **Reads stay concurrent.** WAL journalling is enabled, so dashboard and API
  reads are not blocked by an in-flight write.
- **One instance.** Do not point several SoroBeacon processes at the same file,
  and do not put it on a network filesystem — SQLite's locking assumes a local
  disk. Use Postgres for multi-instance deployments. A SQLite deployment also
  has no leader election (advisory locks are Postgres-only): the instance polls
  unconditionally and `/api/v1/health` reports `"leader_election": false`.
- **Cooldown semantics are unchanged.** Postgres uses `SELECT ... FOR UPDATE`;
  SQLite enforces the same one-alert-per-window rule with its single writer.
  The shared conformance suite runs both.
- The Postgres pool variables (`DATABASE_MAX_CONNS`, …) are rejected at startup
  with a SQLite URL rather than being silently ignored.

Use Postgres when you need multiple instances, more writers than one, or
managed backups and replication.

## Multi-instance deployment

Several instances are supported: they all serve the API and the dashboard, and
they elect **one** poller between them. Without that election every instance
polls the same contracts, races the same checkpoint, and dispatches its own
copy of every alert — which is what made the product single-instance until
leader election landed.

Instances compete for a Postgres **session-level advisory lock**
(`pg_try_advisory_lock`, key `0x534F4245434F4E`) in `internal/lease`. There is
no lock table, no migration and no coordinator process, and because advisory
locks are scoped to a database, every instance must use the same
`DATABASE_URL`. The holder runs the ingest loop and the retention pruner; the
others serve HTTP and wait.

How fast leadership moves:

| Event | What releases the lock | Time to a new poller |
|---|---|---|
| Leader exits gracefully (SIGTERM) | It releases the lock explicitly on the way out | ~1 lease interval (3s) |
| Leader is killed (SIGKILL, OOM, node loss) | The lock goes when its database session disappears | ~1 lease interval (3s) |
| Leader loses its connection to Postgres | It detects the dead session on the next renewal, stops polling, and the lock is already free | ~1 lease interval (3s) |
| Leader is partitioned from Postgres while the server still sees the session | Nobody: it stops polling (safe) and a waiting instance cannot take a lock the server still considers held, until Postgres reaps the session on its TCP keepalive timers | up to the `tcp_keepalives_*` timers |

The last row is deliberate. The lease prefers *at most* one poller over *always*
one: a stalled cluster sends no alerts, while a duplicated one sends every
alert twice, and the second failure is the one users notice.

Details worth knowing before you scale:

- **Each instance holds one dedicated connection** for the lease, outside the
  `DATABASE_MAX_CONNS` pool. A session-level lock cannot be pinned on a pooled
  connection, so the lease dials its own and never shares it.
- **A follower is healthy.** `/api/v1/health` reports `leader`,
  `leader_election` and `leader_since`; the overview page shows the role.
  Readiness does not fail for not polling, and the deploy order does not
  matter — whichever instance wins the lock polls.
- **The hand-over is ordered.** A demoted leader cancels the poller and waits
  for the cycle to return before giving the lock up, so the new leader never
  starts while the old one is still mid-cycle.
- **PgBouncer:** the lock needs a session, so point `DATABASE_URL` at a direct
  connection or use `pool_mode=session`. A transaction-pooled PgBouncer cannot
  hold a session-level advisory lock, and no instance would poll.
- **Throughput does not scale with replicas.** The lease elects one poller, so
  adding instances buys availability, not ingestion. For more headroom use a
  SoroTrail indexer (`SOURCE_MODE=sorotrail`), which stores events durably past
  the RPC's window and serves them to whichever instance holds the lease.

The dedup guard (`alerts` unique on `(rule_id, event_id)`) and the cooldown's
`SELECT ... FOR UPDATE` still apply across instances, but with the lease they
are no longer load-bearing: only one instance polls at a time.

## Database growth: alerts and delivery attempts

The database (Postgres or SQLite) stores several tables that grow over time:

| Table | What it stores | Growth rate |
|---|---|---|
| `alerts` | One row per rule match; unique on `(rule_id, event_id)` | Proportional to the number of matching events × enabled rules |
| `delivery_attempts` | Every delivery try (success or failure) with response snippet | One row per alert × up to 3 attempts per channel |
| `ingest_state` | Single-row poller checkpoint | Constant (one row) |
| `monitors`, `rules`, `channels` | Configuration | Set by operator; grows only when adding/removing monitors/channels/ rules |

### The retention setting

`ALERT_RETENTION` bounds the size of the `alerts` and `delivery_attempts` tables.

- **Unset (default):** Nothing is pruned. Tables grow indefinitely.
- **Set to a duration** (e.g. `90d`, `24h`): Alerts older than the duration are
  deleted in batches of 1000. Because `delivery_attempts` has `ON DELETE CASCADE`
  on the foreign key to `alerts`, old delivery attempts are automatically cleaned
  up when their associated alerts are pruned.

**Example:** With `ALERT_RETENTION=90d` and a moderately active deployment
(~10 alerts/day), the `alerts` table would grow by ~900 rows per day, or ~328,500
per year. After 90 days, the pruner would begin deleting rows, keeping the table
roughly at a 90-day window.

### Monitoring database size

The `/api/v1/stats` endpoint reports `alerts_count` and `alerts_last_24h`. The
`/metrics` endpoint exposes `alerts_total` and `delivery_attempts_total` as
Gauge/Histogram metrics. Use these to observe growth patterns and choose an
appropriate `ALERT_RETENTION` value.

## Honest unknowns

- **Exact RPC QPS at scale:** We have not measured requests-per-second at 500+
  monitored contracts. The grouping logic is deterministic, but real-world RPC
  latency varies by provider.
- **Pruner batch timing:** The `DefaultPruneInterval` and `DefaultPruneBatch`
  constants in the store package control how often and how many rows are deleted
  per retention cycle. These are set to reasonable defaults but have not been
  tuned for very large databases.
- **Multi-instance coordination:** Several instances now elect a single poller
  and are supported (see above). What is not measured is behaviour during a
  sustained Postgres partition, where the lease deliberately stops polling
  rather than risk two pollers: the recovery time is whatever the server's TCP
  keepalive configuration takes to reap the stale session.

If you hit limits that are not covered here, please file an issue so we can
measure and document the behavior.