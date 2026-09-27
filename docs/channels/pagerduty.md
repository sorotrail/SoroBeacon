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
