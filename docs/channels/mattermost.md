# Mattermost

Posts alerts to a Mattermost channel through an incoming webhook. Each
message carries the plain-text alert summary plus an attachment with the
structured fields (monitor, rule type, event ID, contract ID, ledger), with a
side-bar colour taken from the alert severity.

Mattermost's incoming-webhook payload is Slack-compatible, so the message
looks the same as it would in Slack with attachments.

## Setup

1. In Mattermost: **Main Menu → Integrations → Incoming Webhooks → Add
   Incoming Webhook**. Pick the default channel and save, then copy the URL
   (`https://mattermost.example.com/hooks/xxx`).
2. If you want to use the `channel` or `username` overrides below, a system
   admin must enable **Enable integrations to override usernames** (and, for
   the channel, the webhook must not be locked to its channel) under
   **System Console → Integrations → Integration Management**.
3. Create the channel:

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "ops-mattermost",
  "type": "mattermost",
  "config": {
    "webhook_url": "https://mattermost.example.com/hooks/xxx",
    "channel": "#alerts",
    "username": "sorobeacon"
  }
}'
```

4. Verify:

```sh
curl -s -X POST localhost:8080/api/v1/channels/<id>/test
```

## Config

| Key | Required | Description |
| --- | --- | --- |
| `webhook_url` | yes | Mattermost incoming webhook URL. Must be `https://`. Treated as a secret: it never appears in errors, logs or delivery records. |
| `channel` | no | Channel to post to instead of the webhook's default, e.g. `#alerts` or `town-square`. Left out of the payload when unset. |
| `username` | no | Display name to post as instead of the webhook's default. Left out of the payload when unset. |
| `template` | no | Go `text/template` overriding the message text. The attachment fields are unaffected. See [Message templates](templates.md). |

## Failure behaviour

A non-2xx response becomes a delivery error of the form
`mattermost: status 403: <truncated response body>`; the webhook URL is never
included. Network errors are reported with the URL stripped. Deliveries are
retried by the dispatcher like every other HTTP channel.

[Digest](digest.md) messages are sent as text only, without the attachment,
because a digest covers many alerts.
