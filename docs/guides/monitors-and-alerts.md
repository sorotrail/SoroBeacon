# Monitors & alerts

## The model

```
Monitor ──watches──▶ contract IDs
   │
   ├── Rules      (conditions checked against every event of those contracts)
   └── Channels   (where matching events get delivered)
```

* A **monitor** groups one or more contract addresses under a name and an enabled flag.
* Each **rule** belongs to one monitor and has a `type` (which evaluator runs) and `params` (its arguments). Every enabled rule is evaluated against every event emitted by the monitor's contracts.
* A monitor's **channels** receive every alert any of its rules fires.

One event can fire multiple rules — each match is its own alert. The same rule can never fire twice for the same event: alerts are deduplicated on `(rule_id, event_id)` at the database level.

## Walkthrough: watch a token, alert on big transfers

```sh
# Channel
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "treasury-alerts", "type": "slack",
  "config": {"webhook_url": "https://hooks.slack.com/services/T000/B000/XXXX"}
}'
# -> {"id": 1, ...}

# Monitor
curl -s -X POST localhost:8080/api/v1/monitors -d '{
  "name": "Token treasury",
  "contract_ids": ["CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"],
  "channel_ids": [1]
}'
# -> {"id": 1, ...}

# Rule: transfers over 100 XLM-equivalent (7 decimals -> stroops as a string)
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "value_threshold",
  "params": {
    "event_name": "transfer",
    "comparison": "gt",
    "threshold": "1000000000"
  }
}'
```

{% hint style="info" %}
Contract IDs are validated as strkeys (starting with `C`) at create time. This matters: the RPC rejects an entire `getEvents` request if any filter contains a malformed ID, so one bad address would otherwise stall ingestion for every monitor.
{% endhint %}

## Reviewing alert history

```sh
# Filterable history, newest first
curl -s 'localhost:8080/api/v1/alerts?monitor_id=1&from=2026-07-01T00:00:00Z&limit=20'

# What happened to a specific alert's deliveries (every attempt, success or failure)
curl -s localhost:8080/api/v1/alerts/7/deliveries
```

Each alert's `payload` carries the full decoded event — contract, event name, ledger, transaction hash, topics, and value — so the history is useful for auditing even without the original chain data (which the RPC forgets within days).

## Pausing things

Everything has an `enabled` flag, togglable via `PATCH` or the dashboard:

* Disable a **monitor** → its contracts stop being polled for (unless another monitor watches them).
* Disable a **rule** → it stops matching; other rules on the monitor continue.
* Disable a **channel** → dispatch skips it; other channels still receive alerts.
