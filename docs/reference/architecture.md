# Architecture

One Go process, three pipeline stages, Postgres for state:

```
Stellar RPC ──getEvents──▶ poller ─▶ decoder ─▶ rules engine ─▶ alerts ─▶ dispatcher ─▶ channels
                              │                                    │                       │
                              └── ingest_state ──── Postgres ──────┴── delivery_attempts ──┘
```

## Ingestion (`internal/poller`)

* Every `POLL_INTERVAL`, the poller collects the contract IDs of all **enabled** monitors and calls `getEvents` on the RPC.
* The RPC caps requests at **5 filters × 5 contract IDs per filter**, so contracts are batched across filters and, past 25, across multiple requests.
* Pagination cursors are followed until a page comes back short; then the checkpoint (`ingest_state.last_ledger`) advances to the node's `latestLedger`.
* **Cold start** begins at the current tip (the RPC retains only ~1–7 days of events, so deep backfill is impossible). **Warm start** resumes at `last_ledger + 1`.
* RPC failures back off exponentially, capped at 10× the poll interval.

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
