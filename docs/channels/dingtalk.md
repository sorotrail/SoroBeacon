# DingTalk

Posts alert messages to a DingTalk group chat via a custom robot webhook.

## Setup

1. In DingTalk: open the target group chat → **Group Settings → Smart Group Assistant → Add Robot → Custom Robot**, set a name, choose **Custom Keyword** or **Signature** as the security setting, copy the webhook URL.
   - If you choose **Signature**, DingTalk gives you a `secret` (starts with `SEC...`). Provide it in the channel config so SoroBeacon can sign each request.
   - If you choose **Custom Keyword** (no signature), omit the `secret` field.
2. Create the channel:

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "ops-dingtalk",
  "type": "dingtalk",
  "config": {
    "webhook_url": "https://oapi.dingtalk.com/robot/send?access_token=...",
    "secret": "SEC..."
  }
}'
```

3. Verify:

```sh
curl -s -X POST localhost:8080/api/v1/channels/1/test
```

## Config

| Key | Required | Description |
| --- | --- | --- |
| `webhook_url` | yes | DingTalk robot webhook URL. Must use HTTPS. Treated as a secret. |
| `secret` | no* | Signing secret from the robot's **Signature** security setting. Required when the robot is configured with "additional signature". |
| `template` | no | Go `text/template` overriding the message. See [Message templates](templates.md). |

* `secret` is optional in the config but required when the robot uses **Signature** security mode.

## Security modes

### Signature (recommended)

The robot is configured with **Signature**. DingTalk issues a `secret` (e.g., `SECabc123...`). Every request from SoroBeacon includes two query parameters:

- `timestamp` — current Unix time in milliseconds.
- `sign` — Base64-encoded HMAC-SHA256 of `timestamp\nsecret` using `secret` as the key.

DingTalk verifies the signature and rejects requests older than 1 hour.

### Custom Keyword (no signature)

The robot is configured with **Custom Keyword** only. No signature is sent; the webhook URL alone authorizes the request. Anyone with the URL can post, so keep it secret.

## Message format

Markdown message rendered from the shared alert template (override it with [`template`](templates.md)):

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

DingTalk's markdown support is a subset of CommonMark. The default template uses plain text which renders correctly.

## Error handling

DingTalk returns HTTP 200 with a JSON body on both success and failure. A non-zero `errcode` indicates failure (e.g., `310000` for signature mismatch). SoroBeacon surfaces the `errcode` and `errmsg` as a delivery error and retries with exponential backoff.