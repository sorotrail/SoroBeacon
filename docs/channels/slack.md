# Slack

Posts alert messages to a Slack channel via an incoming webhook.

## Overview

SoroBeacon sends alerts to Slack using Slack's [Incoming Webhooks](https://api.slack.com/messaging/webhooks). Each webhook URL is tied to a specific Slack app and target channel. If you need alerts delivered to a different channel, you must create a new webhook URL for that channel.

## Prerequisites

1. A Slack workspace where you have permission to add apps/incoming webhooks.
2. A target Slack channel (public or private) where the webhook will post messages.

## Setup

### 1. Create a Slack app and generate an incoming webhook URL

1. Open [api.slack.com/apps](https://api.slack.com/apps) and select **Create New App** → **From scratch**.
2. Name the app and select the workspace that contains the alert destination.
3. Open **Incoming Webhooks** in the app settings and switch **Activate Incoming Webhooks** on.
4. Select **Add New Webhook to Workspace**. In Slack's authorization screen, choose the exact destination channel and select **Allow**. If the channel is private, the app must be permitted to post there.
5. Copy the generated webhook URL and store it in a secret manager or password manager.

> **Security warning:** The webhook URL is a secret bound to the channel selected during authorization. It cannot be redirected by changing a SoroBeacon setting; a different destination requires creating a new webhook for that channel. Do not paste the URL into logs, tickets, shell history, or source control. If it is exposed, revoke it in the Slack app settings and create a replacement.

### 2. Create the Slack channel in SoroBeacon

The command below reads the webhook URL without displaying it, then sends the request body to `curl` over standard input. The API response contains the new channel ID, not its configuration.

```sh
read -r -s -p 'Slack webhook URL: ' SLACK_WEBHOOK_URL
printf '\n'
curl -sS -X POST http://localhost:8080/api/v1/channels \
  -H 'Content-Type: application/json' \
  --data-binary @- <<JSON
{
  "name": "alerts-slack",
  "type": "slack",
  "config": {"webhook_url": "${SLACK_WEBHOOK_URL}"}
}
JSON
unset SLACK_WEBHOOK_URL
```

### 3. Test the configuration

Use the numeric `id` returned by channel creation. A successful test sends a test message to the Slack destination configured by that webhook.

```sh
curl -sS -X POST http://localhost:8080/api/v1/channels/<channel_id>/test
```

Replace `<channel_id>` with the returned ID. If the API requires authentication, add `-H 'Authorization: Bearer <api-token>'` to both requests; never put the webhook URL in an API token header.

## Configuration

The Slack channel supports the following configuration keys (as defined in `internal/notify/slack.go`):

| Key | Required | Description |
| --- | --- | --- |
| `webhook_url` | yes | Slack incoming webhook URL. Treated as a secret. |
| `template` | no | Go `text/template` overriding the message body. See [Message templates](templates.md) for details. |

## Message formatting

- Without a custom `template`, SoroBeacon sends the shared plain-text alert summary. With a template, it sends the rendered text instead. The outgoing JSON contains only a `text` field.
- Slack attachments and Block Kit blocks are not supported by this notifier. Templates can customize message text and Slack `mrkdwn`, but cannot add attachment or block payloads.
- Slack mentions can be written into a template where appropriate. See [Message templates](templates.md) for available alert fields and escaping considerations.

## Notes

- Webhook URLs are per-channel. To deliver to multiple channels, create one Slack channel configuration per target channel (each with its own webhook URL).
- Treat `webhook_url` as sensitive. Channel configuration is encrypted at rest only when `CONFIG_ENCRYPTION_KEY` is configured; see [channel secrets](secrets.md). The API does not return channel configuration.
- For more details on incoming webhooks, see the [Slack API documentation](https://api.slack.com/messaging/webhooks).
