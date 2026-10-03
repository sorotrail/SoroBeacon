# HTTP API

Base path: `/api/v1`. All bodies are JSON. Errors return `{"error": "..."}` with an appropriate status code.

## Authentication

With `API_TOKEN` set, every endpoint below requires
`Authorization: Bearer <token>` and answers `401` through the normal error
envelope without it:

```sh
curl -s localhost:8080/api/v1/monitors -H "Authorization: Bearer $API_TOKEN"
```

* The tokens are the comma-separated values in `API_TOKEN`; any one of them is
  accepted, which is what makes rotation possible without downtime. Comparison
  is constant-time, and a missing header, a malformed header and a wrong token
  all return the same `401 {\"error\":\"unauthorized\"}` — the response never
  says which of those it was.
* **Probes are exempt**: `GET /health`, `GET /livez` and `GET /readyz` require
  no token, so an authenticated deployment cannot fail its own health checks
  (or the docker-compose healthcheck). The trade-off is that `/readyz` reports
  per-dependency detail — including dependency error strings — to anyone who
  can reach the port. Do not expose those paths to the public internet.
* A dashboard session cookie from `/login` is accepted too, so the dashboard's
  own same-origin links (`/alerts.csv`, for one) work in a browser. A script
  should send the bearer header instead.
* A bearer token may also be a **scoped token** minted by `POST /tokens`
  (recognisable by its `sb_` prefix). It is checked against the route's scope
  list, so it can be limited to one resource and revoked or expired without a
  restart — see [Tokens](#tokens). Static `API_TOKEN` values and dashboard
  sessions are unrestricted and pass every route.
* With `API_TOKEN` unset the API is open, exactly as it was before
  authentication existed, and the process logs one warning at startup.

{% hint style="warning" %}
An unauthenticated instance mutates monitors and channels for anyone who can
reach the port. Set `API_TOKEN` on anything beyond a trusted network.
{% endhint %}

## Monitors

| Method & path | Description |
| --- | --- |
| `POST /monitors` | Create. Body: `name`, `contract_ids` (validated strkeys), optional `enabled`, `channel_ids`, `network`. On a multi-network instance `network` must be a chain this instance polls; omitting it selects the primary. |
| `GET /monitors` | List. `?enabled=true` filters to enabled, `?network=testnet` to one chain. |
| `GET /monitors/{id}` | Get one (includes `channel_ids`). |
| `GET /monitors/{id}/stats` | Per-monitor counts: `alerts`, `alerts_last_24h`, `alerts_last_7d`, `last_alert_at` (omitted when the monitor has never alerted), `deliveries_succeeded`, `deliveries_failed`, and `rules` — one `{rule_id, type, alerts}` entry per rule, always an array. A monitor with no history returns explicit zeroes, not nulls; an unknown id is a 404, because "this monitor has not fired" and "there is no such monitor" are different answers. |
| `PATCH /monitors/{id}` | Partial update; any subset of the create fields. `channel_ids` replaces attachments. |
| `DELETE /monitors/{id}` | Delete (cascades to rules and alerts). |

```sh
curl -s -X POST localhost:8080/api/v1/monitors -d '{
  "name": "My token",
  "contract_ids": ["CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"],
  "channel_ids": [1]
}'
curl -s -X PATCH localhost:8080/api/v1/monitors/1 -d '{"enabled": false}'
```

## Rules

| Method & path | Description |
| --- | --- |
| `POST /monitors/{id}/rules` | Create. Body: `type`, `params` (validated per type), optional `enabled`. |
| `GET /monitors/{id}/rules` | List the monitor's rules. |
| `PATCH /monitors/{id}/rules/{ruleID}` | Partial update; params re-validated. |
| `DELETE /monitors/{id}/rules/{ruleID}` | Delete. |

Params for the built-in types: [`event_emitted`](../rules/event-emitted.md), [`value_threshold`](../rules/value-threshold.md), [`token_event`](../rules/token-event.md), [`self_transfer`](../rules/self-transfer.md), [`time_window`](../rules/time-window.md), [`absence_of_event`](../rules/absence-of-event.md). Invalid params are rejected with `400` at create/update time.

## Channels

| Method & path | Description |
| --- | --- |
| `POST /channels` | Create. Body: `name`, `type`, `config` (validated per type), optional `enabled`, optional `digest_mode` (`""` or `"window"`) and `digest_window_seconds` (must be > 0 when the mode is `window`). See [Digest mode](../channels/digest.md). |
| `GET /channels` / `GET /channels/{id}` | List / get. **`config` is never returned.** Each channel also carries its delivery health: `consecutive_failures`, `consecutive_permanent_failures`, `last_error`, `last_error_at`, `last_success_at` and `disabled_at` (set only by auto-disable). |
| `PATCH /channels/{id}` | Partial update; config re-validated. Setting `enabled: true` on a channel that was off is the only way out of an auto-disable, and clears its failure counters in the same write. |
| `DELETE /channels/{id}` | Delete. |
| `POST /channels/{id}/test` | Send a synthetic alert through the channel right now. `200 {"status":"sent"}` or `502 {"status":"failed","error":"..."}`. |

Config shapes per type: [Discord](../channels/discord.md) · [Slack](../channels/slack.md) · [Telegram](../channels/telegram.md) · [ntfy](../channels/ntfy.md) · [Email](../channels/email.md) · [Webhook](../channels/webhook.md)

## Maintenance windows

| Method & path | Description |
| --- | --- |
| `POST /maintenance-windows` | Create. Body: `reason`, `scope` (`global`/`monitor`/`contract`), `start_at`, `end_at`, plus `monitor_id` or `contract_id` for those scopes. `end_at` must be after `start_at`. |
| `GET /maintenance-windows` | List. `?active=true` filters to windows containing now; `?upcoming=true` to future windows. |
| `GET /maintenance-windows/{id}` | Get one. |
| `PATCH /maintenance-windows/{id}` | Partial update; re-validated. |
| `DELETE /maintenance-windows/{id}` | Delete. |

Alerts raised inside a window are still stored; they are marked `suppressed`
with the window's reason and not delivered. See [Maintenance windows](../guides/maintenance-windows.md).

## Alerts

| Method & path | Description |
| --- | --- |
| `GET /alerts` | History. Query: `monitor_id`, `rule_id`, `contract_id` (matches `payload.contract_id`), `q` (case-insensitive substring, matched against the alert's `event_id` and the full text of its JSON payload — which is where `contract_id`, `event_name` and every other rule-specific field live, so no per-rule-type field list is needed. `%`, `_` and `\` in the term are literal, not wildcards. Blank or whitespace-only means no search filter; over 256 characters is 400. Composes with every other filter and with `cursor`, so a searched page keeps its search while paging), `from`/`to` (RFC 3339), `sort` (`created_at_desc` default, `created_at_asc`; anything else is 400), `limit` (≤500, default 50), `cursor` (keyset: pass the previous response's `next_cursor`; comparison follows `sort`). |
| `GET /alerts.csv` | CSV export of the same filtered alerts (same query params as `GET /alerts`; `cursor` is ignored). Responds `text/csv` with an attachment filename carrying the requested date range. Columns, in order: `id`, `monitor_name`, `rule_id`, `contract_id`, `event_name`, `event_id`, `ledger`, `created_at` (RFC 3339), `payload` (the raw JSON). Values beginning with `=`, `+`, `-` or `@` are prefixed with an apostrophe so spreadsheet software treats them as text, not formulas. With no `limit` the export is capped at 10000 rows; an explicit `limit` is honoured up to that cap. |
| `GET /alerts/{id}/deliveries` | Delivery attempts for one alert. `?status=success` or `?status=failed` filters in SQL; omit for all. Unknown values are `400`. |

```sh
curl -s 'localhost:8080/api/v1/alerts?monitor_id=1&from=2026-07-01T00:00:00Z&limit=20'
# {"alerts": [...], "next_cursor": "42"}
```

## Tokens

Scoped credentials for the API itself: what to hand a CI job so it can create a
monitor without being able to delete every channel.

| Method & path | Description |
| --- | --- |
| `POST /tokens` | Mint. Body: `scopes` (required, at least one), optional `name`, optional `expires_in` (a Go duration, `72h`). Returns `201` with the token fields **and `token`** — the raw secret, which appears in no other response ever. |
| `GET /tokens` | List this workspace's tokens: `id`, `name`, `prefix` (`sb_` plus eight characters), `scopes`, `created_at`, `expires_at`, `last_used_at`, `revoked_at`. Never the secret, and never the stored digest. |
| `POST /tokens/{id}/revoke` | Revoke now. `204`; the row survives with `revoked_at` set, so the audit trail outlives the credential. Revoking twice keeps the first timestamp. Another workspace's id gets the same `404` as a nonexistent one. |

```sh
curl -s -X POST localhost:8080/api/v1/tokens -H "Authorization: Bearer $API_TOKEN" \
  -d '{"name":"ci-deploy","scopes":["monitors:read","monitors:write"],"expires_in":"720h"}'
# {"id":3,"name":"ci-deploy","prefix":"sb_3f9a2b71","scopes":["monitors:read","monitors:write"],
#  "created_at":"2026-09-26T10:00:00Z","expires_at":"2026-10-26T10:00:00Z","last_used_at":null,"revoked_at":null,
#  "token":"sb_3f9a2b71Qm8…"}
```

Scopes are `<resource>:<read|write>` over `monitors`, `channels`, `alerts`,
`templates`, `stats` (read only) and `tokens`; `write` implies `read`
on the same resource, and `*` is every scope. Only resources the API serves have
scopes — saved searches live on the dashboard, which a scoped token cannot sign
into, so a `searches:read` scope would grant nothing.
`POST /templates/{id}/instantiate`
requires both `templates:write` and `monitors:write`, since it creates monitors.
An unknown or misspelled scope is a `400` naming the entry, not a silently
dropped one.

Three behaviours worth knowing before you script against this:

* `403` means the credential is valid and not allowed. `GET /version` is the one
  route a scoped token may call without a scope; an unmapped route denies a
  scoped token, so a scope list never silently gains access to a future
  endpoint.
* A scoped token can only mint tokens carrying scopes it already holds
  (`403` otherwise), so `tokens:write` is not a route back to `*`.
* Only the digest of the secret is stored, so a lost token cannot be recovered
  from the database or the dashboard — mint another and revoke the first.
  `last_used_at` is written at most once a minute per token; treat it as
  "recently used", not as an exact audit timestamp.

## Operational

| Method & path | Description |
| --- | --- |
| `GET /health` | Checks the database and the RPC. `200` when both are ok, `503` with per-dependency detail when degraded. Adds `leader`, `leader_election` and (while polling) `leader_since` so an operator can see which replica holds the poller lease; a follower is still `200`, because it serves everything except polling. |
| `GET /health` | Checks Postgres and the RPC. `200` when both are ok, `503` with per-dependency detail when degraded. On a multi-network instance it gains a `networks` array (processed ledger, chain tip, lag, last poll, per-chain errors) and degrades when any polled chain degrades. |
| `GET /stats` | Counts (monitors, rules, channels, alerts, alerts last 24h), last ingested ledger, last poll time. |
| `GET /stats/alerts-daily` | Daily alert counts for the last 30 UTC calendar days. Quiet days are explicit zeroes. `{"timezone":"UTC","days":[{"day":"2026-09-01","count":0}, ...]}`. |
| `GET /audit` | Append-only log of monitor, rule and channel changes, newest first. Query: `target_type` (`monitor`\|`rule`\|`channel`), `target_id`, `from`/`to` (RFC 3339), `limit` (≤500, default 50). Each entry has `actor` (the request ID), `action` (`create`\|`update`\|`delete`), `target_type`, `target_id`, `diff` and `created_at`. `diff` records the *names* of the fields that were sent, never their values — a channel's config holds webhook URLs and tokens and is never stored. There is no endpoint to update or delete entries. |
