# Cisco Webex

Posts alert messages to a Webex room via a bot access token.

This channel is designed for enterprise and telecom environments where Webex is
the standard chat platform and Slack/Discord are not permitted.

## Setup

1. **Create a Webex bot**:

   - Go to <https://developer.webex.com/my-apps/new/bot>
   - Fill in the bot details (name, avatar, description)
   - Copy the **Bot Access Token** (starts with `Y2lz...` or similar)

2. **Find the room ID**:

   - Add the bot to the target space/room in Webex
   - Use the Webex API to list rooms the bot is in:
     ```sh
     curl -H "Authorization: Bearer <bot_token>" \
       https://webexapis.com/v1/rooms
     ```
   - Or use the Webex developer portal "Test" feature for the `/rooms` endpoint
   - Copy the `id` of the target room (format: `Y2lzY29zcGFyazovL3VzL1JPT00v...`)

   **Note**: The bot must be a member of the room to post messages.

3. **Create the channel**:

   ```sh
   curl -s -X POST localhost:8080/api/v1/channels -d '{
     "name": "ops-webex",
     "type": "webex",
     "config": {
       "bot_token": "Y2lzY29zcGFyazovL3VzL1JPT00v...",
       "room_id": "Y2lzY29zcGFyazovL3VzL1JPT00v..."
     }
   }'
   ```

4. **Verify**:

   ```sh
   curl -s -X POST localhost:8080/api/v1/channels/1/test
   ```

## Config

| Key        | Required | Description |
|------------|----------|-------------|
| `bot_token` | yes      | Bot access token from Webex Developer Portal. Treated as a secret — never logged or returned in API responses. |
| `room_id`   | yes      | Webex room/space ID where alerts will be posted. The bot must be a member of this room. |
| `template`  | no       | Go `text/template` overriding the message. See [Message templates](templates.md). |

## Message format

Markdown message rendered from the shared alert template (override it with
[`template`](templates.md)):

```
🔔 **SoroBeacon alert: Token treasury**
**Rule:** value_threshold (#3)
**Contract:** `CA7QYNF7...UWDA`
**Event:** transfer
**Ledger:** 3721765
**Tx:** `4f2a...`
**Event ID:** `0000015985...-0000000001`
**At:** 2026-07-21 09:41:32 UTC
```

The JSON payload posted to `POST https://webexapis.com/v1/messages` is:

```json
{
  "roomId": "Y2lzY29zcGFyazovL3VzL1JPT00v...",
  "markdown": "<rendered markdown message>"
}
```

## Error handling

- **401 Unauthorized / 403 Forbidden**: Returns an error suggesting to check the
  bot token and that the bot is a member of the room. The bot token is never
  included in error messages.
- Other non-2xx responses return an error with the HTTP status code and a
  truncated response body.
- Network failures are wrapped and surfaced as delivery errors.
- The channel config (which contains the bot token) is never logged or returned
  in API responses.

## Notes

- The bot token is sent in the `Authorization: Bearer <token>` header.
- Messages are sent as markdown so the monitor name, event ID, and other fields
  render distinctly in Webex.
- For high availability, consider creating multiple bots or using a shared bot
  account.