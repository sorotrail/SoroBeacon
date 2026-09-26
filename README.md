# SoroBeacon 📡

**Monitoring and alerting for Soroban smart contracts.** Point SoroBeacon at
one or more contracts on Stellar, define rules ("this event fired", "an
edmitted value crossed a threshold", "more than N in M minutes"), and get alerts
on Discord, Slack, Telegram, Matrix, PagerDuty, Twilio SMS, email, or any webhook — with a
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

Reliability around the edges: each monitor carries a **poll priority**
(`low`/`normal`/`high`, default `normal`) and the poller schedules high-priority
contracts first with a weighted round-robin that never starves the low tier. A
chain **reorganisation** is detected by re-reading recently ingested ledger
hashes — a changed hash retracts the alerts derived from the orphaned range
(kept and flagged, never deleted), and an optional confirmation depth can hold
alerts until an event is buried. On Postgres, `alerts` is **range-partitioned
by month**, so retention drops whole expired partitions instead of deleting row
by row, and it can **archive** each batch to a directory or S3 before deleting
it so a failed archive blocks the delete.

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
instance with no external service. SQLite backs a single instance well;
it serialises writes, so use Postgres for several writers or several instances.
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
| `RPC_URLS`      | _(none — `RPC_URL` is used)_           | Ordered, comma-separated endpoints to fail over between; takes priority over `RPC_URL` |
| `NETWORK_PASSPHRASE` | per network                       | Overrides the network passphrase             |
| `DATABASE_URL`  | *(required)*                           | Backend URL by scheme: Postgres (`postgres` / `postgresql`) or a single-file SQLite database (`sqlite:///path/to/sorobeacon.db`); validated at load |
| `DATABASE_MAX_CONNS` | pgx default                       | Pool max connections (`0` = driver default)  |
| `DATABASE_MIN_CONNS` | pgx default                       | Pool min connections (`0` = driver default)  |
| `DATABASE_MAX_CONN_LIFETIME` | pgx default                | Max connection lifetime (`0` = driver default) |
| `DATABASE_MAX_CONN_IDLE_TIME` | pgx default               | Max idle time (`0` = driver default)         |
| `REPLICA_DATABASE_URL` | _(unset — reads go to the primary)_ | Postgres URL of a read replica for the dashboard's list, search, stats and chart queries; must differ from `DATABASE_URL` and is rejected with a `sqlite` URL |
| `POLL_INTERVAL` | `5s`                                   | How often to poll `getEvents` (min `1s`)     |
| `HTTP_ADDR`     | `:8080`                                | API + dashboard listen address (`host:port`) |
| `HTTP_MAX_BODY_BYTES` | `1048576` (1 MiB)                 | Max API write-body size; GET is unaffected   |
| `CORS_ALLOWED_ORIGINS` | _(empty, CORS off)_             | Comma-separated browser Origins; empty disables CORS |
| `MONITOR_SILENT_AFTER` | `24h`                            | Mark monitors silent on the dashboard after this much time since last match |
| `ALERT_RETENTION` | _(unset, keep forever)_             | How long to keep alerts; `90d`, `24h`. Postgres drops whole expired partitions |
| `ARCHIVE_URL`   | _(unset, off)_                         | Archive expired alerts before deletion (directory or `s3://bucket/prefix`) |
| `REORG_TRACKING_WINDOW` | `128`                        | Recent ledger hashes tracked for reorg detection; `0` disables |
| `REORG_CONFIRMATION_DEPTH` | `0`                       | Ledgers an event must be buried before it may alert |
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

Set `RPC_URLS` to a comma-separated, ordered list and SoroBeacon fails over
between the endpoints instead of staking the alert stream on one of them.
Transport errors, `429`s and `5xx`s quarantine an endpoint with an
exponential backoff and retry the call on the next one; a `4xx` or a
JSON-RPC error does not, because it would fail identically everywhere. A
quarantined endpoint is probed again once its backoff expires, and a
successful probe puts it back in rotation. Every endpoint in the list is
checked at startup and **a mixed-network list is fatal** — failover would
otherwise interleave two chains' events. `RPC_URL` keeps working unchanged
as the single-endpoint case; never set both.

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

**Distributed tracing** answers the per-alert question metrics cannot: when
an alert was late, which stage was slow? With `OTLP_ENDPOINT` set, one
OTLP/HTTP trace per poll cycle spans the whole path — `poller.poll` →
`poller.fetch_events` (RPC fetch + decode) → `rules.evaluate` →
`poller.create_alert` → `store.create_alert` → `notify.deliver` per
channel — with each delivery a child of its alert's span, never a root.
Every span carries the ambient `X-Request-ID` as a `request_id` attribute,
so a log line and its trace can be joined. Tracing is **off by default**
(no endpoint, no exporter, no overhead); `OTLP_SAMPLE_RATE` scales it down
on busy deployments. Span attributes never contain channel config, tokens
or webhook URLs — channels are identified by row id only.

Try it locally with the collector of your choice; for example
[Jaeger](https://www.jaegertracing.io/docs/latest/getting-started/) all-in-one
exposes an OTLP/HTTP endpoint on port 4318:

```sh
# Run a local collector (Jaeger all-in-one; OTLP/HTTP on :4318,
# UI on :16686)
docker run --rm -p 16686:16686 -p 4318:4318 jaegertracing/all-in-one:latest

# Point SoroBeacon at it
cp .env.example .env   # edit DATABASE_URL as usual
OTLP_ENDPOINT=http://localhost:4318 ./bin/sorobeacon

# After a matching event lands, open http://localhost:16686 and search
# for service "sorobeacon"; one poll cycle is one trace from the RPC
# fetch to every channel delivery.
```

Channel secrets (webhook URLs, bot tokens, SMTP credentials) live in each
channel's `config` JSON in the database. They are never logged and never
returned by the API. Set `CONFIG_ENCRYPTION_KEY` to encrypt them at rest;
see the [configuration guide](docs/getting-started/configuration.md#encrypting-channel-config-at-rest).

> ⚠️ With `API_TOKEN` unset the API and dashboard are **unauthenticated**.
> Set it to require `Authorization: Bearer <token>` on `/api/v1` and a
> sign-in on the dashboard, or keep the listener on a trusted network.

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

Five rule types ship:

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

**`topic_regex`** — match a regular expression against a decoded topic, at a
given position or any topic when `position` is omitted. Real contracts emit
families of events (`swap_exact_in`, `swap_exact_out`, `pool_deposit`, …) and
one pattern covers the family. Patterns are unanchored RE2 matched within a
topic's string value and are capped at 512 bytes; a position beyond an event's
topic list simply doesn't match:

```sh
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "topic_regex",
  "params": {
    "pattern": "^swap_",
    "position": 0
  }
}'
```

**`address_watchlist`** — match any SEP-41 token event whose from or to
address is on a configured list. "Did these specific addresses move anything"
becomes one rule instead of one `token_event` rule per address. `match` is
`from`, `to` or `either` (the default); matching is exact and case-sensitive;
the address set is built once, so a watchlist of hundreds of addresses costs
no more per event than one of two:

```sh
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "address_watchlist",
  "params": {
    "addresses": ["GDW6...ACCOUNT", "GBXG...EXCHANGE"],
    "match": "either",
    "event": "transfer"
  }
}'
```

**`topic_position`** — match when the decoded topic at a fixed position
exactly equals a configured value. Custom (non-SEP-41) contracts put
meaningful values in fixed topic positions — a pool ID, a market symbol, an
account — and this is the direct "position N equals V" question that
`event_emitted` (first topic only) and `token_event` (SEP-41 slots only)
cannot ask. Comparison is exact string equality against the topic's decoded
string form; a position beyond the event's topic count is a non-match, not
an error:

```sh
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "topic_position",
  "params": {
    "position": 2,
    "equals": "POOL_USDC_XLM",
    "event": "deposit"
  }
}'
```

See [docs/rules/topic-position.md](docs/rules/topic-position.md).

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

Nine channel types ship with the MVP. `config` is validated on create/update
and never returned in responses. Each has a page under
[docs/channels/](docs/channels/):
[Discord](docs/channels/discord.md), [Slack](docs/channels/slack.md),
[Telegram](docs/channels/telegram.md), [Matrix](docs/channels/matrix.md),
[PagerDuty](docs/channels/pagerduty.md), [Email](docs/channels/email.md),
[Signal](docs/channels/signal.md), [Webex](docs/channels/webex.md),
[DingTalk](docs/channels/dingtalk.md) and the
[generic webhook](docs/channels/webhook.md).

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
# Webex:    {"bot_token": "Y2lzY29zcGFyazovL3VzL1JPT00v...", "room_id": "Y2lzY29zcGFyazovL3VzL1JPT00v..."}
# Signal:   {"api_url": "http://signal-cli:8080", "number": "+15551234567", "recipients": ["+15559876543"]}
# DingTalk: {"webhook_url": "https://oapi.dingtalk.com/robot/send?access_token=...", "secret": "SEC..."}

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

#### Live alerts (Server-Sent Events)

`GET /api/v1/alerts/stream` streams alerts as they are created, so the
dashboard and any API consumer can react without polling `/api/v1/alerts` on
a timer. SSE rather than WebSockets: the traffic is one-directional, and SSE
survives proxies (and reconnects on its own) with far less configuration.

```sh
curl -N localhost:8080/api/v1/alerts/stream             # every alert
curl -N 'localhost:8080/api/v1/alerts/stream?monitor_id=1'
```

Each alert arrives as an `alert` event whose `data` is one JSON object: the
stored alert's fields plus the monitor's `name`.

```
event: alert
data: {"id":7,"monitor_id":1,"monitor_name":"My token","rule_id":3,"event_id":"0000…","payload":{"contract_id":"C…","event_name":"transfer"},"created_at":"2026-09-24T12:00:00Z"}
```

`monitor_id` filters server-side. Comment lines (`: keep-alive`) are sent
every 15s so an idle connection is not reaped by a proxy. A slow or dead
client never blocks alert creation: each subscriber has a buffered queue and,
when it fills, the oldest pending event is dropped — the loss is counted by
`sorobeacon_alerts_stream_dropped_total` on `/metrics`. The alerts page in the
dashboard subscribes to this endpoint and appends new alerts live.

## CLI

The same binary doubles as a CLI for a running instance, so bootstrapping a
deployment or changing it from a CI pipeline does not need curl scripts. The
server starts when `sorobeacon` is run with no arguments; any argument makes
it a client:

```sh
sorobeacon monitor list
sorobeacon monitor create --name "My token" \
  --contract CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA --channel 1
sorobeacon monitor get 1
sorobeacon monitor disable 1
sorobeacon monitor delete 1

sorobeacon rule list 1
sorobeacon rule add 1 --type frequency_threshold --param event_name=transfer \
  --param count=50 --param window=5m
sorobeacon rule delete 1 2

sorobeacon channel list --type slack
sorobeacon channel create --name ops-slack --type slack \
  --config webhook_url=https://hooks.slack.com/services/...
sorobeacon channel test 1
sorobeacon channel delete 1
```

`--config` and `--param` take one `key=value` per flag, and a value is typed
by its JSON spelling: `count=50` is a number, `window=5m` a string (quote a
value that must stay a string) and `to=["ops@example.com"]` an array. For
anything nested, `--config-json` and `--params` take a whole JSON object.

The instance to talk to comes from `SOROBEACON_URL` (default
`http://localhost:8080`) and can be overridden with `--url`; `SOROBEACON_TOKEN`
or `--token` sends `Authorization: Bearer` for an instance with `API_TOKEN`
set. Output is a readable table by default and JSON with `--json`, so a script
can pipe it into `jq`. Failures print the API's error envelope message and
exit non-zero, and a channel's `config` — webhook URLs, bot tokens, SMTP
credentials — is never printed, since the API does not return it. Run
`sorobeacon help` for the command list, or `sorobeacon monitor`, `sorobeacon
rule` or `sorobeacon channel` for a group's own usage and flags.

```sh
sorobeacon monitor list --json
sorobeacon monitor create --name "My token" --contract C... --json
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
cmd/sorobeacon      wiring + graceful shutdown, CLI subcommands (cli*.go)
cmd/sorobeacon      wiring + graceful shutdown
internal/config     env config
internal/telemetry  OpenTelemetry tracer setup (OTLP/HTTP; off by default)
internal/stellar    RPC client (getEvents/getLatestLedger/getHealth) + ScVal decoder
internal/store      Postgres (pgx) + embedded golang-migrate migrations
internal/rules      RuleEvaluator interface + event_emitted, value_threshold,
                    token_event, frequency_threshold
internal/notify     Notifier interface + 7 channels + retrying dispatcher
internal/poller     ingest loop: poll -> decode -> match -> alert -> dispatch
internal/api        chi JSON API
internal/web        html/template + htmx dashboard
internal/apiclient  HTTP client for the API, shared by the CLI
```

`cmd/sorobeacon` also contains the CLI subcommands (`cli*.go`); they talk to a
running instance only through `internal/apiclient`, so the CLI and the API
cannot drift apart.

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
### Notification Channels

Supported channels include [Discord](docs/channels/discord.md), [Slack](docs/channels/slack.md), [Telegram](docs/channels/telegram.md), [Matrix](docs/channels/matrix.md), [PagerDuty](docs/channels/pagerduty.md), [Twilio SMS](docs/channels/twilio.md), [Email](docs/channels/email.md), [Signal](docs/channels/signal.md), [Webex](docs/channels/webex.md), [DingTalk](docs/channels/dingtalk.md), and generic [Webhooks](docs/channels/webhook.md).
