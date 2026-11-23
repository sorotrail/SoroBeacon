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

## Read replicas

Postgres is the bottleneck for the dashboard's read traffic: the alert search,
the monitor list, the headline stats and the alert-counts-by-day chart all hit
the same pool the poller uses to write alerts. `REPLICA_DATABASE_URL` gives
those reads a second pool, pointed at a streaming replica:

```sh
REPLICA_DATABASE_URL=postgres://sorobeacon:…@replica-host:5432/sorobeacon
```

Four reads are routed, all of them dashboard-facing and none of them
read-after-write:

| Query | Where it appears |
| --- | --- |
| `ListMonitorsPage` | the monitor list / its search |
| `ListAlerts` | the alert search in the API and the dashboard |
| `GetStats` | the headline counters |
| `AlertCountsByDay` | the alert chart's daily totals |

Everything else stays on the primary, deliberately. The reads that back the
store's own writes — a monitor or channel read back right after it was
created, a just-delivered alert and its delivery attempts — would show the
caller its own edit missing. The poller's bookkeeping (`GetIngestState`,
`LedgerHashes`) is worse than a display problem: a stale row there can make
the poller re-scan or skip ledgers. `ExpiredAlerts` is read by the pruner to
decide what to archive and delete, and those two decisions have to agree on the
same row set.

What to expect:

- **Off by default.** Unset, Postgres holds one pool and every query goes to
  it — the behaviour of every deployment that predates this setting.
- **A replica that is down is not an outage.** Once the process is up, a
  replica that stops answering is detected per query and the read is replayed
  once on the primary. The cost is one extra round trip, not a failed request.
  Only a connection-level failure counts: a query the replica *answered* with
  an error (a missing relation, a permission, a statement timeout) is reported
  rather than replayed, because the primary would return the same error and the
  fallback would hide it.
- **A replica that cannot be reached at boot is a startup error.** An operator
  who set the variable believes reads are being routed; booting into
  everything-on-the-primary while every log line claims otherwise is the worse
  failure.
- **Readiness follows the primary only.** `/readyz` stays green while the
  replica is down, because the instance can still serve traffic through the
  fallback. Watch the replica with the metrics below instead of with readiness.
- **Bounded staleness and cursor safety.** Replication lag is sampled from the
  replica via `pg_last_xact_replay_timestamp()`. When observed lag exceeds the
  configured bound (`DefaultMaxReplicaLag` = `30s`), routed reads automatically
  fall back to the primary pool until the replica catches up. For keyset
  pagination on `ListAlerts`, if a cursor ID minted against the primary has not
  yet replicated to the replica, `ListAlerts` detects the missing cursor anchor
  and replays the page against the primary so pagination never skips or
  truncates rows.
- **The replica must be at least as new as the primary's schema.** Migrations
  run against `DATABASE_URL`; a replica created from a base backup taken before
  a migration will answer routed queries with `column does not exist`, which is
  reported rather than silently falling back.

Metrics on `/metrics`:

- `sorobeacon_store_reads_total{pool="primary|replica"}` — where routed reads
  actually landed. If the `replica` series stays flat, routing is not happening.
- `sorobeacon_store_replica_fallbacks_total` — reads that had to be replayed on
  the primary (because the replica was unreachable, exceeded the `30s` lag
  bound, or lacked a newly minted pagination cursor anchor). A steady rate means
  the replica is unreachable or too far behind to be worth routing to.
- `sorobeacon_store_replica_enabled` — `1` when a replica is configured, `0`
  when everything reads from the primary.
- `sorobeacon_store_replica_lag_seconds` — observed replication lag in seconds
  on the read replica (`now() - pg_last_xact_replay_timestamp()`); `0` when no
  replica is configured.

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

## Evaluation capacity model

To size a SoroBeacon deployment accurately, operators need both the per-rule-type
CPU cost and the end-to-end throughput of the poller across its pipeline stages.
All measurements below were collected against a deterministic mock `EventSource`
and in-memory store harness on a **4-vCPU AMD EPYC 7543P (32-Core Host, Linux
`amd64`, Go 1.26)** profile so RPC network jitter is excluded and every figure is
reproducible locally.

### Per-rule-type evaluation cost

Every rule type registered in `rules.NewRegistry()` is benchmarked separately on
matching and non-matching decoded events (`BenchmarkRuleTypeCapacity` in
`internal/rules/capacity_bench_test.go`). The non-matching path is the dominant
hot path in production because most `(rule, event)` pairs do not fire an alert.

| Rule type | Match latency (`ns/op`) | No-match latency (`ns/op`) | Throughput (no-match `evals/s`) | Heap allocations (`B/op`, `allocs/op`) | Cost tier & driver |
|---|---:|---:|---:|---:|---|
| `contract_allowlist` | 230 | 239 | ~4,180,000 | 144 B / 2 allocs | **Fast** — caches compiled allowlist set on pointer receiver |
| `address_watchlist` | 242 | 295 | ~3,390,000 | 176 B / 2 allocs | **Fast** — caches normalized address set on pointer receiver |
| `topic_regex` | 348 | 182 | ~5,490,000 | 112 B / 2 allocs | **Fast** — caches compiled RE2 `*regexp.Regexp` on pointer receiver |
| `event_emitted` | 709 | 574 | ~1,740,000 | 88 B / 2 allocs | **Medium** — single JSON param decode + topic comparison |
| `absence_of_event` | 765 | 681 | ~1,470,000 | 32 B / 1 alloc | **Medium** — `Spec` duration parse + topic name check |
| `self_transfer` | 1,158 | 726 | ~1,380,000 | 80–192 B / 2–7 allocs | **Medium** — fast exit when `from != to`; `big.Int` compare on match |
| `frequency_threshold` | 1,268 | 944 | ~1,060,000 | 112–136 B / 2–3 allocs | **Medium (stateful)** — mutex-guarded rolling-window slice prune |
| `baseline_deviation` | 2,410 | 2,180 | ~458,000 | 320 B / 6 allocs | **Medium (stateful)** — bounded median/MAD + Welford window per bucket |
| `token_supply_change` | 1,250 | 1,123 | ~890,000 | 136 B / 4 allocs | **Medium** — `json.NewDecoder` + `big.Int` amount threshold |
| `time_window` | 1,241 | 1,154 | ~866,000 | 144 B / 3 allocs | **Medium** — `HH:MM` UTC clock parsing + weekday check |
| `event_name_glob` | 1,161 | 1,760 | ~568,000 | 136 B / 4 allocs | **Medium** — `path.Match` across pattern list |
| `topic_position` | 1,540 | 1,435 | ~697,000 | 112 B / 2 allocs | **Medium** — `json.NewDecoder` (`DisallowUnknownFields`) per call |
| `token_event` | 2,187 | 1,199 | ~834,000 | 144–256 B / 2–7 allocs | **Medium** — SEP-41 slot extraction + `i128` `big.Int` parsing |
| `value_threshold` | 5,003 | 5,155 | ~194,000 | 960 B / 18 allocs | **Heavy** — runs `Validate` + `json.Unmarshal` + `big.Float`/`big.Int` |
| `numeric_range` | 6,339 | 5,380 | ~186,000 | 1,618 B / 27 allocs | **Heavy** — runs `Validate` + two `big.Int` bound parses per call |
| `composite` | 9,413 | 7,068 | ~141,000 | 1,384 B / 26 allocs | **Heavy** — recursive child unmarshalling + child rule evaluation |

**Key observation:** Rule types do not have uniform cost. A `composite` or
`numeric_range` rule is **25×–38× more expensive** per event than a cached
`topic_regex` or `contract_allowlist` rule because uncached evaluators re-parse
their `params` JSON (`json.NewDecoder` with `DisallowUnknownFields`) on every
event.

### Pipeline stage breakdown and bottleneck analysis

`BenchmarkPollerCapacityStages` (`internal/poller/capacity_test.go`) decomposes
a fixed batch of **100 events evaluated against 10 monitors × 5 `value_threshold`
rules (5,000 evaluations per batch)** into four cumulative stages:

| Stage | Latency per 100-event batch | Event throughput (`events/s`) | Evaluation throughput (`evals/s`) | Memory (`B/op`, `allocs/op`) |
|---|---:|---:|---:|---:|
| **Stage 1:** RPC fetch + XDR/JSON decode (`RPCSource.FetchEvents`) | 0.54 ms | 185,178 | — | 121 KB / 2,018 allocs |
| **Stage 2:** Pure rule evaluation (`Registry.Evaluate` × 50 rules) | 14.28 ms | 7,005 | 350,242 | 4.97 MB / 90,010 allocs |
| **Stage 3:** `Ingestor.Handle` at **0% match rate** (`ListRules` + `Evaluate`) | 19.63 ms | 5,095 | 254,758 | 6.23 MB / 94,012 allocs |
| **Stage 4a:** `Ingestor.Handle` at **10% match rate** (500 alerts written & dispatched) | 25.75 ms | 3,883 | 194,155 | 7.97 MB / 115,926 allocs |
| **Stage 4b:** `Ingestor.Handle` at **100% match rate** (5,000 alerts written & dispatched) | 113.29 ms | 883 | 44,136 | 29.64 MB / 314,079 allocs |

**Bottleneck identification:**

1. **At low match rates (< 5%, normal steady state):**
   - **In-process CPU bottleneck:** Per-evaluation JSON parameter unmarshalling
     inside heavy rule types (`value_threshold`, `numeric_range`, `composite`)
     accounts for **73%** of `Ingestor.Handle` CPU time (14.28 ms of 19.63 ms).
   - **Database round-trip bottleneck (`ListRules` per event):** `Ingestor.Handle`
     currently calls `store.ListRules(ctx, m.ID, true)` inside the per-event
     loop (`O(events × monitors)` queries per poll cycle). Against an in-memory
     store this adds 5.35 ms per 100 events (1,000 calls), but against Postgres
     over a 0.5 ms local network RTT, `100 events × 10 monitors = 1,000`
     `ListRules` queries add **~500 ms** of serial query latency per poll cycle.
2. **At high match rates (>= 10%, alert storms or noisy rules):**
   - **Alert persistence (`store.CreateAlert`) and payload serialization:**
     Each matching `(rule, event)` pair marshals the event payload (`json.Marshal`)
     and executes a transactional deduplication + cooldown check (`SELECT ... FOR UPDATE`
     on Postgres, or `BEGIN IMMEDIATE` single-writer lock on SQLite). Moving from
     0% to 100% match rate increases cycle time **5.8×** (from 19.63 ms to
     113.29 ms in memory, and bounded by ~500–2,000 tx/s on disk-backed SQLite/Postgres).
     Rule cooldowns (`cooldown` parameter) do **not** bypass `CreateAlert` — they
     are enforced inside `CreateAlert` to increment `suppressed_since_last`.

### Scaling across monitors, rules, and events

`BenchmarkPollerScalingDimensions` measures full `Poller.Poll` cycles (at a 4%
rule match rate) across `(monitors M, rules/monitor R, events/cycle E)`:

| Scenario (`M` monitors × `R` rules × `E` events) | Total evaluations / cycle | Cycle duration | Event throughput (`events/s`) | Evaluation throughput (`evals/s`) |
|---|---:|---:|---:|---:|
| `m1_r5_e50` (1 × 5 × 50) | 250 | 2.15 ms | 23,215 | 116,070 |
| `m10_r5_e50` (10 × 5 × 50) | 2,500 | 15.85 ms | 3,155 | 157,758 |
| `m50_r5_e50` (50 × 5 × 50) | 12,500 | 81.85 ms | 611 | 152,711 |
| `m10_r20_e50` (10 × 20 × 50) | 10,000 | 60.68 ms | 824 | 164,803 |
| `m10_r5_e200` (10 × 5 × 200) | 10,000 | 70.39 ms | 2,841 | 142,065 |

**Operator capacity formula:**

 Evaluation work per cycle scales linearly with the product $E \times M \times R$:

$$\text{Cycle Time (ms)} \approx E \times M \times \left( T_{\text{list\_rules}} + R \times T_{\text{eval}} + R \times p_{\text{match}} \times T_{\text{alert\_write}} \right)$$

Where:
- $T_{\text{eval}} \approx 0.001\text{ ms}$ for fast/medium rules and $0.005\text{–}0.009\text{ ms}$ for heavy rules (`value_threshold`, `numeric_range`, `composite`).
- $T_{\text{list\_rules}} \approx 0.005\text{ ms}$ (in-memory/SQLite page cache) to $0.3\text{–}0.5\text{ ms}$ (Postgres network RTT).
- $T_{\text{alert\_write}} \approx 1\text{–}3\text{ ms}$ per matched alert on disk-backed Postgres/SQLite.

To keep `poll_duration` below 50% of the default `POLL_INTERVAL=5s` (a **2.5 s**
budget per cycle) at a typical Stellar rate of **25 contract events per 5s ledger**
and a **1% alert match rate**:
- **Single-node SQLite (`DATABASE_URL=sqlite://...`):** Comfortably sustains up
  to **~50 monitors × 10 rules (500 active rules)** (~12,500 evaluations per 5s
  cycle, ~80 ms CPU + ~125 alert writes/s peak).
- **Postgres (`DATABASE_URL=postgres://...`):** Sustains **~40–60 monitors × 10
  rules (400–600 active rules)** before per-event `ListRules` round-trips
  consume the 2.5 s cycle budget.

### Steady-state vs catch-up throughput

When SoroBeacon restarts after downtime or recovers from an RPC outage, the
poller enters catch-up mode, draining historical ledgers in 100-event paginated
batches (`stellar.DefaultEventsLimit = 100`). `BenchmarkPollerSteadyStateVsCatchUp`
compares both regimes on 10 monitors × 5 rules (50 rules total):

| Mode | Workload per `Poll` cycle | Cycle duration | Event throughput (`events/s`) | Evaluation throughput (`evals/s`) | Catch-up rate |
|---|---|---:|---:|---:|---|
| **Steady state** | 1 ledger, 20 events (1 RPC page, 1,000 evals) | 6.79 ms | 2,947 | 147,344 | Real-time (`0.14%` of 5s budget) |
| **Catch-up** | 50 ledgers, 500 events (5 RPC pages, 25,000 evals) | 165.65 ms | 3,018 | 150,919 | **~300 ledgers/s** (cpu-bound) or **~25–50 ledgers/s** (with 50 ms RPC page RTT) |

Because `Poller.Poll` drains all paginated `FetchEvents` pages for a batch before
sleeping for `POLL_INTERVAL`, a poller that is **1 hour behind (720 ledgers,
~14,400 events at 20 events/ledger)** requires 144 paginated RPC calls and
~720,000 rule evaluations:
- **Evaluation + local persistence time:** ~4.8 seconds.
- **RPC pagination time (at 50 ms/page):** ~7.2 seconds.
- **Total catch-up time for a 1-hour outage:** **~12 seconds** within a single
  drain cycle.

### Reproducing the measurements

Run the benchmark suite from the repository root using `make bench` (or `go test`
directly):

```sh
# Run both the per-rule-type and end-to-end poller capacity benchmarks
make bench

# Or invoke go test directly with custom benchtime
go test -run=^$ -bench="BenchmarkRuleTypeCapacity|BenchmarkPoller" -benchmem ./internal/rules/... ./internal/poller/...
```

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