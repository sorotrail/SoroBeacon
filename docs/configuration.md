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
| `CONFIG_ENCRYPTION_KEY` | **yes** | Base64 AES-GCM key that encrypts channel `config` at rest. Losing it makes encrypted configs unrecoverable — back it up with the database. |
| `API_TOKEN` | **yes** | Bearer token(s) for `/api/v1` and the dashboard sign-in. Anyone holding one can read and mutate everything, so treat it like a password: mode `0600` on disk, a secret manager in production, and never in a log line, ticket or shell history. |
| `NETWORK_PASSPHRASE` | no (public nets) | SDF passphrases are public. For `NETWORK=custom` it identifies a private network — treat it as operational config, not a credential. |
| `RPC_URL` / `SOROTRAIL_URL` | maybe | A URL is not a password, but provider URLs sometimes embed tokens in the path or query. Do not commit those. |
| `CORS_ALLOWED_ORIGINS` | no | An allow-list, not a credential. Think hard before allowing a third-party origin: whatever credential that origin's users hold can act through their browser. |
| everything else | no | |

Channel `config` in the database is the place webhook URLs, bot tokens
and SMTP passwords live. Do not copy those into environment variables
or into this file. `CONFIG_ENCRYPTION_KEY` is the key that encrypts that
column; see [Channel config encryption](#channel-config-encryption).

## Database

| Variable | Type | Default | Required | What it does |
| --- | --- | --- | --- | --- |
| `DATABASE_URL` | URL string | _(none)_ | **required** | Connection string whose scheme selects the backend. `postgres` / `postgresql` → a Postgres server (pgx pool), e.g. `postgres://user:pass@host:5432/sorobeacon?sslmode=disable`. `sqlite` → a single database file, e.g. `sqlite:///var/lib/sorobeacon/sorobeacon.db`, with no server to run. Load fails if it is empty; a Postgres URL must carry a host and a SQLite URL a file path. Errors never echo a password. |

### SQLite backend

`sqlite://<path>` stores everything in one file and needs no Postgres. The
parent directory is created if it is missing, and the database runs in WAL
mode. It is aimed at a single instance — one contract on a small VPS or a
Raspberry Pi.

**Writes serialise.** SQLite allows one writer at a time, so the store holds
the write lock for the duration of a write transaction (it uses `BEGIN
IMMEDIATE` and a single connection). The alert cooldown, which Postgres
enforces with `SELECT ... FOR UPDATE`, is enforced the same way and with the
same result — one alert per window — but write throughput is bounded by that
one writer. Reads run concurrently under WAL. Do not point several SoroBeacon
instances at one SQLite file; use Postgres for that.

The `DATABASE_MAX_CONNS`, `DATABASE_MIN_CONNS`,
`DATABASE_MAX_CONN_LIFETIME` and `DATABASE_MAX_CONN_IDLE_TIME` variables tune
the **Postgres** pool. Setting any of them with a `sqlite` URL is a startup
error rather than a setting that silently does nothing.

## API authentication

| Variable | Type | Default | Required | What it does |
| --- | --- | --- | --- | --- |
| `API_TOKEN` | comma-separated string | empty (authentication off) | optional | Static bearer token(s). Each value is accepted on every `/api/v1` route as `Authorization: Bearer <token>`, and any one of them signs in to the dashboard at `/login`. A list rather than a single value so a token can be rotated without downtime: add the new one, roll clients over, remove the old one. Values are trimmed; a token cannot contain a comma. |

Generate one with `openssl rand -hex 32`. The token is a credential: it is never
logged, never returned in an error body, and the access log records the matched
route pattern rather than the raw URL, so a token smuggled into a query string
is not written out either. Only the number of configured tokens appears in the
startup log line (`api_token_count`).

**Unset ⇒ open, with a warning.** With no `API_TOKEN`, `/api/v1` and the
dashboard behave exactly as they did before authentication existed, and the
process logs one warning at startup. That is deliberate: an upgrade, or the
docker-compose quickstart, must never lock the operator out. A value that is
set but yields no token (`,`, whitespace) is an error instead, because the
operator plainly meant to require one.

**Probes are exempt.** `GET /health`, `GET /livez` and `GET /readyz` need no
token, so an authenticated deployment cannot fail its own health checks. Two
consequences worth knowing: `/readyz` reports per-dependency detail (including
dependency error strings) to anyone who can reach the port, and `/metrics` on
the same listener is not authenticated at all. Keep both off the public
internet.

```sh
# one token
export API_TOKEN=$(openssl rand -hex 32)
curl -s localhost:8080/api/v1/monitors -H "Authorization: Bearer $API_TOKEN"

# rotation: both tokens work during the hand-over
API_TOKEN="$OLD_TOKEN,$NEW_TOKEN"
```

### The dashboard

The dashboard has no user accounts. `GET /login` asks for the token and, on
success, sets an HttpOnly, `SameSite=Lax` session cookie (12 hours, in memory
only — a restart signs everyone out). `SameSite=Lax` matters: it is why the
dashboard's state-changing forms cannot be forged from another origin. The
cookie's `Secure` flag follows the request, so it is set when SoroBeacon
terminates TLS itself and absent on a plain-HTTP deployment.

## Channel config encryption

| Variable | Type | Default | Required | What it does |
| --- | --- | --- | --- | --- |
| `CONFIG_ENCRYPTION_KEY` | base64 string | empty (encryption disabled) | optional | AES-GCM key used to encrypt each channel's `config` at rest. Must decode to 16, 24 or 32 bytes (32, i.e. AES-256, recommended); validated at startup so a bad value fails boot, not the first write. Generate with `openssl rand -base64 32`. Unset stores config as plaintext and logs one startup warning. |

When set, new and updated channel rows hold a JSON envelope
(`{"sorobeacon_config":"v1:…"}`). Rows written before the key was
set stay plaintext, keep working, and are re-encrypted lazily on their next
write. Losing the key makes encrypted rows undecryptable: reads fail with an
error naming the channel and never echo ciphertext or key material. Back the
key up alongside your database backups.

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
| `CORS_ALLOWED_ORIGINS` | comma-separated origins | empty (CORS disabled) | optional | Browser Origins allowed to call the API cross-origin. Empty disables CORS. The dashboard is same-origin and never needs this. Do not allow origins you do not control: whatever credential their users hold can act through their browser. |

## Polling

| Variable | Type | Default | Required | What it does |
| --- | --- | --- | --- | --- |
| `POLL_INTERVAL` | Go duration | `5s` | optional | How often the poller calls `getEvents`. Parsed with `time.ParseDuration`. **Minimum `1s`** — a smaller value is a startup error. |

Only applies to `SOURCE_MODE=rpc` in practice (the standalone poller).
Upstream (`sorotrail`) reads the indexer; this interval is still loaded
but the poller is not the source.

### Reorg detection

The poller records the hash of each recently ingested ledger and re-reads the
window every cycle. A ledger whose hash changes is a chain reorganisation, and
the alerts derived from the orphaned range are marked retracted — kept, never
deleted, because a delivered notification cannot be unsent. Reorgs are logged
at `warn` and counted in `sorobeacon_reorgs_total`.

| Variable | Type | Default | Required | What it does |
| --- | --- | --- | --- | --- |
| `REORG_TRACKING_WINDOW` | integer (ledgers) | `128` | optional | How many recent ledger hashes to keep and re-check. `0` disables detection (the behaviour before the feature). Roughly ten minutes of Stellar history at the default, and one `getLedgers` call per cycle. A source that cannot report ledger hashes (SoroTrail, or an RPC node too old for `getLedgers`) simply has no detection. |
| `REORG_CONFIRMATION_DEPTH` | integer (ledgers) | `0` | optional | Hold an event until it is this many ledgers behind the tip before evaluating it. Trades alert latency for fewer retractions; `0` alerts immediately, the historical default. |

## Retention and archiving

| Variable | Type | Default | Required | What it does |
| --- | --- | --- | --- | --- |
| `ALERT_RETENTION` | duration or `<n>d` | empty (keep forever) | optional | How long alerts (and their delivery attempts) are kept. Unset keeps history forever, so an upgrade never starts deleting. On Postgres, retention first drops whole expired monthly partitions (effectively free) and then deletes the ragged edge in batches of 1000. |
| `ARCHIVE_URL` | URL or path | empty (archiving off) | optional | Where retention copies a batch of expired alerts before deleting them. A local directory path, `file://`, `dir://`, or `s3://bucket/prefix` (region from `?region=` or `AWS_REGION`; credentials from `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`; `?endpoint=` for MinIO or a test server). A failed archive blocks that batch's delete, so nothing is dropped un-archived. Requires `ALERT_RETENTION` to have any effect. Archive objects are NDJSON, one alert per line, keyed by the batch's own id range so re-running is idempotent. |

Archive objects contain only alert rows (contract id, event, payload, ledger); channel `config` — the webhook URLs, bot tokens and SMTP credentials — is never read by the archiver and can never appear in one.

## Logging

| Variable | Type | Default | Required | What it does |
| --- | --- | --- | --- | --- |
| `LOG_LEVEL` | enum | `info` | optional | Minimum `log/slog` level: `debug` \| `info` \| `warn` (or `warning`) \| `error`. Logs are structured JSON on stdout. |

## Channel health

| Variable | Type | Default | Required | What it does |
| --- | --- | --- | --- | --- |
| `CHANNEL_DISABLE_AFTER_FAILURES` | integer | `0` (never auto-disable) | optional | How many consecutive **permanent** channel failures take a channel out of rotation. Negative, fractional and non-numeric values are a startup error. |

### What channel health tracks

A channel whose bot token was revoked fails on every alert, forever, and
nothing used to surface that short of reading `delivery_attempts` one alert at
a time. So the usual discovery route was "we noticed we stopped getting
alerts". Every delivery now folds its outcome into the channel:

| Field | Meaning |
| --- | --- |
| `consecutive_failures` | Deliveries that have failed since the last success. One success resets it to 0. |
| `consecutive_permanent_failures` | Permanent failures (401/403/404) since the last success. This is what auto-disable counts. A transient failure neither increments *nor* clears it, so an unrelated 5xx cannot hide a revoked credential behind it. |
| `last_error` | The most recent failure message, cleared by the next success. Notifiers redact URLs and tokens before building these, so it never contains channel config. |
| `last_error_at` / `last_success_at` | When that failure or success happened. |
| `disabled_at` | Set only when health tracking turned the channel off. This is what makes the dashboard say *auto-disabled* rather than *disabled*. |

All of it is derived, so it can always be rebuilt from delivery history, and it
is exposed on `GET /api/v1/channels` and on the dashboard's
[channels page](guides/dashboard.md).

### Permanent versus transient

Failures are split by kind, because auto-disabling is a destructive answer to a
temporary problem:

- **Permanent** — `401`, `403`, `404`, and SMTP 5xx replies. The credential or
the endpoint is gone and retrying can only ever fail again. Only these count
toward `CHANNEL_DISABLE_AFTER_FAILURES`.
- **Transient** — `5xx`, `429`, timeouts, connection errors, and anything
unrecognised. They are counted and reported, but never held against the
channel: taking a channel out of rotation because a provider had a bad
afternoon turns a short outage into silently lost alerts.

### Auto-disable and re-enabling

The threshold defaults to **off**, and turning it on is a deliberate act. When
the threshold is reached the channel is disabled, stamped with `disabled_at`,
and removed from the delivery path; the dashboard marks the row and repeats the
last error.

Getting back is **explicit**: `PATCH /api/v1/channels/{id}` with
`{"enabled": true}` (or the *Enable* button on the dashboard). That write clears
the counters and `disabled_at` in the same statement, so the channel does not
immediately re-disable on its next failure. Renaming a channel, or editing its
config, deliberately does **not** clear anything — otherwise a cosmetic edit
would wipe the evidence of a channel that is still dropping alerts. A
delivery retried by hand, or a test send, does move the counters, so an
operator who has just fixed a channel can watch it clear; a test send never
carries the disable threshold, so a diagnostic click cannot switch alerting
off.

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
