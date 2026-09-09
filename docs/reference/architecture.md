# Architecture

One Go process, three pipeline stages, Postgres for state:

```
EventSource ──page──▶ poller ─▶ rules engine ─▶ alerts ─▶ dispatcher ─▶ channels
 (RPC or SoroTrail)        │                              │                   │
                           └── ingest_state ── Postgres ──┴── delivery_attempts ─┘
```

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

## Ingestion (`internal/poller`)

* Every `POLL_INTERVAL`, the poller collects the contract IDs of all **enabled** monitors and pages its event source from the checkpoint.
* The source's cursors are followed until it reports no more events; then the checkpoint (`ingest_state.last_ledger`) advances to the minimum `latestLedger` the source reported.
* **Cold start** begins at the source's tip (an RPC retains only ~1–7 days of events, so deep backfill is impossible there; an indexer holds everything, but a fresh monitor has no reason to replay the past). **Warm start** resumes at `last_ledger + 1`.
* Source failures back off exponentially, capped at 10× the poll interval.
* In `rpc` mode the poller verifies the RPC's network passphrase at startup
  against the configured one and refuses to start on mismatch.

## Observability (`internal/metrics`, `internal/reqid`, `internal/buildinfo`)

* `/metrics` — Prometheus: poll outcomes/duration, lag behind the tip,
  seconds since last poll, the scanned→matched→alerted funnel, deliveries
  per channel and outcome, HTTP duration by route pattern.
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

## Delivery (`internal/notify`)

The dispatcher fans each new alert out to the monitor's enabled channels. Per channel: up to 3 attempts with exponential backoff (1s → 2s → 4s), and **every attempt** — success or failure, with a response snippet — is recorded in `delivery_attempts`. One misbehaving channel never blocks the others or the poller.

## Data model

| Table | Purpose |
| --- | --- |
| `monitors` | Name, contract IDs (jsonb), enabled flag |
| `rules` | Type + params (jsonb) per monitor |
| `channels` | Type + config (jsonb, holds secrets), enabled flag |
| `monitor_channels` | Which channels a monitor alerts to |
| `alerts` | One row per rule match; unique on `(rule_id, event_id)` |
| `delivery_attempts` | Every delivery try with status and response snippet |
| `ingest_state` | Single-row poller checkpoint (last ledger, cursor) |

Migrations are embedded in the binary and applied automatically at startup (golang-migrate).

## Trust boundaries

* Everything behind `HTTP_ADDR` (API + dashboard) is **unauthenticated** in the MVP.
* Channel secrets are redacted from logs, API responses, error messages, and delivery snippets — but stored **unencrypted** in Postgres (encryption at rest is a designed-for contributor issue).
