# ntfy

Posts the rendered alert as plain text to a topic on [ntfy.sh](https://ntfy.sh) or a self-hosted ntfy instance.

Self-hosted instances are often reached on a private network, so `server_url` may use `http://` as well as `https://`.

## Setup

1. Pick a topic name (or create one on your ntfy server). For ntfy.sh, a random unguessable topic is safer than a short public name.
2. Optional: create an access token if the topic requires auth (`ntfy token` on self-hosted, or ntfy.sh account token).
3. Create the channel:

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "ops-ntfy",
  "type": "ntfy",
  "config": {"topic": "sorobeacon-ops"}
}'
```

Self-hosted example:

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "ops-ntfy",
  "type": "ntfy",
  "config": {
    "server_url": "http://ntfy.lan",
    "topic": "sorobeacon-ops",
    "access_token": "tk_...",
    "priority": 4
  }
}'
```

4. Verify:

```sh
curl -s -X POST localhost:8080/api/v1/channels/1/test
```

## Config

| Key | Required | Description |
| --- | --- | --- |
| `topic` | yes | ntfy topic name. Must not contain `/`. |
| `server_url` | no | Base URL of the ntfy server. Defaults to `https://ntfy.sh`. Trailing slashes are ignored. `http://` is allowed for private/self-hosted instances. |
| `access_token` | no | Sent as `Authorization: Bearer …`. Treated as a secret; never logged or returned. |
| `priority` | no | ntfy priority `1`–`5` (min → max). Omitted uses ntfy's default. |

## Message format

The POST body is the shared plain-text alert template. The `Title` header is the monitor name.

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
