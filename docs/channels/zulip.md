# Zulip

Posts alerts to a Zulip stream as a bot user. Zulip's stream/topic model
suits alerting well: by default each monitor gets its own topic, so a noisy
contract stays in its own thread instead of burying everything else.

The channel calls Zulip's
[send a message](https://zulip.com/api/send-message) endpoint
(`POST /api/v1/messages`) with a form-encoded body and HTTP basic auth
(bot email + API key).

## Creating a bot

1. In Zulip: **Settings (gear) → Personal settings → Bots → Add a new bot**.
2. Choose bot type **Incoming webhook** (it can only send messages, which is
   all SoroBeacon needs) — **Generic bot** also works.
3. Give it a name, e.g. *SoroBeacon*, and create it.
4. Copy the bot's **email** (e.g. `beacon-bot@example.zulipchat.com`) and
   **API key** from the bot list.
5. Make sure the bot can post to the target stream: for a private stream,
   subscribe the bot to it (**Stream settings → Subscribers**).

## Setup

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "ops-zulip",
  "type": "zulip",
  "config": {
    "site": "https://example.zulipchat.com",
    "bot_email": "beacon-bot@example.zulipchat.com",
    "api_key": "…",
    "stream": "alerts",
    "topic": "sorobeacon"
  }
}'
```

Verify:

```sh
curl -s -X POST localhost:8080/api/v1/channels/<id>/test
```

## Config

| Key | Required | Description |
| --- | --- | --- |
| `site` | yes | Your Zulip server's base URL, e.g. `https://example.zulipchat.com` or `https://zulip.example.org`. Must be `https://`, because the API key is sent with every request. A trailing slash is fine. |
| `bot_email` | yes | The bot's email address from the bot settings page. |
| `api_key` | yes | The bot's API key. Treated as a secret: it never appears in errors, logs or delivery records. Can be an [external secret](secrets.md) reference. |
| `stream` | yes | Name of the stream to post to. |
| `topic` | no | Topic to post every alert under. When unset, each alert goes to a topic named after its monitor (or `SoroBeacon` for a [digest](digest.md)). Topics longer than Zulip's 60-character limit are shortened. |
| `template` | no | Go `text/template` overriding the message content (Zulip renders it as Markdown). See [Message templates](templates.md). |

A missing required key is rejected when the channel is created, with an error
naming every missing key, e.g. `zulip: api_key, stream is required`.

## Failure behaviour

A non-2xx response becomes a delivery error of the form
`zulip: status 401: <truncated response body>`. Zulip's error bodies
(`{"result":"error","msg":"…"}`) do not echo the credentials, and the request
URL is never included. A `401` usually means a wrong `bot_email`/`api_key`;
a `400` with a stream error usually means the bot is not subscribed to a
private stream or the stream name is wrong.
