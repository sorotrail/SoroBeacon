# Lark / Feishu

Posts alert messages to a Lark (Feishu) group chat via a custom bot webhook.

Lark is the international version of Feishu (飞书), the major workplace platform
across Asia-Pacific, widely used by trading and fintech teams. Like DingTalk,
it supports optional HMAC-SHA256 signing, so it needs a real integration rather
than the generic webhook.

## Setup

1. **Create a custom bot in Lark/Feishu**:

   - Open the target group chat
   - Click the group settings → **Bots** → **Add Bot** → **Custom Bot**
   - Set a name and description
   - **Copy the webhook URL** (format: `https://open.larksuite.com/open-apis/bot/v2/hook/xxxx`)
   - Optionally enable **Signature Verification** and copy the **Signing Secret**

2. **Create the channel**:

   ```sh
   curl -s -X POST localhost:8080/api/v1/channels -d '{
     "name": "ops-lark",
     "type": "lark",
     "config": {
       "webhook_url": "https://open.larksuite.com/open-apis/bot/v2/hook/xxxx",
       "secret": "optional-signing-secret"
     }
   }'
   ```

   The `secret` is only required when you enabled Signature Verification on the
   bot configuration page.

3. **Verify**:

   ```sh
   curl -s -X POST localhost:8080/api/v1/channels/1/test
   ```

## Config

| Key          | Required | Description |
|--------------|----------|-------------|
| `webhook_url` | yes      | The custom bot webhook URL from Lark/Feishu. Must use HTTPS. Treated as a secret — never logged or returned in API responses. |
| `secret`      | no       | The signing secret for the custom bot. Required only when Signature Verification is enabled on the bot. Treated as a secret. |
| `template`    | no       | Go `text/template` overriding the message. See [Message templates](templates.md). |

## Message format

Plain-text message rendered from the shared alert template (override it with
[`template`](templates.md)):

```
🔔 SoroBeacon alert: Token treasury
Rule: value_threshold (#3)
Contract: CA7QYNF7...UWDA
Event: transfer
Ledger: 3721765
Tx: 4f2a...
Event ID: 0000015985...-0000000001
At: 2026-07-21 09:41:32 UTC
```

The JSON payload posted to the webhook URL:

**Without secret:**
```json
{
  "msg_type": "text",
  "content": {
    "text": "<rendered message>"
  }
}
```

**With secret (signature verification enabled):**
```json
{
  "msg_type": "text",
  "content": {
    "text": "<rendered message>"
  },
  "timestamp": 1699360473,
  "sign": "base64_encoded_hmac_sha256"
}
```

### Signature algorithm

When `secret` is configured, the request includes `timestamp` and `sign` fields
computed as follows (per Lark/Feishu documentation):

1. Create the signing key: `timestamp + "\n" + secret`
2. Compute HMAC-SHA256 of an **empty string** using that key
3. Base64 encode the result

This differs from the generic webhook channel (which signs `timestamp + "." + body`
with the secret as key, hex-encoded) and from DingTalk (which has a similar but
different format). The implementation follows Lark's official documentation.

## Error handling

- **Lark API errors**: Lark returns HTTP 200 with a JSON body containing a
  non-zero `code` on failure (e.g., `{"code":94100,"msg":"signature verification failed"}`).
  These are surfaced as errors with the Lark error code and message.
- **HTTP errors**: Non-2xx HTTP responses return an error with the status code.
- **Network failures**: Wrapped and surfaced as delivery errors.
- The channel config (webhook URL and secret) is never logged or returned in
  API responses or error messages.

## Notes

- The webhook URL must use HTTPS (enforced at config validation).
- For rich text or interactive cards, you can override the message using the
  `template` field with Lark's markdown syntax, but the channel currently sends
  `msg_type: "text"`. Card support may be added in the future.
- If you rotate the signing secret, update the channel config with the new
  secret. There is no automatic rotation support.