# Troubleshooting

The most common "it does not work" reports are diagnosable from endpoints
and logs that already exist. Work through the matching section below instead
of opening a bug first.

Channel `config` holds webhook URLs, bot tokens and SMTP credentials. Never
paste those into a bug report, a log snippet, or a `response_snippet`.
Redact them.

## First checks

```sh
curl -s localhost:8080/api/v1/livez      # process alive (always 200 if up)
curl -s localhost:8080/api/v1/readyz     # database + RPC/indexer
curl -s localhost:8080/api/v1/health     # same dependencies, extra RPC ledger
curl -s localhost:8080/api/v1/stats      # monitors/rules/channels/alerts, last poll
curl -s localhost:8080/api/v1/version    # version, commit, build date
```

| Probe | What a failure means |
| --- | --- |
| `GET /api/v1/livez` is unreachable | The process is not listening on `HTTP_ADDR` (default `:8080`). Jump to [The service does not start](#the-service-does-not-start). |
| `GET /api/v1/readyz` returns `503` | Read `checks.database` vs `checks.rpc`. Database down → [Database connection failures](#database-connection-failures). RPC/indexer down → [RPC errors and rate limiting](#rpc-errors-and-rate-limiting). |
| `GET /api/v1/health` returns `503` | Same split: `db` vs `rpc`. `rpc_latest_ledger` is present only when the RPC answered. |
| `GET /api/v1/stats` shows `last_poll_at` far in the past, or `last_ledger` stuck | The poller is not ingesting. Check `poll failed` log lines and the RPC section. |

`livez` is deliberately empty: a process whose database and RPC are both
down is still alive. Kubernetes should use `livez` for liveness and `readyz`
for readiness, not the other way around.

## No alerts appearing

This is usually correct behaviour, not a fault.

**A monitor only matches events that occur after it was created.** Soroban
RPC nodes retain events for roughly 1–7 days and SoroBeacon does not
backfill history. An empty dashboard right after you create a monitor is
expected until the contract emits a matching event.

Walk this sequence:

1. `GET /api/v1/stats` — `monitors`, `rules` and `channels` should be
   non-zero if you think you configured them. `alerts` / `alerts_last_24h`
   stay at zero until a rule matches.
2. Confirm the monitor and its rules are **enabled** (`GET /api/v1/monitors`
   with `?enabled=true`, or the dashboard monitor detail). A disabled
   monitor is not polled; a disabled rule is not evaluated.
3. Confirm the contract ID is a `C…` strkey. One malformed ID can stall
   `getEvents` for every monitor sharing the request.
4. Confirm the rule type and params actually match the event (`event_name`,
   threshold, and so on). See the [rule reference](rules/event-emitted.md).
5. Look for `alert created` in the logs (`alert_id`, `monitor`, `rule_id`,
   `event_id`). If that line never appears, nothing matched — not a
   delivery problem.
6. Look for `poll failed` (`err`, `retry_in`). If the poller is backing
   off, events are not being ingested at all.

If `stats.last_poll_at` is recent and `last_ledger` is moving, the poller
is healthy and the contract simply has not emitted a matching event since
the monitor was created.

## Alerts appearing but not delivered

An alert in history means the rule matched. Delivery is a separate path.

1. `GET /api/v1/alerts/{id}/deliveries` (or the dashboard alert row) —
   every attempt is recorded with `status` `success` or `failed` and a
   redacted `response_snippet`.
2. Confirm the monitor has **enabled** channels attached. Dispatch skips
   disabled channels.
3. Look for `alert delivered` (`alert_id`, `channel_id`, `attempt`) vs
   `alert delivery failed` (same keys plus `err`). Failed attempts retry
   three times with exponential backoff (1s, 2s, 4s).
4. `build notifier` / `list channels for alert` error lines mean the
   channel record itself could not be turned into a sender — usually a
   broken `config` (wrong type shape). Recreate the channel; `config` is
   write-only and never returned by the API.
5. `POST /api/v1/channels/{id}/test` isolates the channel from the poller.
   If the test fails, jump to [A channel test fails](#a-channel-test-fails).
   If the test succeeds but live alerts do not, the monitor is not attached
   to that channel.

Do not paste `response_snippet`s that might still contain hostnames tied
to a private webhook. The dispatcher already truncates snippets at 500
bytes and never logs `config`.

## A channel test fails

`POST /api/v1/channels/{id}/test` sends a synthetic alert through the real
channel. `200 {"status":"sent"}` means the remote accepted it;
`502 {"status":"failed","error":"..."}` is the channel's error (secrets
stripped).

- Discord / Slack: the webhook URL is rejected or the channel was deleted.
  Recreate the channel with a fresh webhook.
- Telegram: `bot_token` or `chat_id` is wrong, or the bot is not in the
  chat.
- Email: SMTP host/port/credentials, or TLS policy. The error names the
  SMTP stage, not the password.
- Generic webhook: non-2xx from the receiver, or HMAC verification
  failure on their side (`X-SoroBeacon-Signature` is hex HMAC-SHA256 of
  the body under your `secret`).

The dashboard **Send test** button is the same endpoint and shows the
result inline.

## The service does not start

Startup is `config.Load` → migrate → Postgres → event source → HTTP
server. A failure here logs `msg=fatal` with `err` and exits 1.

| Symptom | Check |
| --- | --- |
| `DATABASE_URL is required` | Set `DATABASE_URL` (the only required env var). See [configuration](getting-started/configuration.md). |
| `invalid SOURCE_MODE` / `SOROTRAIL_URL is required` | `SOURCE_MODE` is `rpc` or `sorotrail`; upstream mode needs `SOROTRAIL_URL`. |
| `invalid RPC_URL` | Must be an absolute `http` or `https` URL. |
| `invalid LOG_LEVEL` | `debug`, `info`, `warn`, or `error`. |
| Network passphrase mismatch | A mainnet RPC behind testnet config (or the reverse) refuses to start. Align `NETWORK` / `RPC_URL` / `NETWORK_PASSPHRASE`. |
| Listen / bind error | Another process is already using `HTTP_ADDR` (default `:8080`). |
| Migration error | Postgres is reachable but schema apply failed. Check the URL, privileges, and that an old migration was not edited. |

`livez` never answers if the process did not finish startup.

## Database connection failures

1. `GET /api/v1/readyz` → `checks.database.healthy` is `false`; the
   `detail` is the driver error (no password).
2. `GET /api/v1/health` → `db` is not `"ok"`.
3. Logs: `database ready` never appears at startup, or later API calls
   fail with store errors.

Confirm `DATABASE_URL` (host, port, dbname, `sslmode`), that Postgres is
up (`docker compose ps`), and that the role can connect. Store
integration tests skip unless `TEST_DATABASE_URL` is set — a green
`go test ./...` without it does not prove the store paths work.

## RPC errors and rate limiting

1. `GET /api/v1/readyz` → `checks.rpc.healthy` is `false`. In `rpc` mode
   this is the Stellar RPC; in `sorotrail` mode it is the indexer.
2. `GET /api/v1/health` → `rpc` is not `"ok"`; `rpc_latest_ledger` is
   missing when the call failed.
3. Logs: `poll failed` with `err` and `retry_in`. The poller backs off
   exponentially, capped at 10× `POLL_INTERVAL`, then recovers on its own.
   `could not verify network passphrase` at startup is a warning: the
   process still starts, but you should fix `NETWORK` / `RPC_URL`.

Public Soroban RPCs rate-limit aggressive `getEvents` traffic. Symptoms
are HTTP 429 / timeout in `err`, `retry_in` climbing, and `last_poll_at`
stalling. Slow `POLL_INTERVAL` (minimum `1s`, default `5s`), point
`RPC_URL` at your own node, or switch `SOURCE_MODE=sorotrail` so several
SoroBeacon instances share one indexer.

A malformed contract ID in any monitor can make the RPC reject the whole
batched `getEvents` request. The poller logs `skipping invalid contract id`
(`monitor_id`, `contract_id`) for IDs it can detect locally.

## Raising the log level

`LOG_LEVEL` is `debug` \| `info` \| `warn` \| `error` (default `info`).
Logs are structured JSON via `log/slog`. Restart (or recreate the
container) after changing it.

Useful keys — all `lower_snake_case`:

| Key | Where it appears | Meaning |
| --- | --- | --- |
| `msg` | every line | Short event name (`fatal`, `poll failed`, `alert created`, `alert delivered`, `alert delivery failed`, …) |
| `err` / `error` | failures | The error; never includes channel `config` |
| `request_id` | API errors | Value of `X-Request-ID` (also echoed in error bodies) |
| `alert_id`, `channel_id`, `attempt` | delivery | Which alert/channel/try |
| `rule_id`, `event_id`, `monitor` | matching | Which rule fired |
| `retry_in` | poll failures | Backoff until the next ingest attempt |
| `interval` | poller started | Configured `POLL_INTERVAL` |
| `url` | upstream mode | SoroTrail indexer base URL (not a channel webhook) |

`debug` adds request-level noise; use it briefly while triaging, then
return to `info`.

## What to include in a bug report

Match [.github/ISSUE_TEMPLATE/bug_report.md](../.github/ISSUE_TEMPLATE/bug_report.md):

- **What happened** / **What you expected**.
- **How to reproduce**: monitors, rules and channels involved (redact
  secrets); the exact request or dashboard action; status code, response
  body, alert behaviour, logs.
- **Environment**: `GET /api/v1/version` output; `SOURCE_MODE` (`rpc` or
  `sorotrail`) and `NETWORK`; channel type if the issue is delivery.

Also attach, when you have them:

- `GET /api/v1/readyz` and `GET /api/v1/health` JSON.
- `GET /api/v1/stats` JSON.
- `GET /api/v1/alerts/{id}/deliveries` for a failing alert.
- A few structured log lines (`msg`, `err`, `request_id`) — still with
  secrets redacted.

Do not send channel `config`, webhook URLs, bot tokens, SMTP passwords, or
unredacted `response_snippet`s.
