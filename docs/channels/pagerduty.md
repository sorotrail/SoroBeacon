# PagerDuty

Opens incidents through the PagerDuty **Events API v2**, for on-call flows where a chat message is not enough. Deduplication is the point of this channel: without a stable dedup key, a rule that keeps firing pages a human over and over for the same condition.

## Setup

1. In PagerDuty: **Services** → your service → **Integrations** → **Add an integration** → **Events API v2**. Copy the **integration key** (the routing key).
2. Create the channel:

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "oncall-pagerduty",
  "type": "pagerduty",
  "config": {"routing_key": "R0UT1NGK3Y", "severity": "critical"}
}'
```

3. Verify:

```sh
curl -s -X POST localhost:8080/api/v1/channels/4/test
```

## Config

| Key | Required | Description |
| --- | --- | --- |
| `routing_key` | yes | Events API v2 integration key. Treated as a secret. |
| `severity` | no | One of `critical`, `error`, `warning`, `info`. Defaults to `warning`. |
| `events_url` | no | Override the Events API base URL, default `https://events.pagerduty.com`. |
| `webhook_secret` | no | PagerDuty webhook subscription secret. Enables inbound acknowledgement sync (below). Treated as a secret. |

## Payload

Each alert is sent as a `trigger` event:

```json
{
  "routing_key": "R0UT1NGK3Y",
  "event_action": "trigger",
  "dedup_key": "3:0000015985348674617345-0000000001",
  "payload": {
    "summary": "SoroBeacon alert: Token treasury",
    "source": "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA",
    "severity": "warning",
    "custom_details": {
      "monitor_name": "Token treasury",
      "rule_id": 3,
      "rule_type": "value_threshold",
      "event_name": "transfer",
      "ledger": 3721765,
      "...": "the alert payload, verbatim"
    }
  }
}
```

* `dedup_key` is deterministic for a given `(rule_id, event_id)`, mirroring SoroBeacon's own dedup guard, so a redelivered alert never opens a second incident.
* `custom_details` carries the alert payload and its identifying fields — never any channel configuration.

Non-2xx responses count as failures and are retried with backoff; the error carries the status code but never the routing key.

## Acknowledgement sync

Without sync, SoroBeacon and PagerDuty disagree: a responder acknowledges in
PagerDuty and SoroBeacon keeps escalating. With sync the two stay in step.

### Provider side setup

1. In PagerDuty: **Integrations** -> **Generic Webhooks (v3)** -> **New Webhook**.
2. Webhook URL: `https://<your-sorobeacon>/hooks/pagerduty/<workspace>/<channel-id>`
   (`default` is the workspace id on a single-tenant instance).
3. Subscribe to `incident.triggered`, `incident.acknowledged` and
   `incident.resolved`.
4. Copy the signing secret PagerDuty shows into the channel's `webhook_secret`
   (a `${secret:...}` reference works).

### Behaviour

- **Inbound.** A verified acknowledgement or resolution calls the store's own
  acknowledgement path, which stamps the alert and stops escalation in one
  transaction, exactly like a local acknowledgement. The acknowledger is
  recorded as `pagerduty:<agent name>`. SoroBeacon has no persisted "resolved"
  alert state, so a provider resolution stops escalation the same way and is
  recorded as `resolved` on the provider reference.
- **Outbound.** Acknowledging in the API or dashboard sends an
  `acknowledge` event to the incident (by dedup key). Resolution is not
  propagated outward in this version.
- **Authentication.** The `X-PagerDuty-Signature` header (`v1=` HMAC-SHA256 of
  the raw body) is verified in constant time. With no `webhook_secret`
  configured every inbound request is refused. Unknown channels, bad
  signatures and malformed paths all return the same 401. The endpoint is
  outside `/api/v1` because PagerDuty cannot present a SoroBeacon token.
- **Loops are structurally impossible.** An inbound update carries an origin in
  its context, and the outbound propagator refuses to send anything when that
  origin is set. The stored provider state is a second guard: a state the
  provider already has is not sent again.
- **Conflicts.** State only moves forward: open, acknowledged, resolved. The
  first acknowledger and time are kept. A resolution outranks an
  acknowledgement and a late or replayed event never moves an alert backwards,
  so arrival order does not decide the outcome.
- **Provider unreachable.** The local acknowledgement still succeeds and stays
  authoritative. The failure is stored on the alert's provider reference
  (`alert_external_refs.sync_error`, a short secret-free reason) and logged; the
  provider is not marked acknowledged.
- **Correlation.** `alert_external_refs` records the dedup key and the
  provider's incident id (learned from the webhook) per alert and channel.

### Limitations

- PagerDuty only. Opsgenie is not synchronised.
- The divergence from an unreachable provider is visible in the store and logs;
  there is no API or dashboard field for it yet, and no automatic retry.
- Events delivered before this version have no provider reference and are not
  synchronised.
