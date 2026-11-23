# Discord

SoroBeacon sends alert notifications to a Discord channel through an incoming
webhook. It posts the alert summary as a message; it does not need a Discord
bot or a bot token.

## Create a Discord webhook

You need permission to manage webhooks in the Discord server. If you do not
see the Webhooks settings, ask a server administrator to create one for you.

1. Open Discord and select the server where alerts should appear.
2. Open the server menu and choose **Server Settings**.
3. Open **Integrations**, then **Webhooks**.
4. Select **New Webhook** or **Create Webhook**.
5. Give the webhook a recognizable name if you like.
6. Choose the destination channel for the notifications.
7. Save the webhook.
8. Select **Copy Webhook URL** and keep the URL private. You will enter it in
   SoroBeacon in the next step.

## Create a Discord channel in SoroBeacon

The API base URL is `http://localhost:8080` in the default local setup; replace
it with the address of your SoroBeacon instance. When API authentication is
enabled, use a bearer token that can write channels. A scoped token needs the
`channels:write` scope. If `API_TOKEN` is not configured, omit the
`Authorization` header; an unauthenticated API should only be reachable on a
trusted network. See [API authentication](../reference/api.md#authentication).

Replace the webhook URL placeholder below with the URL you copied. The example
uses placeholders only; do not put a real webhook URL in documentation,
version control, or shared shell history.

```sh
export SOROBEACON_URL='http://localhost:8080'
export API_TOKEN='<your-sorobeacon-api-token>'

curl -sS -X POST "$SOROBEACON_URL/api/v1/channels" \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $API_TOKEN" \
  -d '{
    "name": "ops-discord",
    "type": "discord",
    "config": {
      "webhook_url": "https://discord.com/api/webhooks/<WEBHOOK_ID>/<WEBHOOK_TOKEN>"
    }
  }'
```

The endpoint is `POST /api/v1/channels`. It returns the created channel,
including its `id`; the secret `config` is not included in the response. Use
that `id` to test delivery below. If API authentication is disabled, remove the
`Authorization` header and the `API_TOKEN` assignment from this example.

## Config

| Key | Required | Description |
| --- | --- | --- |
| `webhook_url` | yes | Discord incoming webhook URL. It must be non-empty; there is no default. Treat it as a secret. |
| `template` | no | Optional Go `text/template` that replaces the default alert message. Omitted or empty uses the built-in plain-text alert summary. Invalid template syntax is rejected when the channel is created or updated. See [Message templates](templates.md). |

SoroBeacon sends the rendered message as Discord webhook `content`. Other
Discord webhook options, such as a custom username, avatar, or embed, are not
configurable through this channel.

## Test the channel

Testing sends a synthetic alert through the webhook, so check the destination
Discord channel for the test message. Replace `<CHANNEL_ID>` with the numeric
`id` returned when you created the channel:

```sh
curl -sS -X POST "$SOROBEACON_URL/api/v1/channels/<CHANNEL_ID>/test" \
  -H "Authorization: Bearer $API_TOKEN"
```

This endpoint does not need a request body. If API authentication is disabled,
omit the `Authorization` header. A successful test returns HTTP `200` with
`{"status":"sent"}`. A failed delivery returns HTTP `502` with
`{"status":"failed","error":"..."}`; check the error and the webhook
configuration before trying again.

## Security considerations

The webhook URL is a secret: anyone who has it can post messages to its
associated Discord channel. Keep it private; do not commit it to Git or paste
it into issues, support requests, or logs. If it is exposed, delete or rotate
the webhook in Discord and update the SoroBeacon channel with the replacement.

SoroBeacon never returns channel config from the API. To keep channel secrets
out of the database in plaintext, configure encryption at rest with
[`CONFIG_ENCRYPTION_KEY`](../getting-started/configuration.md#encrypting-channel-config-at-rest).
You can also use an external secret reference; see
[External secrets in channel config](secrets.md).

## Troubleshooting

- **SoroBeacon rejects the channel configuration:** Check that `config` is
  valid JSON, `webhook_url` is present and non-empty, and any `template` has
  valid Go template syntax.
- **You cannot create a webhook in Discord:** Your Discord account needs
  permission to manage webhooks in that server. Ask a server administrator to
  grant access or create the webhook.
- **The test returns `failed`:** Confirm that you copied the complete webhook
  URL, that the webhook still exists and points to the intended channel, and
  that SoroBeacon can reach Discord over the network. If the webhook URL was
  exposed, rotate it and update the SoroBeacon channel.
- **The API rejects the request:** When authentication is enabled, check that
  the bearer token is valid and, for a scoped token, includes `channels:write`.
