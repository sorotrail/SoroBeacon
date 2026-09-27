# Twilio SMS

Sends alerts as SMS text messages using the Twilio Messages API via HTTP basic authentication and form-encoded bodies.

## Setup

1. Obtain your **Account SID** and **Auth Token** from your Twilio Console.
2. Purchase or configure a Twilio phone number (`from`).
3. Create the channel:

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "sms-alerts",
  "type": "twilio",
  "config": {
    "account_sid": "AC...",
    "auth_token": "...",
    "from": "+15005550006",
    "to": ["+15551234567"]
  }
}'
```

4. Verify:

```sh
curl -s -X POST localhost:8080/api/v1/channels/5/test
```

## Config

| Key | Required | Description |
| --- | --- | --- |
| `account_sid` | yes | Twilio Account SID. |
| `auth_token` | yes | Twilio Auth Token. Treated as a secret. |
| `from` | yes | Twilio phone number sending the SMS. |
| `to` | yes | List of recipient phone numbers (non-empty array). |
| `api_base` | no | Override the Twilio API base URL, default `https://api.twilio.com`. |
| `template` | no | Go `text/template` overriding the message. See [Message templates](templates.md). |

## Cost & Length Caveat

* **Per-message billing:** Twilio bills per SMS segment. High-frequency rules or large recipient lists can incur significant carrier and Twilio usage costs.
* **Length limit:** SMS messages are hard-limited to 160 characters. Long messages are automatically compacted and truncated to fit safely without failing delivery.
