# Architecture

One Go process, three pipeline stages, one database for state. The database is
selected by the `DATABASE_URL` scheme: Postgres (pgx) for a team deployment, or
a single SQLite file for a node that should not run a database server.

```
EventSource ──page──▶ poller ─▶ rules engine ─▶ alerts ─▶ dispatcher ─▶ channels
 (RPC or SoroTrail)        │                              │                   │
                           └── ingest_state, rule_absence_state ─ Postgres or SQLite ─┴── delivery_attempts ─┘
```

Only the instance holding the poller lease (`internal/lease`) runs the ingest
loop, while every instance serves the API and dashboard.

The two backends share one `store.Store` interface, one behavioural
conformance suite (`internal/store/conformance_test.go`), and one
channel-config encryption envelope. They differ in DDL (parallel migration
sets) and in how the alert cooldown serialises: Postgres locks the rule row
with `SELECT ... FOR UPDATE`, SQLite holds its single write lock through a
`BEGIN IMMEDIATE` transaction. Both yield one alert per window.

## Event sources (`internal/poller`, `internal/sorotrail`)

The poller knows only the `EventSource` interface — `LatestLedger` plus a
stateless, cursor-paged `FetchEvents` returning already-decoded events —
so the ingest loop is identical regardless of backend:

* **`rpc` (default)** — polls a Stellar RPC node's `getEvents` and decodes
  locally. The RPC's caps (5 filters × 5 contract IDs per filter) are
  absorbed by the source, which encodes batch position in its opaque
  cursors.
* **`sorotrail` (upstream)** — reads a [SoroTrail](https://github.com/sorotrail/SoroTrail)
  indexer's `/api/v1/events`. SoroTrail stores events durably past the
  RPC's ~1-7 day retention window, so monitoring covers history the RPC
  has already dropped; events arrive already decoded; the indexer's
  cursor passes through untouched.

Contributors: a new backend is an implementation of the interface plus one
line in `cmd/sorobeacon`'s mode switch. Nothing in the poller changes.

### Upstream mode (`sorotrail`) guarantees and trade-offs

Running with `SOURCE_MODE=sorotrail` delegates chain ingestion and XDR decoding
to an upstream SoroTrail indexer while keeping rule evaluation, deduplication,
cooldowns, and alert delivery inside SoroBeacon:

* **Startup & network verification** — SoroBeacon verifies `/api/v1/stats` (and
  `/api/v1/health`) against the configured `NETWORK` / `NETWORK_PASSPHRASE` at
  startup and on the first poll, refusing to evaluate events from a wrong-chain
  or non-SoroTrail HTTP endpoint. If SoroTrail is temporarily unreachable at
  boot, SoroBeacon logs a warning, backs off, and verifies identity on the
  first reachable cycle.
* **End-to-end lag & readiness** — SoroBeacon tracks both its own poll lag
  behind SoroTrail (`sorobeacon_poll_lag_ledgers`) and SoroTrail's lag behind
  the Stellar chain head (`sorobeacon_upstream_lag_ledgers`, plus
  `sorobeacon_upstream_consecutive_failures`). Readiness (`/api/v1/readyz`) and
  the external heartbeat check total lag against the chain head, so a stalled
  upstream indexer cannot masquerade as a caught-up SoroBeacon.
* **Short pages & cursor recovery** — On short or empty final pages, SoroBeacon
  advances its checkpoint to SoroTrail's ingested tip (`last_ingested_ledger` /
  `last_ledger`) rather than re-polling the same range. If an opaque pagination
  cursor expires or is rejected across an upstream restart, SoroBeacon logs a
  warning and re-anchors from its durable ledger checkpoint (`last_ledger + 1`),
  relying on `(rule_id, event_id)` deduplication to prevent duplicate alerts.
* **Upstream reorgs & retractions** — SoroBeacon does not maintain a duplicate
  ledger-hash window in upstream mode; instead it consumes SoroTrail's
  retraction signals (`retracted` event flags, `retracted_from_ledger`,
  `retracted_event_ids`, and `verified_through_ledger` regressions), marks
  affected alerts as `retracted_at` in its store, stops pending escalations,
  and dispatches a `[RETRACTED]` notice for any alert that had already been
  delivered.

## Ingestion (`internal/poller`)

* Every `POLL_INTERVAL`, the poller collects the contract IDs of all **enabled** monitors and pages its event source from the checkpoint.
* The source's cursors are followed until it reports no more events; then the checkpoint (`ingest_state.last_ledger`) advances to the minimum `latestLedger` the source reported.
* **Cold start** begins at the source's tip (an RPC retains only ~1–7 days of events, so deep backfill is impossible there; an indexer holds everything, but a fresh monitor has no reason to replay the past). **Warm start** resumes at `last_ledger + 1`.
* Source failures back off exponentially, capped at 10× the poll interval.
* After a cycle that **succeeded**, the poller also runs the absence sweep (see below). A failed cycle skips it: "we could not look" is not "nothing happened".
* In `rpc` mode the poller verifies the RPC's network passphrase at startup
  against the configured one and refuses to start on mismatch.

## Leader election (`internal/lease`)

Every instance serves the API and the dashboard; exactly one polls. Instances
compete for a Postgres session-level advisory lock (`pg_try_advisory_lock`,
key `0x534F4245434F4E`), so the election needs no table, no migration and no
coordinator process — and it is Postgres-only, which is why a `sqlite://`
deployment uses `lease.SingleNode`: no election, the instance is the poller.

The holder runs `poller.Run` (and the retention pruner); the losers run nothing
but their HTTP servers. The lock belongs to the session that took it, which is
what makes failover prompt rather than timer-driven: a leader that exits
releases it explicitly, and a leader that dies frees it when its session goes.

`Lease.Run(ctx, job)` runs `job` in its own goroutine for exactly as long as
the lock is held, renewing every three seconds. A renewal that fails — the
session died, the connection was cut, the lock was released elsewhere — cancels
`job` and waits for it to return *before* the lock is given up, so the next
leader never starts polling while this one is mid-cycle. That ordering is the
split-brain guard: the failure it prevents is two instances ingesting the same
events and delivering every alert twice.

## Observability (`internal/metrics`, `internal/reqid`, `internal/buildinfo`)

* `/metrics` — Prometheus: poll outcomes/duration, lag behind the tip,
  seconds since last poll, the scanned→evaluated→matched→alerted funnel,
  deliveries per channel and outcome, HTTP duration by route pattern. The
  full list is in the [metrics reference](metrics.md).
* `/api/v1/livez`, `/api/v1/readyz` — liveness checks nothing (restart
  loops otherwise); readiness checks the database and the event source
  concurrently, bounded per check, with per-dependency detail.
* `/api/v1/version` — version, commit and build date via `-ldflags`.
* Every request carries an `X-Request-ID`; error bodies and log lines echo
  it, so a reported error maps to one request in the logs.

## Decoding (`internal/stellar`)

The RPC client requests `xdrFormat: "json"` so topics and values arrive readable; against older nodes that reject the parameter it falls back—once, then remembered—to base64 XDR, decoded locally via the Stellar SDK. Either path normalizes into one small value vocabulary rules can rely on:

`nil`, `bool`, `string` (symbols, strings, addresses), `*big.Int` (every integer type up to `i256`), `[]byte`, `[]any`, `map[string]any`

## Matching & alerts (`internal/rules`, `internal/store`)

Each event is checked against every enabled rule of every monitor watching its contract. A match inserts an alert with `ON CONFLICT (rule_id, event_id) DO NOTHING` — the database-level dedup guard that makes ingestion idempotent across restarts and replays.

Rules come in two shapes, and a type belongs to exactly one. A `RuleEvaluator` (the default) decides whether one arriving event matches. An `AbsenceEvaluator` — currently only [`absence_of_event`](../rules/absence-of-event.md) — watches for an event that never arrives, so a matching event merely re-arms it and the poller's sweep fires it once the configured window has elapsed. The sweep stores each rule's last-seen clock in `rule_absence_state` and event ids derived from the silent window itself, so the same dedup guard collapses repeated sweeps and survives restarts.

## Delivery (`internal/notify`)

The dispatcher fans each new alert out to the monitor's enabled channels. Per channel: up to 3 attempts with exponential backoff (1s → 2s → 4s), and **every attempt** — success or failure, with a response snippet — is recorded in `delivery_attempts`. One misbehaving channel never blocks the others or the poller.

## Data model

| Table | Purpose |
| --- | --- |
| `monitors` | Name, contract IDs (jsonb), enabled flag |
| `rules` | Type + params (jsonb) per monitor |
| `channels` | Type + config (jsonb, holds secrets; encrypted at rest when `CONFIG_ENCRYPTION_KEY` is set), enabled flag |
| `monitor_channels` | Which channels a monitor alerts to |
| `alerts` | One row per rule match; unique on `(rule_id, event_id)` |
| `delivery_attempts` | Every delivery try with status and response snippet |
| `ingest_state` | Single-row poller checkpoint (last ledger, cursor) |
| `rule_absence_state` | Last-seen clock per absence rule, one row per `(rule_id, event_name)` |

Migrations are embedded in the binary and applied automatically at startup
(golang-migrate). Postgres and SQLite each have their own embedded set —
`internal/store/migrations/` and `internal/store/migrations/sqlite/` — because
Postgres DDL (JSONB, TIMESTAMPTZ, `generate_series`) does not run unmodified on
SQLite.

## Trust boundaries

* Everything behind `HTTP_ADDR` (API + dashboard) requires `API_TOKEN` when it is set: the API takes `Authorization: Bearer <token>`, the dashboard takes the same token at `/login` and then a session cookie. With `API_TOKEN` unset the whole listener is **unauthenticated** (one warning at startup) so the quickstart keeps working. The probes (`/health`, `/livez`, `/readyz`) are exempt either way.
* `/metrics` is served at the root of the same listener and is **not** behind that authentication — keep it off the public internet or add a proxy rule.
* Authentication is not uniform across listeners: scoped tokens work only on REST, the gRPC server accepts static tokens only and reads the default workspace, and `/graphql` is mounted outside the API's auth middleware. The [Authorization reference](authorization.md) enumerates each surface and its limits.
* Channel secrets are redacted from logs, API responses, error messages, and delivery snippets. When `CONFIG_ENCRYPTION_KEY` is set they are also encrypted at rest, so a database or backup compromise yields ciphertext rather than credentials; when it is unset they are stored as plaintext.
