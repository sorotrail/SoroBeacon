# Generic webhook

POSTs the **full structured alert as JSON** to any HTTP endpoint you control — the integration escape hatch for PagerDuty bridges, incident bots, data pipelines, anything.

## Setup

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "incident-pipe",
  "type": "webhook",
  "config": {
    "url": "https://example.com/hooks/sorobeacon",
    "secret": "shared-secret",
    "previous_secret": "the-secret-we-are-rotating-from"
  }
}'
```

## Config

| Key | Required | Description |
| --- | --- | --- |
| `url` | yes | Endpoint to POST alerts to. Treated as a secret (it often embeds tokens). |
| `secret` | yes | HMAC key that signs every request. |
| `previous_secret` | no | Second HMAC key, used only while rotating `secret`. See [Rotating the secret](#rotating-the-secret). |

## Headers

Every request carries three headers:

| Header | Example | Description |
| --- | --- | --- |
| `X-SoroBeacon-Timestamp` | `1700000000` | Decimal Unix seconds the signature covers. |
| `X-SoroBeacon-Signature` | `b7ae34…` | Hex HMAC-SHA256 over the canonical string under `secret`. |
| `X-SoroBeacon-Signature-Previous` | `d88c5d…` | Same signature under `previous_secret`. Sent only when `previous_secret` is set. |

## The canonical string

The signature covers the timestamp **and** the body, separated by a single `.`:

```
<X-SoroBeacon-Timestamp>.<raw request body>
```

For example, a delivery at `1700000000` with body `{"event_id":"e1"}` is signed over exactly these bytes:

```
1700000000.{"event_id":"e1"}
```

The digest is `HMAC-SHA256(secret, canonicalString)`, hex encoded. `internal/notify.Sign(secret, timestamp, body)` computes it.

The generic webhook sends structured JSON rather than the plain-text summary,
so it has no [`template`](templates.md) option — reshape the JSON in your
receiver instead.

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

Recompute the HMAC over the **raw bytes you received** — re-serializing the JSON first changes key order and whitespace and breaks verification. Also reject stale timestamps: the timestamp is in the signed string precisely so a captured request cannot be replayed later.

```go
const maxSkew = 5 * time.Minute

// verify reports whether r is an authentic, fresh SoroBeacon delivery.
// Call it with the raw request body before unmarshalling the JSON.
func verify(r *http.Request, body []byte, secret, previousSecret string) bool {
	ts, err := strconv.ParseInt(r.Header.Get("X-SoroBeacon-Timestamp"), 10, 64)
	if err != nil {
		return false // missing or malformed timestamp
	}
	// Replay check: the timestamp is part of the signed string, so an old
	// signature cannot be reused with a fresh timestamp.
	if time.Since(time.Unix(ts, 0)).Abs() > maxSkew {
		return false
	}

	canonical := strconv.FormatInt(ts, 10) + "." + string(body)

	// Accept either the current or the rotating-out secret. Check all
	// candidates before returning so the comparison time doesn't reveal
	// which key matched.
	ok := false
	for _, key := range []string{secret, previousSecret} {
		if key == "" {
			continue
		}
		mac := hmac.New(sha256.New, []byte(key))
		mac.Write([]byte(canonical))
		expected := hex.EncodeToString(mac.Sum(nil))
		for _, header := range []string{
			"X-SoroBeacon-Signature",
			"X-SoroBeacon-Signature-Previous",
		} {
			if hmac.Equal([]byte(expected), []byte(r.Header.Get(header))) {
				ok = true
			}
		}
	}
	return ok
}
```

{% hint style="warning" %}
Compute the HMAC over the raw bytes you received — re-serializing the JSON first will change key order/whitespace and break verification.
{% endhint %}

### Rotating the secret

To rotate without dropping deliveries:

1. Put the **current** secret in `previous_secret` and the **new** secret in `secret`. SoroBeacon now sends both signatures.
2. Update your receiver to verify against the new key (accepting either, as above).
3. Once you are satisfied, clear `previous_secret`. Only `X-SoroBeacon-Signature` is sent from then on.

## Delivery semantics

Non-2xx responses count as failures and are retried (3 attempts, exponential backoff); make your receiver idempotent on `event_id` + `rule_id`.
