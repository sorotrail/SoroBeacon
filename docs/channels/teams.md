# Microsoft Teams

Posts alert messages to a Microsoft Teams channel via an **Incoming Webhook**, rendered as a **MessageCard** so the monitor name, rule type, event ID and timestamp are readable at a glance.

## Setup

1. In Microsoft Teams: open the target channel → **Manage channel** → **Connectors** (or **••• → Connectors**) → **Incoming Webhook**. Give it a name, create it, and copy the webhook URL.
   The URL contains a unique path that grants permission to post to the channel — treat it as a secret.
2. Create the channel:

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "ops-teams",
  "type": "teams",
  "config": {"webhook_url": "https://<tenant>.webhook.office.com/webhookb2/..."}
}'
```

3. Verify:

```sh
curl -s -X POST localhost:8080/api/v1/channels/1/test
```

## Config

| Key | Required | Description |
| --- | --- | --- |
| `webhook_url` | yes | Microsoft 365 incoming webhook URL. **Treated as a secret** — never logged or returned by the API. Must use HTTPS. |
| `template` | no | Go `text/template` over the alert fields, overriding the default message. See [templates.md](templates.md). |

## Message format

Each alert is sent as a MessageCard with the monitor, rule and event identifying facts:

```
┌─────────────────────────────────────┐
│ SoroBeacon alert: treasury          │
│ (red accent for critical severity)  │
├─────────────────────────────────────┤
│ Monitor   │ treasury                │
│ Rule      │ value_threshold (#3)    │
│ Event     │ transfer                │
│ Contract  │ CA7QYNF7SOWQ3GLR2...    │
│ Ledger    │ 123456                  │
│ Tx        │ abc123                  │
│ Event ID  │ 0000015985000000000-... │
│ At        │ 2026-07-21 09:41:32 UTC │
└─────────────────────────────────────┘
```

The card's accent colour reflects the alert severity: red for `critical`, blue for `info`, amber for `warning` (the default). A [digest](digest.md) summary is sent verbatim as the card body.

Non-2xx responses count as failures and are retried with backoff; the error carries the status code but never the webhook URL.

## Security

The webhook URL grants permission to post to the channel. SoroBeacon:

- Validates that the URL is present and uses HTTPS at channel creation time
- Never logs the webhook URL
- Never includes the URL in error messages or delivery response snippets
- Stores the URL encrypted at rest when `CONFIG_ENCRYPTION_KEY` is set
