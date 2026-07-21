# Generic webhook

POSTs the **full structured alert as JSON** to any HTTP endpoint you control — the integration escape hatch for PagerDuty bridges, incident bots, data pipelines, anything.

## Setup

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "incident-pipe",
  "type": "webhook",
  "config": {"url": "https://example.com/hooks/sorobeacon", "secret": "shared-secret"}
}'
```

## Config

| Key | Required | Description |
| --- | --- | --- |
| `url` | yes | Endpoint to POST alerts to. Treated as a secret (it often embeds tokens). |
| `secret` | yes | HMAC key used to sign every request. |

## Payload

```json
{
  "id": 42,
  "monitor_id": 1,
  "monitor_name": "Token treasury",
  "rule_id": 3,
  "rule_type": "value_threshold",
  "event_id": "0000015985348674617345-0000000001",
  "contract_id": "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA",
  "event_name": "transfer",
  "ledger": 3721765,
  "tx_hash": "4f2a...",
  "payload": { "topics": ["transfer", "..."], "value": "..." },
  "created_at": "2026-07-21T09:41:32Z"
}
```

## Verifying the signature

Every request carries an `X-SoroBeacon-Signature` header: the **hex HMAC-SHA256 of the raw request body** under your `secret`. Reject anything that doesn't verify.

```go
func verify(secret string, body []byte, header string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(header))
}
```

{% hint style="warning" %}
Compute the HMAC over the raw bytes you received — re-serializing the JSON first will change key order/whitespace and break verification.
{% endhint %}

Non-2xx responses count as failures and are retried (3 attempts, exponential backoff); make your receiver idempotent on `event_id` + `rule_id`.
