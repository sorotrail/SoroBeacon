# Telegram

Sends alert messages through a Telegram bot using the Bot API `sendMessage` method.

## Setup

### 1. Create a bot and obtain its token

1. Open Telegram and start a conversation with [@BotFather](https://t.me/BotFather), Telegram's official bot-management account.
2. Send `/newbot`, follow the prompts for a display name and a unique username ending in `bot`, then copy the token BotFather returns.
3. Treat the token as a password. Do not paste it into source code, tickets, shell commands, or chat. If it is exposed, use BotFather's `/revoke` flow and replace the token in SoroBeacon.

### 2. Find the destination chat ID

The bot must receive an update before Telegram includes that chat in `getUpdates`. The command below prompts for the token without displaying it and prints only the chat type and ID, not message contents. It requires `curl` and `jq`.

```sh
read -r -s -p 'Telegram bot token: ' TELEGRAM_BOT_TOKEN
printf '\n'
curl --config - <<CURL_CONFIG | jq -r '
.result[] | (.message.chat? // .channel_post.chat?) |
select(.) | [.type, (.id | tostring)] | @tsv'
url = "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/getUpdates"
silent
show-error
CURL_CONFIG
unset TELEGRAM_BOT_TOKEN
```

Use the returned `type` to choose the right ID:

- **Private chat:** open the bot and send `/start`. Run the command and use the ID on the `private` row.
- **Group or supergroup:** add the bot to the group, then send a command that addresses it, such as `/start@your_bot_username`. Run the command and use the ID on the `group` or `supergroup` row. Group IDs are commonly negative.
- **Channel:** add the bot as a channel administrator with the **Post Messages** permission, then publish a new channel post. Run the command and use the ID on the `channel` row. A public channel's `@username` can also be used as `chat_id` instead of its numeric ID.

If no row appears, confirm the bot received a new message or channel post and run the lookup again. `getUpdates` is unavailable while a webhook is configured for that bot; do not disable a production webhook without coordinating with its owner. Avoid running this lookup alongside another long-polling consumer of the same bot.

**Group delivery permission:** adding the bot is not enough if group permissions prevent it from sending messages. Grant it explicit permission to post (or make it an administrator with the required send-message permission). For a channel destination, it must be an administrator with **Post Messages** enabled.

### 3. Create the SoroBeacon channel

Enter the token and destination ID when prompted. The request body is sent to `curl` over standard input; the API response does not include the channel configuration.

```sh
read -r -s -p 'Telegram bot token: ' TELEGRAM_BOT_TOKEN
printf '\n'
read -r -p 'Destination chat ID: ' TELEGRAM_CHAT_ID
curl -sS -X POST http://localhost:8080/api/v1/channels \
  -H 'Content-Type: application/json' \
  --data-binary @- <<JSON
{
  "name": "oncall-telegram",
  "type": "telegram",
  "config": {
    "bot_token": "${TELEGRAM_BOT_TOKEN}",
    "chat_id": "${TELEGRAM_CHAT_ID}"
  }
}
JSON
unset TELEGRAM_BOT_TOKEN TELEGRAM_CHAT_ID
```

### 4. Test delivery

```sh
curl -sS -X POST http://localhost:8080/api/v1/channels/<channel_id>/test
```

Replace `<channel_id>` with the numeric ID returned by channel creation. If the SoroBeacon API requires authentication, add `-H 'Authorization: Bearer <api-token>'` to both API requests.

## Configuration

| Key | Required | Description |
| --- | --- | --- |
| `bot_token` | yes | Bot API token issued by BotFather. A secret; SoroBeacon uses it to call `sendMessage`. |
| `chat_id` | yes | Destination's numeric chat ID or a public channel username such as `@channelname`; passed as a string. |
| `api_base` | no | Bot API base URL. Defaults to `https://api.telegram.org`; mainly intended for a self-hosted Bot API server or tests. |
| `template` | no | Go `text/template` overriding the plain-text alert message. See [Message templates](templates.md). |

Channel configuration is encrypted at rest only when `CONFIG_ENCRYPTION_KEY` is set. See [channel secrets](secrets.md). The API does not return channel configuration; protect the bot token as you would any other credential.
