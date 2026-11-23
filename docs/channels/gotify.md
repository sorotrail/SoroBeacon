# Gotify

Posts alert messages to a self-hosted **Gotify** server using an application token. Ideal for teams that already self-host their monitoring stack and prefer alerts stay within their infrastructure.

## Setup

1. In Gotify: **Apps** → **Create application** → copy the **application token**.
2. Create the channel:

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "ops-gotify",
  "type": "gotify",
  "config": {
    "server_url": "https://gotify.example.com",
    "token": "A1B2C3D4E5F6...",
    "priority": 5
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
| `server_url` | yes | Gotify server base URL (e.g., `https://gotify.example.com` or `https://gotify.example.com/gotify`). Must use HTTP or HTTPS. Trailing slashes are normalized. |
| `token` | yes | Gotify application token. **Treated as a secret** — never logged or returned by the API. |
| `priority` | no | Message priority (1–10). Defaults to `5`. Passed through to Gotify directly. |

## Message format

Each alert is sent as a `POST /message` request with the token in the query string:

```json
{
  "title": "SoroBeacon Alert",
  "message": "Monitor: Token treasury\nRule: event_emitted (#3)\nContract: CA7QYNF7...UWDA\nEvent: transfer\nLedger: 3721765\nTx: 4f2a...\nEvent ID: 0000015985348674617345-0000000001\nAt: 2026-07-21 09:41:32 UTC\n",
  "priority": 5
}
```

The message body carries:
- **Monitor** — the monitor name
- **Rule** — rule type and ID
- **Contract** — the Soroban contract ID
- **Event** — the event name (when available)
- **Ledger** — the ledger sequence number
- **Tx** — the transaction hash
- **Event ID** — the unique event identifier
- **At** — when the alert was created (UTC)

Non-2xx responses count as failures and are retried with backoff; the error carries the status code but never the token or server URL.

## URL normalization

The `server_url` is normalized at channel creation:
- Trailing slashes are removed: `https://host/` → `https://host`
- The message endpoint is appended: `https://host` → `https://host/message?token=...`
- Sub-paths are preserved: `https://host/gotify` → `https://host/gotify/message?token=...`

Both `http://` and `https://` schemes are accepted.

## Security

The application token is passed as a query parameter (`?token=...`) per Gotify's API convention. SoroBeacon:
- Validates the URL uses HTTP/HTTPS at channel creation time
- Never logs the token or the full URL with the token
- Never includes the token in error messages or delivery response snippets
- Stores the token encrypted at rest when `CONFIG_ENCRYPTION_KEY` is set