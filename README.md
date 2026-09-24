# SoroBeacon 📡

**Monitoring and alerting for Soroban smart contracts.** Point SoroBeacon at
one or more contracts on Stellar, define rules ("this event fired", "an
emitted value crossed a threshold", "more than N in M minutes"), and get alerts
on Discord, Slack, Telegram, Matrix, PagerDuty, email, or any webhook — with a
small dashboard to manage monitors
and review alert history.

Stellar has no good open-source way to watch a contract and get notified when
something happens on it. SoroBeacon aims to be that missing public good for
the Soroban ecosystem: a clean, well-tested core that is deliberately easy to
extend with new rule types and notification channels.

## How it works

```
Stellar RPC ──getEvents──▶ poller ──▶ decoder ──▶ rules engine ──▶ alerts ──▶ dispatcher ──▶ channels
                              │                                      │                          │
                              └── ingest_state ──────── Postgres ────┴───── delivery_attempts ──┘
```

- The **poller** calls the RPC's `getEvents` for every contract watched by an
  enabled monitor (batched across filters to respect RPC caps), following the
  pagination cursor and resuming from a checkpoint after restarts. Soroban
  RPCs only retain events for ~1–7 days, so SoroBeacon polls continuously and
  alerts in near-real-time.
- The **decoder** turns event topics/values into plain Go values, preferring
  the RPC's `xdrFormat: "json"` output and falling back to decoding base64
  XDR `ScVal`s locally (via the maintained `github.com/stellar/go-stellar-sdk`,
  which supersedes the deprecated `github.com/stellar/go`).
- The **rules engine** runs every enabled rule of every monitor watching the
  event's contract. Matches create alerts, deduplicated on
  `(rule_id, event_id)` so a rule can never fire twice for the same event.
- The **dispatcher** fans each alert out to the monitor's channels with
  retries and exponential backoff, recording every delivery attempt.

## Quickstart

```sh
git clone <this repo> && cd sorobeacon
docker compose up --build -d
open http://localhost:8080        # dashboard
```

That starts Postgres and SoroBeacon against the Stellar **testnet** RPC.
Migrations run automatically on startup.

Running without Docker:

```sh
cp .env.example .env   # edit DATABASE_URL
make build
set -a; . ./.env; set +a; ./bin/sorobeacon
```

No Postgres on the box? Point `DATABASE_URL` at a file instead —
`DATABASE_URL=sqlite:///var/lib/sorobeacon/sorobeacon.db` starts a working
instance with no external service. SQLite backs a single instance well; it
serialises writes, so use Postgres for several writers or several instances.
See [capacity and scaling](docs/operations/scaling.md).

## Configuration

All configuration comes from environment variables. The complete
operator reference — every variable `internal/config` reads, grouped by
database / RPC / HTTP / polling / logging, with types, defaults, required
vs optional, secrets, and `SOURCE_MODE`-only notes — is
[docs/configuration.md](docs/configuration.md).

| Variable        | Default                                | Description                                  |
|-----------------|----------------------------------------|----------------------------------------------|
| `SOURCE_MODE`   | `rpc`                                  | `rpc` (standalone) or `sorotrail` (upstream) |
| `SOROTRAIL_URL` | —                                      | SoroTrail indexer base URL (upstream mode)   |
| `NETWORK`       | `testnet`                              | `testnet` \| `mainnet` \| `futurenet` \| `custom` |
| `RPC_URL`       | per network                            | Stellar RPC endpoint; overrides the preset   |
| `NETWORK_PASSPHRASE` | per network                       | Overrides the network passphrase             |
| `DATABASE_URL`  | *(required)*                           | Backend URL by scheme: Postgres (`postgres` / `postgresql`) or a single-file SQLite database (`sqlite:///path/to/sorobeacon.db`); validated at load |
| `DATABASE_MAX_CONNS` | pgx default                       | Pool max connections (`0` = driver default)  |
| `DATABASE_MIN_CONNS` | pgx default                       | Pool min connections (`0` = driver default)  |
| `DATABASE_MAX_CONN_LIFETIME` | pgx default                | Max connection lifetime (`0` = driver default) |
| `DATABASE_MAX_CONN_IDLE_TIME` | pgx default               | Max idle time (`0` = driver default)         |
| `POLL_INTERVAL` | `5s`                                   | How often to poll `getEvents` (min `1s`)     |
| `HTTP_ADDR`     | `:8080`                                | API + dashboard listen address (`host:port`) |
| `HTTP_MAX_BODY_BYTES` | `1048576` (1 MiB)                 | Max API write-body size; GET is unaffected   |
| `CORS_ALLOWED_ORIGINS` | _(empty, CORS off)_             | Comma-separated browser Origins; empty disables CORS |
| `MONITOR_SILENT_AFTER` | `24h`                            | Mark monitors silent on the dashboard after this much time since last match |
| `HTTP_ADDR`     | `:8080`                                | API + dashboard listen address               |
| `LOG_LEVEL`     | `info`                                 | `debug` \| `info` \| `warn` \| `error`       |
| `READYZ_LAG_THRESHOLD` | `0` (disabled)                  | Fail `/readyz` when poller ledger lag exceeds this; 0 leaves probes unchanged |
| `RATE_LIMIT_RPS` | `0` (off)                             | Per-client API requests per second           |
| `RATE_LIMIT_BURST` | `ceil(RPS)` when enabled            | Per-client token-bucket size                 |
| `RATE_LIMIT_TRUST_FORWARDED` | `false`                  | Key clients by `X-Forwarded-For` (proxy only) |

### Networks

A Stellar network's passphrase is its identity. `NETWORK` selects a preset
(`testnet`, `mainnet`, `futurenet` — each carrying its public RPC endpoint
and passphrase); `NETWORK=custom` takes `RPC_URL` + `NETWORK_PASSPHRASE`
for private standalone networks. At startup SoroBeacon asks the RPC which
network it belongs to and **refuses to start on a mismatch**, so a mainnet
endpoint behind testnet configuration fails fast instead of silently
evaluating every monitor against the wrong chain.

### Operating modes

- **`rpc` (default)** — SoroBeacon polls the Stellar RPC node itself.
- **`sorotrail`** — SoroBeacon reads events from a
  [SoroTrail](https://github.com/sorotrail/SoroTrail) indexer instead.
  SoroTrail stores events durably past the RPC's ~1-7 day retention window,
  so upstream monitoring covers history the RPC has already dropped — and
  several SoroBeacon instances can share one indexer.

The ingest loop knows only an `EventSource` interface; adding a backend is
implementing two methods. See
[CONTRIBUTING](CONTRIBUTING.md) and the
[architecture reference](docs/reference/architecture.md).

### Observability

`/metrics` serves Prometheus instrumentation: poll outcomes and duration,
poll lag behind the chain tip, seconds since the last poll, the
events-scanned → events-matched → alerts-fired funnel, deliveries by
channel and outcome, and HTTP request duration by route pattern.
`/api/v1/livez` and `/api/v1/readyz` are orchestration probes (liveness
checks nothing; readiness checks the database and the event source with
per-dependency detail). `/api/v1/version` reports the version, commit and
build date baked in at compile time. Every response carries an
`X-Request-ID` correlation header, echoed in error bodies and log lines.

Channel secrets (webhook URLs, bot tokens, SMTP credentials) live in each
channel's `config` JSON in the database. They are never logged and never
returned by the API. Set `CONFIG_ENCRYPTION_KEY` to encrypt them at rest;
see the [configuration guide](docs/getting-started/configuration.md#encrypting-channel-config-at-rest).

> ⚠️ With `API_TOKEN` unset the API and dashboard are **unauthenticated**.
> Set it to require `Authorization: Bearer <token>` on `/api/v1` and a
> sign-in on the dashboard, or keep the listener on a trusted network.

## Deployment

### Several instances (high availability)

Every instance serves the API and the dashboard; exactly one of them polls.
Instances compete for a Postgres session-level advisory lock
(`pg_try_advisory_lock`, key `0x534F4245434F4E`), and the holder runs the ingest
loop and the retention pruner. There is no extra table, no migration and no
coordinator process to run — a second instance is just a second instance:

```sh
# Two replicas of the same deployment, one poller between them.
docker compose up -d --scale sorobeacon=2
```

- **One poller, always.** Without the lease, two replicas ingest the same
  events and race the same checkpoint, so every alert is delivered twice and
  each instance believes the other's progress is its own.
- **Failover is bounded by the lease interval (3s)** — the follower's next
  attempt to take the lock. A leader that exits gracefully releases the lock on
  the way out; a leader that is killed frees it when its database session
  disappears. There is no long fixed timer in either path.
- **A demoted leader stops polling.** Its poller context is cancelled and the
  lock is not given up until the poller has returned. A leader that loses its
  database connection notices on the next renewal and stops, rather than
  polling alongside the new leader — that overlap is the split-brain case that
  duplicates alerts.
- **The lease uses one dedicated connection per instance**, outside the
  `DATABASE_MAX_CONNS` pool, because an advisory lock lives on the session that
  took it and a pooled connection cannot be pinned for that.
- **A follower is healthy.** `GET /api/v1/health` reports `leader`,
  `leader_election` and `leader_since` so an operator can see which replica
  polls, and the overview page says the same. A follower answers every other
  endpoint normally and never fails readiness for not polling.
- **PgBouncer needs session pooling.** Leader election holds a session-level
  lock, so `DATABASE_URL` must reach Postgres directly or through a
  session-pooled PgBouncer; in transaction pooling mode the lock cannot be
  held. Followers then never promote, and no instance polls.

### SQLite: a single node, no election

A `sqlite://` `DATABASE_URL` is single-node by construction: one file on one
machine, and no advisory locks to take. SoroBeacon runs the poller
unconditionally and `GET /api/v1/health` reports `"leader": true` with
`"leader_election": false`. Do not point several instances at one SQLite file —
use Postgres when you want more than one. See
[capacity and scaling](docs/operations/scaling.md).

## HTTP API

All endpoints are under `/api/v1`.

### Monitors

```sh
# Create a monitor watching one or more contracts
curl -s -X POST localhost:8080/api/v1/monitors -d '{
  "name": "My token",
  "contract_ids": ["CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"],
  "channel_ids": [1]
}'

curl -s localhost:8080/api/v1/monitors            # list (add ?enabled=true)
curl -s localhost:8080/api/v1/monitors/1          # get one
curl -s -X PATCH localhost:8080/api/v1/monitors/1 -d '{"enabled": false}'
curl -s -X DELETE localhost:8080/api/v1/monitors/1
```

`PATCH` accepts any subset of `name`, `contract_ids`, `enabled`,
`channel_ids`; `channel_ids` replaces the monitor's channel attachments.

### Rules

Four rule types ship:

**`event_emitted`** — match on event name (the first topic, by Soroban
convention) and/or exact topic values:

```sh
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "event_emitted",
  "params": {
    "event_name": "transfer",
    "topic_equals": {"1": "GDW6...SENDER"}
  }
}'
```

**`value_threshold`** — numeric comparison on the event's decoded value.
`value_path` is a dot path into the value (map keys / array indexes); omit it
when the value itself is the number. Use a string threshold for integers
beyond 53 bits:

```sh
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "value_threshold",
  "params": {
    "event_name": "transfer",
    "value_path": "amount",
    "comparison": "gt",
    "threshold": "1000000000"
  }
}'
```

`comparison` is one of `gt`, `gte`, `lt`, `lte`, `eq`, `neq`.

**`token_event`** — SEP-41 token events, with the interface's topic layout
built in. `event` is one of `transfer`, `mint`, `burn`, `clawback`,
`set_admin`, or `*` for any of them; `from`/`to` match the semantic address
slots (from is the holder on burn/clawback, the sender on transfer), and
`min_amount`/`max_amount` are inclusive i128 bounds as decimal strings:

```sh
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "token_event",
  "params": {
    "event": "transfer",
    "from": "GDW6...SENDER",
    "min_amount": "1000000000"
  }
}'
```

**`frequency_threshold`** — "more than N matching events within M minutes", a
rolling-window aggregate for mint storms, drain attacks and oracle flapping.
`count` is the positive threshold and `window` a Go duration; `event_name`
scopes which events are counted. It fires once per threshold crossing and then
stays quiet for one full window, so sustained activity alerts at most once per
window rather than per event; the rolling window is rebuilt from stored alerts
after a restart:

```sh
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "frequency_threshold",
  "params": {
    "event_name": "transfer",
    "count": 50,
    "window": "5m"
  }
}'
```

See [docs/rules/frequency-threshold.md](docs/rules/frequency-threshold.md) for
the re-arm semantics.

Every rule type also accepts an optional `cooldown` (a Go duration string such
as `"5m"`): the first match alerts, further matches in the window are counted
and dropped, and the next alert reports `suppressed_since_last`. It survives a
restart and is enforced in the database alongside the dedup guard — see
[docs/rules/cooldown.md](docs/rules/cooldown.md).

```sh
curl -s localhost:8080/api/v1/monitors/1/rules
curl -s -X PATCH localhost:8080/api/v1/monitors/1/rules/2 -d '{"enabled": false}'
curl -s -X DELETE localhost:8080/api/v1/monitors/1/rules/2
```

### Channels

Seven channel types ship with the MVP. `config` is validated on create/update
and never returned in responses. Each has a page under
[docs/channels/](docs/channels/):
[Discord](docs/channels/discord.md), [Slack](docs/channels/slack.md),
[Telegram](docs/channels/telegram.md), [Matrix](docs/channels/matrix.md),
[PagerDuty](docs/channels/pagerduty.md), [Email](docs/channels/email.md) and
the [generic webhook](docs/channels/webhook.md).

```sh
# Discord
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "ops-discord", "type": "discord",
  "config": {"webhook_url": "https://discord.com/api/webhooks/..."}
}'

# Slack:    {"webhook_url": "https://hooks.slack.com/services/..."}
# Telegram: {"bot_token": "123:abc", "chat_id": "-1001234567890"}
# Email:    {"host": "smtp.example.com", "port": 587, "username": "u",
#            "password": "p", "from": "beacon@example.com", "to": ["ops@example.com"]}
# Webhook:  {"url": "https://example.com/hook", "secret": "shared-secret"}
# Matrix:   {"homeserver_url": "https://matrix.example.org", "access_token": "syt_...",
#            "room_id": "!abcdef:example.org"}
# PagerDuty:{"routing_key": "R0UT1NGK3Y", "severity": "warning"}

curl -s localhost:8080/api/v1/channels
curl -s -X PATCH localhost:8080/api/v1/channels/1 -d '{"enabled": false}'
curl -s -X DELETE localhost:8080/api/v1/channels/1

# Send a test alert through a channel
curl -s -X POST localhost:8080/api/v1/channels/1/test
```

Generic webhook deliveries carry an `X-SoroBeacon-Timestamp` header and an
`X-SoroBeacon-Signature` header: the hex HMAC-SHA256 of `<timestamp>.<raw
body>` under your `secret`. During a rotation, set `previous_secret` and a
second `X-SoroBeacon-Signature-Previous` header lets receivers still on the
old key verify. See [the webhook channel docs](docs/channels/webhook.md) for
the canonical string and a verification example.

Discord, Slack, Telegram and email configs accept an optional `template` (a Go
`text/template` over the alert fields) to override the message; see
[docs/channels/templates.md](docs/channels/templates.md).

### Alerts, health, stats

```sh
curl -s 'localhost:8080/api/v1/alerts?monitor_id=1&rule_id=3&sort=created_at_asc&limit=20'
curl -s 'localhost:8080/api/v1/alerts?cursor=42'      # keyset pagination (next_cursor)
curl -s localhost:8080/api/v1/alerts/7/deliveries     # delivery attempts for one alert
curl -s localhost:8080/api/v1/health
curl -s localhost:8080/api/v1/stats
```

## Development

```sh
make build      # go build -> bin/sorobeacon
make test       # unit tests (store integration tests skip without a DB)
make test-db    # all tests against the compose Postgres
make lint       # golangci-lint
make up / down  # docker compose
```

Layout:

```
cmd/sorobeacon      wiring + graceful shutdown
internal/config     env config
internal/stellar    RPC client (getEvents/getLatestLedger/getHealth) + ScVal decoder
internal/store      Postgres (pgx) and SQLite backends + embedded
                    golang-migrate migrations (parallel sets)
internal/rules      RuleEvaluator interface + event_emitted, value_threshold,
                    token_event, frequency_threshold
internal/notify     Notifier interface + 7 channels + retrying dispatcher
internal/poller     ingest loop: poll -> decode -> match -> alert -> dispatch
internal/lease      Postgres advisory-lock leader election for the poller
internal/api        chi JSON API
internal/web        html/template + htmx dashboard
```

### Adding a notification channel

Implement `notify.Notifier` and register a constructor — that's it:

```go
// internal/notify/matrix.go
func NewMatrix(config json.RawMessage) (notify.Notifier, error) { ... }

// register it in DefaultFactory (internal/notify/notify.go):
f.Register("matrix", NewMatrix)
```

Validate config in the constructor (the API calls it to reject bad channels
early), keep secrets out of error messages, and add a test. See
`internal/notify/slack.go` for the smallest complete example.

### Adding a rule type

Implement `rules.RuleEvaluator` (an `Evaluate` + a `Validate` method) and
register it in `rules.NewRegistry`:

```go
// internal/rules/cooldown.go
type Cooldown struct{}
func (Cooldown) Validate(params json.RawMessage) error { ... }
func (Cooldown) Evaluate(ctx context.Context, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error) { ... }

// register it in NewRegistry (internal/rules/rules.go):
r.Register("cooldown", Cooldown{})
```

Decoded events use a small value vocabulary (`nil`, `bool`, `string`,
`*big.Int`, `[]byte`, `[]any`, `map[string]any`); `stellar.Canon`,
`stellar.ToBigFloat` and `stellar.Lookup` are the helpers rules build on.

### Open contributor issues (by design)

- More rule types (absence-of-event, aggregation windows)
- More channels (ntfy, ...)
- A richer SPA dashboard (the current one is intentionally minimal)
- Contract-spec-aware event decoding (named fields instead of raw topics)

## License

Apache-2.0 — see [LICENSE](LICENSE).
