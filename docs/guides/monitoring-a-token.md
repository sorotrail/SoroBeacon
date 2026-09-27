# End-to-end: monitoring a token contract

This guide walks through the complete path from a token contract address to an
alert arriving in your configured channel (Slack, Discord, email, etc.). It
pulls together concepts from the quickstart, monitors, rules, and channels into
one realistic scenario.

## Prerequisites

SoroBeacon is running. The quickest way is:

```sh
docker compose up --build -d
```

Wait until the healthcheck passes:

```sh
curl -s localhost:8080/api/v1/health
# {"db":"ok","rpc":"ok",...,"status":"ok"}
```

## Step 1: Pick a token contract on testnet

On Stellar testnet, tokens that follow [SEP-41](https://github.com/stellar/stellar-protocol/blob/master/ec-0041/ec-0041-token.md)
emit traceable events. We'll monitor a testnet USDC-like token.

**Contract address (testnet):** `CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA`

You can verify this is a valid contract on testnet by checking it on a
[Stellar expert](https://expert.stellar.org/) or by looking at the
[testnet horizon](https://horizon-testnet.stellar.org) endpoints `/accounts`
and `/assets` for that address.

> **Finding a contract ID:** If you have a different token in mind, look up its
> address on a Stellar expert or horizon, then use the strkey form (starts with
> `C`). The contract must be active on the network you're monitoring.

## Step 2: Create a monitor

```sh
curl -s -X POST localhost:8080/api/v1/monitors -d '{
  "name": "My token monitor",
  "contract_ids": ["CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"],
  "channel_ids": []
}'
# -> {"id": 1, ...}
```

The monitor is now watching that contract. `channel_ids` is empty for now — we'll
attach a channel in the next step.

## Step 3: Add a `token_event` rule for large transfers

We'll alert on every token transfer where the amount exceeds a threshold. The
`token_event` rule type knows the SEP-41 topic layout, so we don't have to
hand-count topic indices.

```sh
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "token_event",
  "params": {
    "event": "transfer",
    "min_amount": "1000000"
  }
}'
```

**What this does:** Alert on any `transfer` event where the amount is >= 1,000,000
stroops (≈ 0.1 XLM, since XLM has 7 decimal places). Adjust `min_amount` up
or down to match your threshold.

## Step 4: Create a delivery channel

Pick a channel type you have credentials for. Here we'll use Slack:

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "token-alerts",
  "type": "slack",
  "config": {"webhook_url": "https://hooks.slack.com/services/T000/B000/XXXX"}
}'
# -> {"id": 1, ...}
```

## Step 5: Attach the channel to the monitor

```sh
curl -s -X PATCH localhost:8080/api/v1/monitors/1 -d '{
  "channel_ids": [1]
}'
```

The monitor now has a channel attached. Every time the `token_event` rule fires,
an alert will be dispatched to the Slack webhook.

## Step 6: Trigger a matching event (or wait)

The poller runs every `POLL_INTERVAL` (5 seconds by default). It fetches events
from the RPC and evaluates rules against them. After the first poll cycle that
includes a transfer event above the threshold, an alert will be created and
dispatched.

To **test** this quickly, you can send a synthetic alert:

```sh
curl -s -X POST localhost:8080/api/v1/channels/1/test
# {"status":"sent"}
```

Or, if you have access to the Stellar testnet and can trigger a real token
transfer from the monitored contract, just wait for the next poll interval.

## Step 7: View the alert

Once an alert has been created:

```sh
curl -s 'localhost:8080/api/v1/alerts?monitor_id=1&limit=20'
# {"alerts":[{"id":1,"monitor_id":1,"rule_id":1,"contract_id":"...","payload":{...},"created_at":"..."}],"next_cursor":"..."}
```

The alert detail shows the full decoded event payload — contract, event name,
ledger, transaction hash, topics, and the i128 amount.

In Slack (or whatever channel you configured), you should see a message summarizing
the alert: token transferred, amount, from/to addresses, and a link back to the
alert in the dashboard.

## Troubleshooting: no alert arrives

If you don't see an alert after a few poll intervals:

1. **Check the monitor is enabled:** Visit `/monitors` in the dashboard or use
   `PATCH /monitors/{id}` with `"enabled": true`. New monitors are enabled by
   default, but they can be toggled off.

2. **Check the rule:** Verify the rule is enabled and the params are correct.
   Use `GET /monitors/1/rules` to list rules and their status.

3. **Check the channel:** Use `POST /channels/{id}/test` to verify the channel
   can deliver messages. If the test fails, the webhook URL may be wrong or the
   recipient channel may be unavailable.

4. **Check the logs:** `docker compose logs -f sorobeacon` will show poller
   activity, rule evaluation, and dispatch attempts (successes and failures are
   both recorded).

5. **Verify the contract is being polled:** The poller only watches contracts
   that monitors are watching. If you recently created the monitor, the next
   poll cycle will pick it up.

6. **Check the network:** If `SOURCE_MODE=rpc`, the RPC_URL must point to the
   correct network (testnet by default) and the network passphrase must match.
   Use `GET /api/v1/health` — if `rpc` reports an error, the endpoint may be
   unreachable or on the wrong network.

## Real params JSON used

```json
{
  "event": "transfer",
  "min_amount": "1000000"
}
```

This config alerts on SEP-41 `transfer` events where the amount is at least
1,000,000 stroops (≈ 0.1 XLM). Omitted parameters (`from`, `to`, `max_amount`)
are not constrained, so any sender/recipient matching the contract will fire
the rule as long as the amount threshold is met.