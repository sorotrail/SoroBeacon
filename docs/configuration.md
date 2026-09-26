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
| `VAULT_TOKEN` | **yes** | Credential for the Vault secret provider. Never logged; a resolved secret is never returned by the API. |
| `RPC_URL` / `RPC_URLS` / `SOROTRAIL_URL` | maybe | A URL is not a password, but provider URLs sometimes embed tokens in the path or query. Do not commit those. |
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

`REPLICA_DATABASE_URL` points the read-only dashboard queries at a Postgres
read replica and is likewise rejected with a `sqlite` URL. It is off by
default; see [Read replicas](operations/scaling.md#read-replicas).

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

## External secrets

A channel config value can reference a secret held outside SoroBeacon — in
Vault or the process environment — instead of storing the value in the
database. The value is resolved when a notifier is constructed and is never
written back, logged or returned by the API. See
[External secrets in channel config](channels/secrets.md) for the reference
syntax and providers.

| Variable | Type | Default | Required | What it does |
| --- | --- | --- | --- | --- |
| `SECRETS_PROVIDER` | string | empty (disabled) | optional | `env` or `vault`; selects the provider that resolves `${secret:...}` references. Unset treats references as literals, so an upgrade changes nothing. |
| `SECRETS_CACHE_TTL` | duration | `5m` | optional | How long a resolved secret is reused so every alert does not hit the provider. `0s` disables caching. The cache is dropped whenever a channel is created, updated or deleted. |
| `VAULT_ADDR` | URL string | _(none)_ | required when `SECRETS_PROVIDER=vault` | Vault base URL, e.g. `https://vault.example:8200`. |
| `VAULT_TOKEN` | secret string | _(none)_ | optional | Vault token sent as `X-Vault-Token`. Never logged. |
| `VAULT_NAMESPACE` | string | empty | optional | Vault Enterprise namespace sent as `X-Vault-Namespace`. |

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
| `RPC_URLS` | comma-separated URL list | _(none — `RPC_URL` is used)_ | optional | Ordered RPC endpoints to fail over between, highest priority first. **Takes priority over `RPC_URL` when set**, so you never need both; entries are trimmed, so `a, b` and `a,b` are the same list. Every entry must be an absolute `http` or `https` URL, and one endpoint that names no URL at all (`RPC_URLS=,,`) is a startup error rather than a silent fallback. |
| `NETWORK_PASSPHRASE` | string | per `NETWORK` | **required when `NETWORK=custom`**; optional override otherwise | Network passphrase. Always wins over the preset, so a named network with a local quickstart passphrase works. |

`NETWORK=custom` is for private standalone networks: both an RPC endpoint
(`RPC_URL` or `RPC_URLS`) and `NETWORK_PASSPHRASE` must be set.

### Failing over between endpoints

A public Soroban RPC endpoint rate-limits and goes down, and a monitoring tool
that stops seeing events is the one failure mode it cannot have. With
`RPC_URLS` set, every call goes to the first endpoint in the list that is not
quarantined.

This is the only supported way to keep a paid endpoint with a public
fallback — one list, in priority order:

```sh
RPC_URLS=https://my-paid-rpc.example,https://soroban-testnet.stellar.org
```

A transport error, a `429` or a `5xx` answer means that endpoint is at fault,
so it is quarantined for an exponentially growing backoff (5s, 10s, 20s …
capped at 5m) and the same call is immediately retried on the next endpoint.
Once the backoff expires the endpoint rejoins the rotation, and a successful
probe clears its failure count. A `4xx` answer or a JSON-RPC error object does
**not** trigger failover: the node understood the request and rejected it, so
the next endpoint would reject it identically, and counting it against an
endpoint would quarantine a node that is fine. While at least one endpoint is
in rotation the poller never notices any of this.

Every endpoint is checked at startup and SoroBeacon **refuses to start** if one
of them reports a different network passphrase than the configured one. That
check exists because failover spreads calls across the whole list: a set that
mixed mainnet and testnet would feed a mixture of two chains' events into the
alert stream, intermittently, which is far harder to spot than a hard failure.
An endpoint that is unreachable at startup is logged and skipped instead —
that is what failover is for.

Quarantine and recovery are logged (`rpc endpoint quarantined`,
`rpc endpoint recovered`) with the endpoint URL, the failure count and the
backoff, and the same per-endpoint failure counts are exposed by
`stellar.FailoverClient.Stats()` for the metrics work tracked in
[#26](https://github.com/sorotrail/SoroBeacon/issues/26).

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

## Tracing (OpenTelemetry)

| Variable | Type | Default | Required | What it does |
| --- | --- | --- | --- | --- |
| `OTLP_ENDPOINT` | URL string | empty (tracing off) | optional | OTLP/HTTP base URL spans are shipped to (e.g. `http://localhost:4318` for a local Jaeger). Must be an absolute `http`/`https` URL when set; empty disables tracing entirely — no exporter, no goroutines, no measurable overhead. |
| `OTLP_SERVICE_NAME` | string | `sorobeacon` | optional | `service.name` resource attribute reported with every trace. |
| `OTLP_SAMPLE_RATE` | float | `1` | optional | Fraction of traces kept, in `[0, 1]`. Sampling is parent-based, so a whole poll-cycle trace is kept or dropped as a unit — never half a trace. Out-of-range or non-numeric values fail startup. |

When enabled, one trace covers each poll cycle: `poller.poll` (root) →
`poller.fetch_events` per page (RPC fetch + local decode) →
`rules.evaluate` per rule → `poller.create_alert` → `store.create_alert`
→ one `notify.deliver` per channel. Deliveries are children of their
alert's span, never roots, so one alert's full path is one timeline —
which is the point: metrics (issue #26) show aggregates, traces show the
individual path. Every span also carries a `request_id` attribute (the
ambient `X-Request-ID`) so a log line and its trace can be joined.

Span attributes hold identifiers only (alert, monitor, rule, channel and
event ids, channel type, outcome). **Channel config — webhook URLs, bot
tokens, SMTP credentials — never enters a span**, the same rule that
applies to logs and API responses.

Shutdown flushes pending spans to the collector (bounded to 5 seconds) so
the last moments of a deployment are exported rather than dropped.

```sh
# Local collector: Jaeger all-in-one (OTLP/HTTP on :4318, UI on :16686)
docker run --rm -p 16686:16686 -p 4318:4318 jaegertracing/all-in-one:latest

export OTLP_ENDPOINT=http://localhost:4318
# OTLP_SERVICE_NAME=sorobeacon-staging   # optional
# OTLP_SAMPLE_RATE=0.25                  # keep a quarter of traces
```

## Not environment variables

| Thing | Where it lives |
| --- | --- |
| Channel webhook URLs, bot tokens, SMTP credentials | Channel `config` JSON in Postgres |
| Prometheus metrics | Always on `/metrics` — no flag |
| Build version / commit | Baked in at compile time; served at `GET /api/v1/version` |
| `TEST_DATABASE_URL` | Test-only (`make test-db`); not read by `config.Load` |

## Drift vs `.env.example`

`.env.example` is the copy-paste template. This reference was written
against `internal/config` on the same commit; `.env.example` lists every
runtime variable with matching defaults, including the tracing trio
(`OTLP_ENDPOINT`, `OTLP_SERVICE_NAME`, `OTLP_SAMPLE_RATE`).
