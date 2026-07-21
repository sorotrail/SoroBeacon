# HTTP API

Base path: `/api/v1`. All bodies are JSON. Errors return `{"error": "..."}` with an appropriate status code.

{% hint style="warning" %}
No authentication in the MVP — treat the API as a trusted-network interface.
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
| `POST /channels` | Create. Body: `name`, `type`, `config` (validated per type), optional `enabled`. |
| `GET /channels` / `GET /channels/{id}` | List / get. **`config` is never returned.** |
| `PATCH /channels/{id}` | Partial update; config re-validated. |
| `DELETE /channels/{id}` | Delete. |
| `POST /channels/{id}/test` | Send a synthetic alert through the channel right now. `200 {"status":"sent"}` or `502 {"status":"failed","error":"..."}`. |

Config shapes per type: [Discord](../channels/discord.md) · [Slack](../channels/slack.md) · [Telegram](../channels/telegram.md) · [Email](../channels/email.md) · [Webhook](../channels/webhook.md)

## Alerts

| Method & path | Description |
| --- | --- |
| `GET /alerts` | History, newest first. Query: `monitor_id`, `from`/`to` (RFC 3339), `limit` (≤500, default 50), `cursor` (keyset: pass the previous response's `next_cursor`). |
| `GET /alerts/{id}/deliveries` | Every delivery attempt for one alert. |

```sh
curl -s 'localhost:8080/api/v1/alerts?monitor_id=1&from=2026-07-01T00:00:00Z&limit=20'
# {"alerts": [...], "next_cursor": "42"}
```

## Operational

| Method & path | Description |
| --- | --- |
| `GET /health` | Checks Postgres and the RPC. `200` when both are ok, `503` with per-dependency detail when degraded. |
| `GET /stats` | Counts (monitors, rules, channels, alerts, alerts last 24h), last ingested ledger, last poll time. |
