# Telegram

Sends alert messages through a Telegram bot (`sendMessage` on the Bot API).

## Setup

1. Talk to [@BotFather](https://t.me/BotFather), `/newbot`, copy the **bot token**.
2. Add the bot to your group (or start a direct chat) and find the **chat ID** — e.g. add [@userinfobot](https://t.me/userinfobot) to the group, or call `getUpdates` on your bot after sending it a message. Group IDs are negative numbers like `-1001234567890`.
3. Create the channel:

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "oncall-telegram",
  "type": "telegram",
  "config": {"bot_token": "123456:ABC-DEF...", "chat_id": "-1001234567890"}
}'
```

4. Verify:

```sh
curl -s -X POST localhost:8080/api/v1/channels/3/test
```

## Config

| Key | Required | Description |
| --- | --- | --- |
| `bot_token` | yes | Token from BotFather. Treated as a secret. |
| `chat_id` | yes | Target chat/group/channel ID (string). |
| `api_base` | no | Override for `https://api.telegram.org` (self-hosted Bot API servers, tests). |
