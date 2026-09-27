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
  disk. Use Postgres for multi-instance deployments.
- **Cooldown semantics are unchanged.** Postgres uses `SELECT ... FOR UPDATE`;
  SQLite enforces the same one-alert-per-window rule with its single writer.
  The shared conformance suite runs both.
- The Postgres pool variables (`DATABASE_MAX_CONNS`, …) are rejected at startup
  with a SQLite URL rather than being silently ignored.

Use Postgres when you need multiple instances, more writers than one, or
managed backups and replication.

## Multi-instance deployment

Running more than one SoroBeacon instance today is **not formally supported**.
Each instance independently:

- Polls the same RPC node
- Stores data into the same Postgres database
- Evaluates rules and dispatches alerts

The database has a dedup guard: alerts are unique on `(rule_id, event_id)`, so
two instances evaluating the same rule against the same event will produce only
one alert. However, there are caveats:

- **Checkpoint racing:** Both instances may advance `ingest_state.last_ledger`
  past the same events, potentially skipping events neither processes.
- **Delivery duplication:** The same alert may be dispatched twice (once per
  instance) if both instances create the alert before the dedup guard triggers.
  The `delivery_attempts` table will record two attempts.

If you need horizontal scaling, the recommended approach is a single instance
with a SoroTrail indexer (`SOURCE_MODE=sorotrail`), which acts as a durable
event queue that multiple SoroBeacon instances can share. This is the design
intended for future multi-instance support.

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
- **Multi-instance coordination:** As noted above, running multiple instances
  against the same database is not actively supported. The dedup guard prevents
  double alerts, but checkpoint consistency is not guaranteed.

If you hit limits that are not covered here, please file an issue so we can
measure and document the behavior.