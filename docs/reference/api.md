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
* With `API_TOKEN` unset the API is open, exactly as it was before
  authentication existed, and the process logs one warning at startup.

{% hint style="warning" %}
An unauthenticated instance mutates monitors and channels for anyone who can
reach the port. Set `API_TOKEN` on anything beyond a trusted network.
{% endhint %}

## Monitors

| Method & path | Description |
| --- | --- |
| `POST /monitors` | Create. Body: `name`, `contract_ids` (validated strkeys), optional `enabled`, `channel_ids`. |
| `GET /monitors` | List. `?enabled=true` filters to enabled. |
| `GET /monitors/{id}` | Get one (includes `channel_ids`). |
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

Params for the built-in types: [`event_emitted`](../rules/event-emitted.md), [`value_threshold`](../rules/value-threshold.md). Invalid params are rejected with `400` at create/update time.

## Channels

| Method & path | Description |
| --- | --- |
| `POST /channels` | Create. Body: `name`, `type`, `config` (validated per type), optional `enabled`, optional `digest_mode` (`""` or `"window"`) and `digest_window_seconds` (must be > 0 when the mode is `window`). See [Digest mode](../channels/digest.md). |
| `GET /channels` / `GET /channels/{id}` | List / get. **`config` is never returned.** |
| `PATCH /channels/{id}` | Partial update; config re-validated. |
| `DELETE /channels/{id}` | Delete. |
| `POST /channels/{id}/test` | Send a synthetic alert through the channel right now. `200 {"status":"sent"}` or `502 {"status":"failed","error":"..."}`. |

Config shapes per type: [Discord](../channels/discord.md) · [Slack](../channels/slack.md) · [Telegram](../channels/telegram.md) · [Email](../channels/email.md) · [Webhook](../channels/webhook.md)

## Alerts

| Method & path | Description |
| --- | --- |
| `GET /alerts` | History. Query: `monitor_id`, `rule_id`, `contract_id` (matches `payload.contract_id`), `from`/`to` (RFC 3339), `sort` (`created_at_desc` default, `created_at_asc`; anything else is 400), `limit` (≤500, default 50), `cursor` (keyset: pass the previous response's `next_cursor`; comparison follows `sort`). |
| `GET /alerts.csv` | CSV export of the same filtered alerts (same query params as `GET /alerts`; `cursor` is ignored). Responds `text/csv` with an attachment filename carrying the requested date range. Columns, in order: `id`, `monitor_name`, `rule_id`, `contract_id`, `event_name`, `event_id`, `ledger`, `created_at` (RFC 3339), `payload` (the raw JSON). Values beginning with `=`, `+`, `-` or `@` are prefixed with an apostrophe so spreadsheet software treats them as text, not formulas. With no `limit` the export is capped at 10000 rows; an explicit `limit` is honoured up to that cap. |
| `GET /alerts/{id}/deliveries` | Delivery attempts for one alert. `?status=success` or `?status=failed` filters in SQL; omit for all. Unknown values are `400`. |

```sh
curl -s 'localhost:8080/api/v1/alerts?monitor_id=1&from=2026-07-01T00:00:00Z&limit=20'
# {"alerts": [...], "next_cursor": "42"}
```

## Operational

| Method & path | Description |
| --- | --- |
| `GET /health` | Checks Postgres and the RPC. `200` when both are ok, `503` with per-dependency detail when degraded. |
| `GET /stats` | Counts (monitors, rules, channels, alerts, alerts last 24h), last ingested ledger, last poll time. |
| `GET /stats/alerts-daily` | Daily alert counts for the last 30 UTC calendar days. Quiet days are explicit zeroes. `{"timezone":"UTC","days":[{"day":"2026-09-01","count":0}, ...]}`. |
| `GET /audit` | Append-only log of monitor, rule and channel changes, newest first. Query: `target_type` (`monitor`\|`rule`\|`channel`), `target_id`, `from`/`to` (RFC 3339), `limit` (≤500, default 50). Each entry has `actor` (the request ID), `action` (`create`\|`update`\|`delete`), `target_type`, `target_id`, `diff` and `created_at`. `diff` records the *names* of the fields that were sent, never their values — a channel's config holds webhook URLs and tokens and is never stored. There is no endpoint to update or delete entries. |
