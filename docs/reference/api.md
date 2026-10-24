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

The complete credential, scope and workspace rules — including the route-by-route
scope table and the surfaces where enforcement differs (gRPC, GraphQL, metrics)
— are in the [Authorization reference](authorization.md).

{% hint style="warning" %}
An unauthenticated instance mutates monitors and channels for anyone who can
reach the port. Set `API_TOKEN` on anything beyond a trusted network.
{% endhint %}

## Rate limiting

API rate limiting uses a per-client token bucket, enabled by setting
`RATE_LIMIT_RPS` (see the [configuration guide](../configuration.md)); with rate
limiting off, no `429` is ever produced. When a client exceeds the configured
budget, the API replies with `429 Too Many Requests` and the rejected response
carries:

* `Retry-After` — the wait in whole seconds until the bucket holds a token
  again. It is derived from the bucket's actual refill time, not a fixed
  constant, rounded up to the next whole second and never `0`, so a client
  that honours it never retries into a second rejection. Retry-After in
  seconds is the interoperable form; the alternative delay format is not used.
* `RateLimit-Limit` — the bucket size (burst). `RateLimit-Remaining` — `0` on
  a rejection. `RateLimit-Reset` — the same value as `Retry-After`.

`RateLimit-*` are informational, conventional headers rather than
standards-track ones: they are not required for interoperability, but clients
that already read the same pattern from other HTTP APIs can use them. They are
sent because the limiter computes the values anyway, and they make throttling
observable without retries.

Successful responses are intentionally left unchanged — adding the headers
there is a separate feature with cache-interaction costs of its own. The header
is set only on the rejected response itself.

One other endpoint answers `429`: retrying an alert delivery while the channel
is still in its retry cooldown (`POST /alerts/{id}/retry`). That gate is not
the token bucket and does not send `Retry-After`; clients should treat
`Retry-After` as present only on token-bucket rejections.

Probes (`/health`, `/livez`, `/readyz`) are exempt from rate limiting.

## Monitors

The **Scope** column lists the scope a scoped token needs; the full
per-route table — including the role gate and the routes where a scoped
token is denied outright — is in the [Authorization reference](authorization.md#route-by-route-table).

| Method & path | Description | Scope |
| --- | --- | --- |
| `POST /monitors` | Create. Body: `name`, `contract_ids` (validated strkeys), optional `enabled`, `channel_ids`, `network`. On a multi-network instance `network` must be a chain this instance polls; omitting it selects the primary. | `monitors:write` |
| `GET /monitors` | List. `?enabled=true` filters to enabled, `?network=testnet` to one chain. | `monitors:read` |
| `GET /monitors/{id}` | Get one (includes `channel_ids`). | `monitors:read` |
| `GET /monitors/{id}/stats` | Per-monitor counts: `alerts`, `alerts_last_24h`, `alerts_last_7d`, `last_alert_at` (omitted when the monitor has never alerted), `deliveries_succeeded`, `deliveries_failed`, and `rules` — one `{rule_id, type, alerts}` entry per rule, always an array. A monitor with no history returns explicit zeroes, not nulls; an unknown id is a 404, because "this monitor has not fired" and "there is no such monitor" are different answers. | `stats:read` |
| `PATCH /monitors/{id}` | Partial update; any subset of the create fields. `channel_ids` replaces attachments. | `monitors:write` |
| `DELETE /monitors/{id}` | Soft-delete by default: the monitor moves to the trash and keeps its rules, alerts and channel links until it is restored or the retention window purges it. `?permanent=true` hard-deletes instead, cascading to rules and alerts. Both answer `204`. | `monitors:write` |
| `GET /monitors/trash` | List soft-deleted monitors, newest deletion first. Same list parameters as `GET /monitors` (`q`, `sort`, `limit`, `cursor`, `network`); returns `{"monitors": [...], "next_cursor": "..."}` with the same keyset-paging contract as `GET /monitors`. | `monitors:read` |
| `POST /monitors/{id}/soft-delete` | Move a monitor to the trash without deleting it. `204`. `404` when the monitor is unknown or already in the trash — the store matches only rows that are not yet deleted. | `monitors:write` |
| `POST /monitors/{id}/restore` | Bring a soft-deleted monitor back. `200` with the restored monitor; `404` if it is not in the trash (or no longer exists). | `monitors:write` |
| `POST /monitors/{id}/duplicate` | Copy a monitor under a unique name: contract IDs, priority, network, every rule (with its enabled flag) and its channel attachments are carried over; alerts are not, and the copy is created **disabled** — a duplicate is reviewed before it fires. `201` with the new monitor (including `channel_ids`), `404` for an unknown source. | `monitors:write` |
| `POST /monitors/bulk` | Enable or disable up to 100 monitors in one call. Body: `ids` (1–100), `enabled` (required). **Partial**: unknown ids do not stop the known ones; the response `200 {"updated": n, "unknown_ids": [...], "enabled": true}` says which ids nothing matched. There is no "all monitors" shorthand, on purpose — an accidental global disable is the failure this endpoint must not enable. | `monitors:write` |
| `POST /monitors/import` | Import a batch of contracts as one monitor each. Accepts JSON (`{ "name": "…", "contract_ids": ["C…"], "channel_ids": [1], "enabled": true, "network": "testnet" }`) or `Content-Type: text/csv` (one contract ID per line, optional `?name=` and `?network=`). At most 500 contracts. **All-or-nothing on validation**: every contract ID and the network are checked before anything is written, so a bad ID on the last row writes nothing (`400` with per-row `field` errors). Contracts already monitored on the same network are reported as `"skipped"`, not re-created. Response `200 {"total": n, "results": [{"row", "contract_id", "monitor_id", "status": "created"|"skipped"|"error", "error"}]}`. | `monitors:write` |

```sh
curl -s -X POST localhost:8080/api/v1/monitors -d '{
  "name": "My token",
  "contract_ids": ["CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"],
  "channel_ids": [1]
}'
curl -s -X PATCH localhost:8080/api/v1/monitors/1 -d '{"enabled": false}'
curl -s -X POST 'localhost:8080/api/v1/monitors/import?name=treasury&network=testnet' \
  -H 'Content-Type: text/csv' \
  --data-binary $'CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABSC4\nCD7777777777777777777777777777777777777777777777777767GY\n'
```

## Rules

| Method & path | Description | Scope |
| --- | --- | --- |
| `POST /monitors/{id}/rules` | Create. Body: `type`, `params` (validated per type), optional `enabled`, optional `severity` (`"info"` default, `"warning"`, `"critical"`). | `monitors:write` |
| `GET /monitors/{id}/rules` | List the monitor's rules. | `monitors:read` |
| `PATCH /monitors/{id}/rules/{ruleID}` | Partial update; params re-validated. | `monitors:write` |
| `DELETE /monitors/{id}/rules/{ruleID}` | Delete. | `monitors:write` |
| `POST /monitors/{id}/rules/bulk` | Create up to 50 rules on one monitor in a single request. Body: a JSON **array** of the same objects `POST /monitors/{id}/rules` takes. **All-or-nothing**: every rule is validated first and nothing is written while any entry is invalid — the `400` names each problem as `rules[i].<field>`. `201` with the array of created rules. | `monitors:write` |
| `POST /monitors/{id}/rules/dry-run` | Evaluate a candidate rule against the monitor's stored alerts using the production evaluation path. Body: `type`, `params`, optional `window_days` (default 30, clamped), optional `max_samples` (1–1000, default 100). Persists nothing and delivers nothing; `200 {"count", "matches": [{"event_id", "contract_id", "event_name", "ledger}], "total_evaluated", "note"}`. The `note` is part of the contract: stored alerts are biased toward matches, so a dry run under-represents the true event corpus. | `monitors:read` + `alerts:read` |

Params for the built-in types: [`event_emitted`](../rules/event-emitted.md), [`value_threshold`](../rules/value-threshold.md), [`token_event`](../rules/token-event.md), [`self_transfer`](../rules/self-transfer.md), [`time_window`](../rules/time-window.md), [`absence_of_event`](../rules/absence-of-event.md). Invalid params are rejected with `400` at create/update time. `GET /rule-types` returns the machine-readable schema of every registered type — its name plus a `parameters` array of field schemas — so a form or script can render the parameter list without hard-coding it.

## Escalation policies

An escalation policy says what happens when a monitor's alert is not
acknowledged: an ordered list of steps, each delaying a fixed duration before
paging a set of channels. `POST /alerts/{id}/acknowledge` stops the ladder for
that alert.

| Method & path | Description | Scope |
| --- | --- | --- |
| `GET /monitors/{id}/escalation` | Get the monitor's policy. `200 {"id", "monitor_id", "steps": [{"position", "delay", "channel_ids"}], "created_at", "updated_at"}`; `404` when the monitor has none. `delay` is a Go duration string (`"5m"`); an empty string in a step means "fire immediately". | `monitors:read` |
| `PUT /monitors/{id}/escalation` | Create **or replace** the whole policy — an ordered list is edited atomically, not step by step. Body: `{"steps": [{"delay": "5m", "channel_ids": [1, 2]}]}`. At least one step, and every step needs at least one existing channel. Delays are validated as Go durations and must not be negative. `200` with the stored policy; `400` with per-field errors (`steps[i].delay`, `steps[i].channel_ids[j]` for an unknown channel); `404` for an unknown monitor. | `monitors:write` + `channels:write` |
| `DELETE /monitors/{id}/escalation` | Remove the policy. `204`; idempotent. | `monitors:write` |

```sh
curl -s -X PUT localhost:8080/api/v1/monitors/1/escalation -d '{
  "steps": [
    {"delay": "", "channel_ids": [1]},
    {"delay": "10m", "channel_ids": [1, 2]}
  ]
}'
```

`GET /rule-types` returns the parameter schema the dashboard's rule builder renders. Each field has a `type` (`string`, `number`, `boolean`, `object`, or `select`), `required`, `description`, and, where applicable, `default` and `options`. A `boolean` field is a JSON boolean in `params`; the builder renders it as a true/false select and starts it at `default` when one is declared. `select` is reserved for genuine enumerations such as `comparison`, `direction`, `match`, and `op`.

## Monitor templates

A template is a reusable monitor shape: a name, a list of rules whose params may
hold `{{parameter}}` placeholders, the channels to attach, and the parameters
the placeholders take.

| Method & path | Description | Scope |
| --- | --- | --- |
| `POST /templates` | Create. Body: `name`, optional `description`, `rules`, `channel_ids`, `parameters`. | `templates:write` |
| `GET /templates` / `GET /templates/{id}` | List / get. | `templates:read` |
| `PATCH /templates/{id}` | Partial update; the rules are re-validated. | `templates:write` |
| `DELETE /templates/{id}` | Delete. Monitors already created from it are unaffected. | `templates:write` |
| `POST /templates/{id}/instantiate` | Create one monitor from the template. | `templates:write` + `monitors:write` |
| `POST /templates/{id}/instantiate/bulk` | Create up to 100 monitors from the template, each in its own transaction. Response `201 {"created": [{"name", "id"}, …]}` lists what landed. | `templates:write` + `monitors:write` |

A template is checked when it is written: every rule's type must be one the
instance knows, and the parts of a rule's params that do not depend on
substitution are validated. A `400` names the rule by index
(`rules[0].type`, `rules[0].params.comparison`).

Instantiation is checked before anything is written, and the monitor, its
rules and its channel links are then created in **one transaction**: a monitor
that exists has every rule it was supposed to have. A problem with the
template, the parameters or the channel ids is a `422`, and the `details`
array names each one:

* `rules[1].params.threshold` — a rule whose params do not validate once the
  parameters are substituted;
* `parameters.amount` — a required parameter with no value, a parameter the
  request names that the template does not declare, or a placeholder the
  template uses that it does not declare;
* `channel_ids[0]` — a channel that does not exist in the caller's workspace.

The bulk endpoint validates every instance before it writes any of them, so a
request with one bad instance creates nothing. Each instance is then written
in its own transaction, so an instance is never half written; the request as a
whole is not one transaction, and a failure part-way through leaves the
instances already created in place and names the instance that failed.

```sh
curl -s -X POST localhost:8080/api/v1/templates/1/instantiate -d '{
  "name": "My token",
  "contract_ids": ["CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"],
  "parameters": {"threshold": "1000"}
}'
```

## Channels

| Method & path | Description | Scope |
| --- | --- | --- |
| `POST /channels` | Create. Body: `name`, `type`, `config` (validated per type), optional `enabled`, optional `digest_mode` (`""` or `"window"`) and `digest_window_seconds` (must be > 0 when the mode is `window`). See [Digest mode](../channels/digest.md). | `channels:write` |
| `GET /channels` / `GET /channels/{id}` | List / get. **`config` is never returned.** Each channel also carries its delivery health: `consecutive_failures`, `consecutive_permanent_failures`, `last_error`, `last_error_at`, `last_success_at` and `disabled_at` (set only by auto-disable). | `channels:read` |
| `GET /channels/{id}/stats` | Delivery statistics over a recent rolling window: `{"channel_id", "total", "succeeded", "failed", "success_rate", "last_success_at", "last_failure_at"}` — the last two omitted when the window holds none of each. An unknown id is a 404. | `stats:read` |
| `PATCH /channels/{id}` | Partial update; config re-validated. Setting `enabled: true` on a channel that was off is the only way out of an auto-disable, and clears its failure counters in the same write. | `channels:write` |
| `DELETE /channels/{id}` | Delete. | `channels:write` |
| `POST /channels/{id}/test` | Send a synthetic alert through the channel right now. `200 {"status":"sent"}` or `502 {"status":"failed","error":"..."}`. | `channels:write` |
| `POST /channels/bulk` | Enable or disable up to 100 channels in one call. Body: `ids` (1–100), `enabled` (required). **Partial**, with the same contract as `POST /monitors/bulk`: unknown ids are reported in `200 {"updated", "unknown_ids", "enabled"}` and do not stop the known ones. Disabling only flips the flag — queued and in-flight deliveries are not touched. | `channels:write` |
| `POST /channels/{id}/duplicate` | Copy a channel under a unique name, credentials in `config` included (that is the point: re-typing a webhook URL to change only the room is the toil this removes). The copy is **always created disabled** — a new destination is reviewed before it receives anything — and has no monitor attachments. `201` with the new channel; the config is decrypted and re-encrypted through the store's own paths and never reaches a log. | `channels:write` |

Config shapes per type: [Discord](../channels/discord.md) · [Slack](../channels/slack.md) · [Telegram](../channels/telegram.md) · [ntfy](../channels/ntfy.md) · [Email](../channels/email.md) · [Webhook](../channels/webhook.md)

## Maintenance windows

| Method & path | Description | Scope |
| --- | --- | --- |
| `POST /maintenance-windows` | Create. Body: `reason`, `scope` (`global`/`monitor`/`contract`), `start_at`, `end_at`, plus `monitor_id` or `contract_id` for those scopes. `end_at` must be after `start_at`. | `alerts:write` |
| `GET /maintenance-windows` | List. `?active=true` filters to windows containing now; `?upcoming=true` to future windows. | `alerts:read` |
| `GET /maintenance-windows/{id}` | Get one. | `alerts:read` |
| `PATCH /maintenance-windows/{id}` | Partial update; re-validated. | `alerts:write` |
| `DELETE /maintenance-windows/{id}` | Delete. | `alerts:write` |

Alerts raised inside a window are still stored; they are marked `suppressed`
with the window's reason and not delivered. See [Maintenance windows](../guides/maintenance-windows.md).

## Inhibitions

An inhibition says "while rule A is firing, hold rule B's alerts" — the
operator's version of maintenance windows, scoped to the relationship between
two rules instead of a time range.

| Method & path | Description | Scope |
| --- | --- | --- |
| `POST /inhibitions` | Create. Body: `source_rule_id`, `target_rule_id`, optional `firing_window_seconds` (absent or ≤0 means the store's documented default). Both rules must exist; a rule inhibiting itself is `400` (it can never carry information). `201` with the stored inhibition. | `alerts:write` |
| `GET /inhibitions` | List all: `200 {"inhibitions": [{"id", "source_rule_id", "target_rule_id", "firing_window_seconds", …}]}` — always an array, never null. | `alerts:read` |
| `DELETE /inhibitions/{sourceID}/{targetID}` | Remove one inhibition by its rule pair. `204`. A malformed id is `400`. | `alerts:write` |

## Alerts

| Method & path | Description | Scope |
| --- | --- | --- |
| `GET /alerts` | History. Query: `monitor_id`, `rule_id`, `contract_id` (matches `payload.contract_id`), `q` (case-insensitive substring, matched against the alert's `event_id` and the full text of its JSON payload — which is where `contract_id`, `event_name` and every other rule-specific field live, so no per-rule-type field list is needed. `%`, `_` and `\` in the term are literal, not wildcards. Blank or whitespace-only means no search filter; over 256 characters is 400. Composes with every other filter and with `cursor`, so a searched page keeps its search while paging), `from`/`to` (RFC 3339), `sort` (`created_at_desc` default, `created_at_asc`; anything else is 400), `limit` (≤500, default 50), `cursor` (keyset: pass the previous response's `next_cursor`; comparison follows `sort`). | `alerts:read` |
| `GET /alerts/stream` | Live server-sent events feed of alerts as they are created — see [Streaming](#streaming-alerts-sse) below. | `alerts:read` |
| `GET /alerts.csv` | CSV export of the same filtered alerts (same query params as `GET /alerts`; `cursor` is ignored). Responds `text/csv` with an attachment filename carrying the requested date range. Columns, in order: `id`, `monitor_name`, `rule_id`, `contract_id`, `event_name`, `event_id`, `ledger`, `created_at` (RFC 3339), `payload` (the raw JSON). Values beginning with `=`, `+`, `-` or `@` are prefixed with an apostrophe so spreadsheet software treats them as text, not formulas. With no `limit` the export is capped at 10000 rows; an explicit `limit` is honoured up to that cap. | `alerts:read` |
| `GET /alerts/export` | NDJSON export of the same filtered alerts — see [NDJSON export](#ndjson-export) below. | `alerts:read` |
| `GET /alerts/{id}/deliveries` | Delivery attempts for one alert. `?status=success` or `?status=failed` filters in SQL; omit for all. Unknown values are `400`. | `alerts:read` |
| `POST /alerts/{id}/deliveries/{channelID}/retry` | Send this alert through this channel again. It records a **new** attempt rather than mutating the failed one, and answers `200` with the attempt (its delivery status and a sanitized `response_snippet`). `409` when the pair already succeeded (nothing to retry), `429` while the pair is still inside the retry cooldown (the body says how long), `400` when the channel is disabled, `404` for an unknown alert, channel, or one with no recorded attempt. | `alerts:write` |
| `POST /alerts/{id}/acknowledge` | Acknowledge an alert: stops any escalation attached to it so the on-call engineer who has seen the page is not escalated on top of it. `200` with the updated alert; `404` for an unknown id. | `alerts:write` |

```sh
curl -s 'localhost:8080/api/v1/alerts?monitor_id=1&from=2026-07-01T00:00:00Z&limit=20'
# {"alerts": [...], "next_cursor": "42"}
```

### Streaming alerts (SSE)

`GET /alerts/stream` pushes each new alert as a Server-Sent Event. SSE rather
than WebSockets because the traffic is one-directional and SSE reconnects on
its own through proxies with far less configuration.

```sh
curl -sN 'localhost:8080/api/v1/alerts/stream?monitor_id=1'
: connected

retry: 3000

event: alert
data: {"id":42,"monitor_id":1,"rule_id":7,"event_id":"…","payload":{…},…}

: keep-alive

```

* **Framing.** Every event is `event: alert` followed by one `data:` line
  holding the alert as compact JSON — the same object `GET /alerts` returns.
  The stream opens with a `: connected` comment (so `EventSource.onopen` fires
  without waiting for the first alert) and a `retry: 3000` directive telling
  browsers to wait 3 s before reconnecting instead of hammering a restarting
  server. An idle connection emits a `: keep-alive` comment every 15 seconds —
  inside the default idle timeout of nginx and most load balancers, so a quiet
  watch is not reaped mid-connection.
* **Filtering.** `?monitor_id=` filters server-side; a client watching one
  monitor is not woken for every other monitor's alerts. A malformed id is a
  normal `400` JSON envelope, before any bytes of the stream are written.
* **Resume.** The server does not track `Last-Event-ID` and does not replay:
  an event published while the client was disconnected is gone from *that
  connection* (it is still in `GET /alerts`). `EventSource` reconnects
  automatically — or, if it never received the `retry` hint, immediately — and
  the next event arrives live. A client that needs no gaps reads
  `GET /alerts?cursor=` from the last id it saw and uses the stream only for
  immediacy.
* **Disconnect.** When the client goes away the request context is cancelled
  and the server unregisters the subscriber in the same exit path, so no
  goroutine or buffer is left behind. A slow subscriber costs at most a
  dropped event from *its* buffer — alert creation is never blocked by a dead
  connection. Headers: `Content-Type: text/event-stream`,
  `Cache-Control: no-store`, `X-Accel-Buffering: no` (asks nginx not to
  buffer). A response writer without `Flush` support answers `500`
  `"streaming unsupported"` before any event.

### NDJSON export

`GET /alerts/export` streams the same filtered set as `GET /alerts` in
`application/x-ndjson`: one JSON object per line, newline-terminated, flushed
in pages of 500 rows.

* **Line shape.** Each line is
  `{"id","monitor_id","monitor_name","rule_id","event_id","event_name","contract_id","ledger","created_at","payload"}`
  — strings for `id`/`monitor_id`/`rule_id`/`ledger`, `created_at` in RFC 3339,
  `payload` the raw JSON as a string. A **federated** alert (delivered by
  `POST /ingest` from another instance) additionally carries `federated:true`,
  `federation_origin`, `source_monitor_id` and `source_rule_id` (ids belonging
  to the sending instance), and its `monitor_name` reads
  `federated:<origin>` — it has no local monitor.
* **Parameters.** Identical to `GET /alerts` (`monitor_id`, `rule_id`, `q`,
  `from`, `to`, `sort`, `limit`, `cursor`); without an explicit `limit` the
  export is capped at 10000 rows.
* **Resume.** Rows are paged, not snapshotted, so an alert inserted
  mid-export may or may not appear. To continue an interrupted export, send
  the same query with `from` set to the newest `created_at` you received.
* **Disconnect.** If the client goes away the store walk stops at the next
  page instead of reading on into a connection nobody reads, and the server
  logs the row count. Lines already sent are complete — each row is one
  `Write` of `row + "\n"` — so a reader never sees half a JSON object.
  `Cache-Control: no-store` and `X-Request-ID` are set on every export.

## Dead letters

A delivery that exhausts its automatic retries is kept as a dead letter so
it can be redriven after the destination is fixed. The operator procedure,
retention rules and the difference between a retry and a redrive are in
[Diagnosing failed alert deliveries](../operations/delivery-failures.md#dead-letter-queue-and-redrive).

| Method & path | Description | Scope |
| --- | --- | --- |
| `GET /dead-letters` | List the queue in id order. Query: `channel_id`, `alert_id`, `limit` (default 50), `after_id` (keyset cursor). | `alerts:read` |
| `POST /dead-letters/{id}/redrive` | Send the stored alert to the stored channel once. | `alerts:write` |

Each dead letter has `id`, `alert_id`, `channel_id`, `last_error` (sanitized),
`attempt_count`, `last_status` (HTTP status, or `0` for a transport error) and
`created_at`.

Redrive is a single delivery attempt, not the automatic retry loop. A
successful redrive returns `204`, records a successful delivery attempt and
deletes the dead-letter row. A failed send returns `502` and leaves the row in
the queue. An unknown dead letter, alert or channel is `404`; a malformed id
is `400`. If the alert/channel pair already has a successful attempt the API
returns `409` and keeps the row. A channel whose config cannot build a
notifier returns `500`.

The API path does not check whether the channel is enabled; the dashboard
path does and returns `502` until the channel is re-enabled. That difference
is documented in the [operations guide](../operations/delivery-failures.md#known-difference-between-the-api-and-dashboard-redrive-paths).

## Tokens

Scoped credentials for the API itself: what to hand a CI job so it can create a
monitor without being able to delete every channel.

| Method & path | Description | Scope |
| --- | --- | --- |
| `POST /tokens` | Mint. Body: `scopes` (required, at least one), optional `name`, optional `expires_in` (a Go duration, `72h`). Returns `201` with the token fields **and `token`** — the raw secret, which appears in no other response ever. | `tokens:write` |
| `GET /tokens` | List this workspace's tokens: `id`, `name`, `prefix` (`sb_` plus eight characters), `scopes`, `created_at`, `expires_at`, `last_used_at`, `revoked_at`. Never the secret, and never the stored digest. | `tokens:read` |
| `POST /tokens/{id}/revoke` | Revoke now. `204`; the row survives with `revoked_at` set, so the audit trail outlives the credential. Revoking twice keeps the first timestamp. Another workspace's id gets the same `404` as a nonexistent one. | `tokens:write` |

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

| Method & path | Description | Scope |
| --- | --- | --- |
| `GET /health` | Checks the database and the RPC. `200` when both are ok, `503` with per-dependency detail when degraded. Adds `leader`, `leader_election` and (while polling) `leader_since` so an operator can see which replica holds the poller lease; a follower is still `200`, because it serves everything except polling. On a multi-network instance it gains a `networks` array (processed ledger, chain tip, lag, last poll, per-chain errors) and degrades when any polled chain degrades. | no credential |
| `GET /livez` | "Is the process alive" — checks nothing, on purpose: a process whose database and RPC are both down is still alive, and a liveness probe that failed on dependencies would cause restart loops instead of traffic removal. Always `200 {"status":"alive"}` while the process serves. | no credential |
| `GET /readyz` | "Can this instance do useful work right now" — checks the database and the RPC concurrently, each with a 3 s bound, plus one `rpc_<network>` check per polled chain on a multi-network instance, and answers `503` with `checks` naming the one that is down (`{"status":"not ready","checks":{"database":{"healthy":false,"detail":"…"}}}`). When a ready-lag threshold is configured it adds a `poller` check that fails once ledger lag exceeds it (a not-yet-polled instance is "waiting for first poll", not a failure). Note it reports dependency error strings to anyone who can reach the port — keep it off the public internet. | no credential |
| `GET /poller` | Last successful ingest position without exposing any configuration: `200 {"status":"ok", "last_processed_ledger", "latest_chain_ledger", "ledger_lag", "last_successful_poll" (RFC 3339), "backing_off"}`. Before the first successful poll it answers `200 {"status":"waiting", "backing_off":…}`; with no poller wired (a pure API replica) `503 {"status":"waiting"}`. | `none` |
| `GET /version` | Build information (version, commit, build date). The one route a scoped token may call without holding any scope. | `none` |
| `GET /rule-types` | Machine-readable vocabulary of rule types: `200 {"rule_types": [{"type", "parameters": [{"name", "type", …}]}, …]}`, sorted by type — the schema `POST /monitors/{id}/rules` validates against. | `none` |
| `GET /stats` | Counts (monitors, rules, channels, alerts, alerts last 24h), last ingested ledger, last poll time. | `stats:read` |
| `GET /stats/alerts-daily` | Daily alert counts for the last 30 UTC calendar days. Quiet days are explicit zeroes. `{"timezone":"UTC","days":[{"day":"2026-09-01","count":0}, ...]}`. | `alerts:read` |
| `GET /audit` | Append-only log of monitor, rule and channel changes, newest first. Query: `target_type` (`monitor`\|`rule`\|`channel`), `target_id`, `from`/`to` (RFC 3339), `limit` (≤500, default 50). Each entry has `actor` (the request ID), `action` (`create`\|`update`\|`delete`), `target_type`, `target_id`, `diff` and `created_at`. `diff` records the *names* of the fields that were sent, never their values — a channel's config holds webhook URLs and tokens and is never stored. There is no endpoint to update or delete entries. | `monitors:read` |

## Federation

| Method & path | Description | Scope |
| --- | --- | --- |
| `POST /ingest` | Receives an alert forwarded by another SoroBeacon instance and stores it without re-evaluating rules: the sending instance already decided, and re-evaluating would duplicate alerts. | `alerts:write` |

The request body is a `notify.FederatedAlert`: `origin` (required,
non-empty), `hop_count` (an integer; at the hop limit the alert is dropped
with a `400` to break forwarding loops) and `alert` — `event_id` (required),
`payload`, `severity` (`info`/`warning`/`critical`, empty meaning `warning`),
and the sender's `monitor_id`/`rule_id`, which are kept only as *source*
ids.

The monitor and rule ids in the payload belong to the **sending** instance.
They are kept as the stored alert's `source_monitor_id` and `source_rule_id`,
and the alert itself is attached to no local monitor and no local rule — its
own `monitor_id` and `rule_id` are zero, and `federated` is true. It lands in
the workspace of the credential that delivered it, never in one the payload
names. A payload that names ids this instance happens to have does not
attach itself to them.

* `201` — the alert was stored (`outcome` is `created` or `duplicate`).
* `400` — the payload is malformed: no `event_id`, no `origin`, a severity
  outside the vocabulary, or a hop count at the limit.
* `422` — the payload cannot be stored here: it violates a database
  constraint. It is a client error, and the sender must not retry it.
* `500` — the receiver's own fault. The sender may retry.
