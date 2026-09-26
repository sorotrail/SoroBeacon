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
| `WORKSPACE_TOKENS` | **yes** | Same shape of credential as `API_TOKEN`, one per workspace. The whole value is a list of secrets; the workspace ids in it are not. |
| `OIDC_CLIENT_SECRET` | **yes** | The provider's secret for this instance. Sent only to the provider's token endpoint, and never logged, rendered or echoed in an error. It authorises nothing at SoroBeacon's own API — a signed-in browser holds a session cookie, not this. |
| `NETWORK_PASSPHRASE` | no (public nets) | SDF passphrases are public. For `NETWORK=custom` it identifies a private network — treat it as operational config, not a credential. |
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

## Workspaces

| Variable | Type | Default | Required | What it does |
| --- | --- | --- | --- | --- |
| `WORKSPACE_TOKENS` | comma-separated `workspace=token` pairs | empty (single workspace) | optional | Bearer tokens scoped to a named workspace. Each pair is a credential like `API_TOKEN`'s — accepted on `/api/v1` and at `/login` — but the request it authenticates can only see and change that workspace's monitors, channels, rules, alerts, saved searches and templates. |

```sh
# two teams, one instance, no shared data
export WORKSPACE_TOKENS="acme=$(openssl rand -hex 32),beta=$(openssl rand -hex 32)"
curl -s localhost:8080/api/v1/monitors -H "Authorization: Bearer $ACME_TOKEN"
```

A workspace id is 1–40 characters of `[a-z0-9_-]`, starting with a letter or a
digit. The narrow grammar is the injection guard for the scoping predicate, so
anything else — `Acme`, `acme eu`, `acme;drop` — is rejected at startup rather
than reaching SQL. The separator is the **first** `=`: write `acme=a=b` and the
token is `a=b`. A token that appears under two workspaces is an error, because
the request it authenticates could not be given one scope.

`API_TOKEN` still works and holds the unscoped credentials: every token in it
belongs to the `default` workspace, which is also where every row written before
this feature existed already lives. So an existing single-tenant deployment
upgrades with no change at all, and adding `WORKSPACE_TOKENS` alongside it
creates a second, separate view of the same instance rather than moving anyone's
data.

**The workspace is a property of the credential.** There is no `X-Workspace`
header and no `?workspace=` parameter, on purpose: a client that could name its
own workspace could read another team's monitors by changing a header. Signing
in to the dashboard with a workspace token scopes the whole session the same
way.

The one exception is work the instance does on its own behalf — the retention
pruner, ingestion and reorg handling — which runs outside any request and sees
every workspace at once. Alert deduplication is also global (`rule_id`,
`event_id`): two teams watching the same contract through the same rule text
share the fingerprint space, which is what prevents one team's event from
alerting twice. Credentials, rule definitions and channel types are code, not
tenant data; only the rows you create through the API or dashboard are scoped.

## Single sign-on (OIDC)

| Variable | Type | Default | Required | What it does |
| --- | --- | --- | --- | --- |
| `OIDC_ISSUER` | URL | empty (single sign-on off) | optional | The provider's base URL. `GET /login` then also offers a **Sign in with `<issuer host>`** button that runs an authorization-code login with PKCE. `/.well-known/openid-configuration` under this URL is read **at startup**, so a mistyped issuer or an unreachable provider fails the deploy rather than the first sign-in. Setting it is what switches SSO on. |
| `OIDC_CLIENT_ID` | string | _(none)_ | **required when `OIDC_ISSUER` is set** | This instance's registration at the provider. The ID token's audience is checked against it, so another client's tokens are refused even when they are signed by the same provider. |
| `OIDC_CLIENT_SECRET` | string | empty | optional | Credential sent to the provider's token endpoint. A secret: never logged, rendered, or echoed in an error. Empty is what a public client (or a private_key_jwt/mTLS registration) expects, and SoroBeacon does not second-guess it. |
| `OIDC_REDIRECT_URL` | URL | _(none)_ | **required when `OIDC_ISSUER` is set** | This instance's absolute callback URL (`https://beacon.example.com/login/oidc/callback`), which must match a redirect URI registered at the provider. Configured rather than derived from the `Host` header so the provider's allow-list still means something behind a proxy. |
| `OIDC_SCOPES` | space- or comma-separated list | `openid profile email` | optional | Scopes to ask for. `openid` is always included, with or without it being written; `email` only matters when `OIDC_ALLOWED_DOMAINS` is set. |
| `OIDC_WORKSPACE` | workspace id | `default` | optional | Which workspace a signed-in user lands in. Seeded into the workspace list at startup like a `WORKSPACE_TOKENS` entry, so the tenant exists before its first user arrives. |
| `OIDC_WORKSPACE_CLAIM` | claim name | empty (no claim mapping) | optional | Read the workspace from this ID-token claim instead, for one instance serving several teams from one provider. A value outside `[a-z0-9_-]`, or a token without the claim, falls back to `OIDC_WORKSPACE` rather than becoming a workspace of its own. A tenant named this way is not in the `workspaces` registry — startup cannot know its names — which is harmless because the scoping predicate matches the `workspace_id` column directly and 0012 deliberately adds no foreign key to it. |
| `OIDC_ALLOWED_DOMAINS` | comma- or space-separated list | empty (every account the provider admits) | optional | E-mail domain allow-list, compared case-insensitively after the ID token is verified. Refusing an account is deliberate configuration, not a default: a provider that only admits your own users needs no list. |
| `OIDC_LOGIN_STATE_TTL` | Go duration | `10m` | optional | How long an unfinished login stays completable. A login in flight is a nonce, a PKCE verifier and a redirect target sitting in this process's memory, so this bounds how long they can wait to be used. Must be greater than 0. |

```sh
# Keycloak, one instance, one team
OIDC_ISSUER=https://accounts.example.com/realms/soro
OIDC_CLIENT_ID=sorobeacon-dashboard
OIDC_CLIENT_SECRET=<from the provider>
OIDC_REDIRECT_URL=https://beacon.example.com/login/oidc/callback
OIDC_ALLOWED_DOMAINS=example.com
```

**Authorization code with PKCE, not implicit.** The dashboard is a
server-rendered app with a secret-bearing backend, so it can keep a
`client_secret` and finish the flow over a back channel where a URL bar cannot.
The implicit flow hands an ID token to the browser through a fragment, where it
lands in history, in referers and in every extension that watches the page — and
it has no PKCE, so the token cannot be bound to the login that asked for it. New
providers deprecate it for exactly that reason. What SoroBeacon asks for is
`response_type=code` with `code_challenge_method=S256`, and the code is
exchanged by this process, not the browser.

**What a callback has to survive.** Verification is not hand-rolled: the
`github.com/coreos/go-oidc/v3` verifier checks the signature against the
provider's JWKS (rotating as keys are published), the issuer, the audience
against `OIDC_CLIENT_ID`, and the expiry. On top of that this package checks the
**nonce**, which the library deliberately leaves to the caller: the value is
generated when the login starts, sent to the provider, and required to appear
unchanged in the ID token, so a token captured from some other login cannot be
presented here. The `state` that ties a callback to its login is single-use and
held server-side (nonce, verifier and post-login target all live under it), and
the browser also gets an `HttpOnly`, `SameSite=Lax` cookie holding the same
value — a callback must present it in both places, which is what stops an
attacker who signed in themselves from handing a victim the session for it.

**First login auto-provisions, because there is nothing to provision.** SoroBeacon
has no user table, and adding one was the rejected alternative: it would need a
migration, an admin screen, a deprovisioning story, and a place to store
passwords this feature exists to stop storing. A verified identity from an
admitted domain simply mints the same in-memory dashboard session a token
mints — 12 hours, cleared by a restart, scoped to the mapped workspace. Revocation
therefore belongs to the provider: deactivate the account there and the next
login fails, while a session already issued runs out on its own. That window is
the honest cost of not having a user database; a shorter session TTL is not
configurable today, so an operator who needs instant revocation should keep
`API_TOKEN` as the credential and treat SSO as the convenience path — or file the
gap, because it is real.

**A session is a session.** An SSO login cannot be told apart from a token login
downstream, and it inherits the same properties: it acts on one workspace, which
the credential chose and the request cannot; and it is **unrestricted** within
it, holding no scope list. That is why a scoped API token cannot sign in at
`/login` (see [Scoped, expiring tokens](operations/security.md#scoped-expiring-tokens))
— and why an SSO session
is a dashboard and API credential, not a read-only one.

Local login is untouched. `API_TOKEN` and `WORKSPACE_TOKENS` keep working
beside a provider, so the switch is incremental and a broken registration never
locks the operator out. With `OIDC_ISSUER` unset the `/login/oidc` routes answer
404 and the sign-in page shows the token form alone. A setting that names SSO
without an issuer (`OIDC_CLIENT_ID` on its own, say) is a startup error rather
than a half-configured login page.



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
| `NETWORKS` | comma-separated list of `NETWORK` names | _(none — only `NETWORK` is polled)_ | optional | Poll several Stellar chains from one instance, primary first. Each chain after the first reads its own suffixed variables (`RPC_URL_MAINNET`, `RPC_URLS_MAINNET`, `NETWORK_PASSPHRASE_MAINNET`). See [Polling several networks](#polling-several-networks). |
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

### Polling several networks

`NETWORKS` is a comma-separated list of the same names `NETWORK` takes, **primary
first**:

```sh
NETWORKS=testnet,mainnet
# the primary keeps the unsuffixed variables
RPC_URL=https://soroban-testnet.stellar.org
# every later chain reads its own suffixed names, else its public preset
RPC_URLS_MAINNET=https://my-paid-mainnet.example,https://soroban-mainnet.stellar.org
```

One poller per chain runs in the one process, each with its own RPC client, its
own contract-spec decoder, its own cursor and its own backoff. The isolation is
the point: a chain whose node is unreachable, rate-limiting, or whose event
stream makes a poller panic stops only that chain. The panic is recovered,
counted (`sorobeacon_poll_panics_total{network=…}`) and that one loop restarts
after `POLL_INTERVAL`; the other chains keep ingesting.

Chains number their ledgers independently, so `CABC…` on testnet and the same
address on mainnet are two different contracts. That is why:

- **a monitor belongs to one network, fixed at creation.** `POST /monitors` takes
  `network` (omitted means the primary) and rejects a chain this instance does
  not poll, rather than creating a monitor that matches nothing forever.
  `PATCH` refuses to change it: re-homing a monitor would silently repoint its
  rules at whatever those ids resolve to on the other chain, and leave its
  stored alerts labelled with the old one. Delete and recreate to switch.
- **reorg detection is per chain.** A shared ledger-hash window would see
  mainnet ledger 100 overwrite testnet ledger 100 and call it a divergence — a
  false positive that retracts valid alerts.
- **`GET /monitors?network=testnet` and `GET /alerts?network=testnet`** filter,
  and the dashboard's monitors, alerts and index pages carry the same filter and
  a per-network ingest table.

Every ingest metric is labelled `network`, and `/metrics` stays one scrape target
because the label is applied per poller over one registry.

`GET /health` gains a `networks` array (processed ledger, chain tip, lag, last
poll, source state) and reports `degraded` when any chain's source is failing —
`rpc` on its own is only ever the primary chain. `/readyz` keeps its `rpc` check
and adds one per chain (`rpc_testnet`, `rpc_mainnet`). The aggregate lag reported
at the top level is the **worst** chain's, so a proxy that only reads the summary
cannot see a stalled network as in sync.

Two mistakes are startup errors rather than quiet behaviour:

- `NETWORK=mainnet` with `NETWORKS=testnet,mainnet`. The unsuffixed `RPC_URL` and
  `NETWORK_PASSPHRASE` configure the **primary**, so a `NETWORK` that is not the
  first entry would leave one chain's endpoint serving another chain's monitors.
  Put `NETWORK`'s value first, or unset it.
- `SOURCE_MODE=sorotrail` with more than one entry. One upstream deployment reads
  one chain, so the second poller could never advance.

Rows written before this feature have no network label, and the migration
deliberately does not guess one. At startup the instance labels them with the
primary network — the chain they really were polled from — so upgrading a
single-network deployment neither loses its monitors from the listings nor
strands its alert history outside every filter.

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
