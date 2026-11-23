# Opsgenie

Creates alerts through the **Opsgenie Alerts API** using GenieKey authorization. Designed for on-call rotations where a chat message is not enough — the channel uses a stable **alias** derived from the rule and event so Opsgenie can deduplicate repeated firings instead of paging a human over and over.

## Setup

1. In Opsgenie: **Settings** → **Integrations** → **Add integration** → **API**. Copy the **API key** (GenieKey).
2. Create the channel:

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "oncall-opsgenie",
  "type": "opsgenie",
  "config": {
    "api_key": "geniekey-...",
    "region": "us",
    "priority": "P3",
    "responders": ["team:payments"]
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
| `api_key` | yes | Opsgenie GenieKey. **Treated as a secret** — never logged or returned by the API. |
| `region` | no | `us` (default) or `eu`. Selects the API base URL. |
| `priority` | no | One of `P1`, `P2`, `P3` (default), `P4`, `P5`. |
| `responders` | no | Array of responder identifiers (e.g., `team:payments`, `user:ops`). |
| `api_base` | no | Override the API base URL (mainly for testing). |

## Payload

Each alert is sent as a `POST /v2/alerts` request:

```json
{
  "message": "SoroBeacon alert: Token treasury",
  "alias": "sorobeacon-3-0000015985348674617345-0000000001",
  "description": "Monitor: Token treasury\nRule: event_emitted (#3)\nContract: CA7QYNF7...UWDA\nEvent: transfer\nLedger: 3721765\nTx: 4f2a...\nEvent ID: 0000015985348674617345-0000000001\nAt: 2026-07-21 09:41:32 UTC\n",
  "priority": "P3",
  "details": {
    "monitor_id": "9",
    "monitor_name": "Token treasury",
    "rule_id": "3",
    "rule_type": "event_emitted",
    "event_id": "0000015985348674617345-0000000001",
    "contract_id": "CA7QYNF7...UWDA",
    "event_name": "transfer",
    "ledger": "3721765",
    "tx_hash": "4f2a..."
  },
  "responders": ["team:payments"]
}
```

### Deduplication

The **`alias`** field is deterministic for a given `(rule_id, event_id)` pair:

```
alias = "sorobeacon-" + rule_id + "-" + event_id
```

This mirrors SoroBeacon's own dedup guard: if the same alert is delivered twice (e.g., a retry or a reorg retraction followed by re-alert), Opsgenie folds it into the existing alert instead of creating a new one and paging again.

### Region selection

| Region | Base URL |
| --- | --- |
| `us` (default) | `https://api.opsgenie.com` |
| `eu` | `https://api.eu.opsgenie.com` |

### Priority levels

Opsgenie priorities map directly: `P1` (highest) through `P5` (lowest). Default is `P3`.

Non-2xx responses count as failures and are retried with backoff; the error carries the status code but never the API key.

## Security

The API key is a GenieKey that grants full alert creation permission. SoroBeacon:
- Validates the key is present at channel creation time
- Never logs the API key
- Never includes the key in error messages or delivery response snippets
- Stores the key encrypted at rest when `CONFIG_ENCRYPTION_KEY` is set