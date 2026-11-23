# Google Chat

Posts alert messages to a Google Chat space via an incoming webhook, using the **cardsV2** format for a structured, readable message.

## Setup

1. In Google Chat: open the target space → **Manage webhooks** → **Add webhook**. Give it a name and copy the webhook URL.
   The URL contains a `key` and `token` query parameter — treat it as a secret.
2. Create the channel:

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "ops-google-chat",
  "type": "googlechat",
  "config": {"webhook_url": "https://chat.googleapis.com/v1/spaces/AAAA/messages?key=...&token=..."}
}'
```

3. Verify:

```sh
curl -s -X POST localhost:8080/api/v1/channels/1/test
```

## Config

| Key | Required | Description |
| --- | --- | --- |
| `webhook_url` | yes | Google Chat incoming webhook URL. **Treated as a secret** — never logged or returned by the API. Must use HTTPS. |

## Message format

Each alert is sent as a `cardsV2` message with the following fields:

```
┌─────────────────────────────────────┐
│ SoroBeacon Alert                    │
│ Token treasury                      │
├─────────────────────────────────────┤
│ Monitor      │ Token treasury       │
│ Event        │ transfer             │
│ Rule Type    │ event_emitted        │
│ Contract ID  │ CA7QYNF7...UWDA      │
│ Event ID     │ 0000015985...-00001  │
│ Timestamp    │ 2026-07-21 09:41:32  │
└─────────────────────────────────────┘
```

The card carries:
- **Monitor** — the monitor name
- **Event** — the event name (when available)
- **Rule Type** — the rule type that fired
- **Contract ID** — the Soroban contract ID
- **Event ID** — the unique event identifier
- **Timestamp** — when the alert was created (UTC)

Non-2xx responses count as failures and are retried with backoff; the error carries the status code but never the webhook URL.

## Security

The webhook URL embeds a `key` and `token` that grant permission to post to the space. SoroBeacon:
- Validates that the URL uses HTTPS at channel creation time
- Never logs the webhook URL
- Never includes the URL in error messages or delivery response snippets
- Stores the URL encrypted at rest when `CONFIG_ENCRYPTION_KEY` is set