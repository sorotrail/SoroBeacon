# Slack

Posts alert messages to a Slack channel via an incoming webhook.

## Setup

1. In Slack: create an app (or use an existing one) → **Incoming Webhooks** → activate → **Add New Webhook to Workspace**, pick the channel, copy the URL.
2. Create the channel:

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "treasury-slack",
  "type": "slack",
  "config": {"webhook_url": "https://hooks.slack.com/services/T000/B000/XXXX"}
}'
```

3. Verify:

```sh
curl -s -X POST localhost:8080/api/v1/channels/2/test
```

## Config

| Key | Required | Description |
| --- | --- | --- |
| `webhook_url` | yes | Slack incoming webhook URL. Treated as a secret. |

Messages use the same plain-text alert summary as every chat channel (see [Discord](discord.md) for a sample).
