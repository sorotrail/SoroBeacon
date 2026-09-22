# Environment variable reference

All runtime configuration is environment variables. There is no config
file. Copy [`.env.example`](../.env.example) and edit it, or set the
variables in the process environment / systemd `EnvironmentFile=`.

This page is the operator reference for **every variable
`internal/config` actually reads**. Channel secrets (webhook URLs, bot
tokens, SMTP credentials) are **not** environment variables — they live
in each channel's `config` JSON in Postgres and must never be logged,
returned by the API, or pasted into a ticket.

`internal/config/config.go` (plus `ParseNetwork` in `network.go`) is the
authoritative source. If this page and the code disagree, the code wins
and this page should be updated.

## Secrets

| Variable | Secret? | Notes |
| --- | --- | --- |
| `DATABASE_URL` | **yes** | Connection string often embeds a password. Mode `0600` on disk; never commit a filled `.env`. |
| `NETWORK_PASSPHRASE` | no (public nets) | SDF passphrases are public. For `NETWORK=custom` it identifies a private network — treat it as operational config, not a credential. |
| `RPC_URL` / `SOROTRAIL_URL` | maybe | A URL is not a password, but provider URLs sometimes embed tokens in the path or query. Do not commit those. |
| `CORS_ALLOWED_ORIGINS` | no | An allow-list, not a credential. Think hard before allowing a third-party origin: the API is unauthenticated. |
| everything else | no | |

Channel `config` in the database is the place webhook URLs, bot tokens
and SMTP passwords live. Do not copy those into environment variables
or into this file.

## Database

| Variable | Type | Default | Required | What it does |
| --- | --- | --- | --- | --- |
| `DATABASE_URL` | URL string | _(none)_ | **required** | Postgres connection string in pgx form, e.g. `postgres://user:pass@host:5432/sorobeacon?sslmode=disable`. Load fails if it is empty. |

## RPC / event source

These select **where events come from** and **which Stellar network**
the RPC (or indexer) belongs to. At startup SoroBeacon asks the RPC
which network it is on and **refuses to start on a mismatch**.

| Variable | Type | Default | Required | What it does |
| --- | --- | --- | --- | --- |
| `SOURCE_MODE` | enum | `rpc` | optional | `rpc` — poll a Stellar RPC node (standalone). `sorotrail` — read a [SoroTrail](https://github.com/sorotrail/SoroTrail) indexer (upstream). Any other value is a startup error. |
| `SOROTRAIL_URL` | URL string | _(none)_ | **required when `SOURCE_MODE=sorotrail`**; ignored in `rpc` mode | Base URL of the SoroTrail indexer. Load fails if this is empty in sorotrail mode. |
| `NETWORK` | enum | `testnet` | optional | `testnet` \| `mainnet` \| `futurenet` \| `custom`. Selects the preset RPC endpoint and passphrase. |
| `RPC_URL` | URL string | per `NETWORK` (testnet: `https://soroban-testnet.stellar.org`) | required when `NETWORK=custom`; optional override otherwise | Stellar RPC endpoint (JSON-RPC 2.0 over HTTP/HTTPS). Must be an absolute `http` or `https` URL when set. |
| `NETWORK_PASSPHRASE` | string | per `NETWORK` | **required when `NETWORK=custom`**; optional override otherwise | Network passphrase. Always wins over the preset, so a named network with a local quickstart passphrase works. |

`NETWORK=custom` is for private standalone networks: both `RPC_URL` and
`NETWORK_PASSPHRASE` must be set.

## HTTP

| Variable | Type | Default | Required | What it does |
| --- | --- | --- | --- | --- |
| `HTTP_ADDR` | listen address | `:8080` | optional | Bind address for the API and dashboard (and `/metrics`). |
| `CORS_ALLOWED_ORIGINS` | comma-separated origins | empty (CORS disabled) | optional | Browser Origins allowed to call the API cross-origin. Empty disables CORS. The dashboard is same-origin and never needs this. The API is unauthenticated — do not allow untrusted origins. |

## Polling

| Variable | Type | Default | Required | What it does |
| --- | --- | --- | --- | --- |
| `POLL_INTERVAL` | Go duration | `5s` | optional | How often the poller calls `getEvents`. Parsed with `time.ParseDuration`. **Minimum `1s`** — a smaller value is a startup error. |

Only applies to `SOURCE_MODE=rpc` in practice (the standalone poller).
Upstream (`sorotrail`) reads the indexer; this interval is still loaded
but the poller is not the source.

## Logging

| Variable | Type | Default | Required | What it does |
| --- | --- | --- | --- | --- |
| `LOG_LEVEL` | enum | `info` | optional | Minimum `log/slog` level: `debug` \| `info` \| `warn` (or `warning`) \| `error`. Logs are structured JSON on stdout. |

## Not environment variables

| Thing | Where it lives |
| --- | --- |
| Channel webhook URLs, bot tokens, SMTP credentials | Channel `config` JSON in Postgres |
| Prometheus metrics | Always on `/metrics` — no flag |
| Build version / commit | Baked in at compile time; served at `GET /api/v1/version` |
| `TEST_DATABASE_URL` | Test-only (`make test-db`); not read by `config.Load` |

## Drift vs `.env.example`

`.env.example` is the copy-paste template. This reference was written
against `internal/config` on the same commit; `.env.example` already
listed every runtime variable (including `CORS_ALLOWED_ORIGINS`) with
matching defaults, so it was not changed in this PR.
