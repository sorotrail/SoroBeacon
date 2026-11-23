# Rocket.Chat

Posts alerts to a Rocket.Chat channel through an incoming webhook
integration. Each message carries the plain-text alert summary plus an
attachment with the structured fields (monitor, rule type, event ID,
contract ID, ledger), coloured by alert severity.

## Setup

1. In Rocket.Chat: **Administration → Workspace → Integrations → New →
   Incoming**. Enable it, choose the channel to post to and the user to post
   as, and save. Copy the **Webhook URL**
   (`https://chat.example.com/hooks/xxxx/yyyy`).
2. Create the channel:

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "ops-rocketchat",
  "type": "rocketchat",
  "config": {
    "webhook_url": "https://chat.example.com/hooks/xxxx/yyyy",
    "alias": "SoroBeacon",
    "emoji": ":satellite:"
  }
}'
```

3. Verify:

```sh
curl -s -X POST localhost:8080/api/v1/channels/<id>/test
```

## Config

| Key | Required | Description |
| --- | --- | --- |
| `webhook_url` | yes | Rocket.Chat incoming webhook URL. Must be `https://`. The token is part of the path, so the URL is treated as a secret and never appears in errors, logs or delivery records. |
| `alias` | no | Display name shown on the message instead of the integration user's name. Left out of the payload when unset. |
| `emoji` | no | Emoji used as the avatar, e.g. `:satellite:`. Left out of the payload when unset. |
| `template` | no | Go `text/template` overriding the message text. The attachment fields are unaffected. See [Message templates](templates.md). |

## Failure behaviour

A non-2xx response becomes a delivery error carrying only the status code,
e.g. `rocketchat: status 400`. The response body is deliberately dropped:
Rocket.Chat incoming integrations can run a script that shapes the reply, and
a script that echoes the request would otherwise leak the hook token into the
delivery record. Network errors are reported with the URL stripped.

[Digest](digest.md) messages are sent as text only, without the attachment.
