# Discord

Posts alert messages to a Discord channel via an incoming webhook.

## Setup

1. In Discord: **Server Settings → Integrations → Webhooks → New Webhook**, pick the target channel, copy the webhook URL.
2. Create the channel:

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "ops-discord",
  "type": "discord",
  "config": {"webhook_url": "https://discord.com/api/webhooks/1234/abcd..."}
}'
```

3. Verify:

```sh
curl -s -X POST localhost:8080/api/v1/channels/1/test
```

## Config

| Key | Required | Description |
| --- | --- | --- |
| `webhook_url` | yes | Discord incoming webhook URL. Treated as a secret. |

## Message format

Plain-text summary rendered from the shared alert template:

```
🔔 SoroBeacon alert: Token treasury
Rule: value_threshold (#3)
Contract: CA7QYNF7...UWDA
Event: transfer
Ledger: 3721765
Tx: 4f2a...
Event ID: 0000015985...-0000000001
At: 2026-07-21 09:41:32 UTC
```
