# absence\_of\_event

Fires when an event you expect to keep arriving **stops arriving** — "alert me when my contract goes quiet for 30 minutes". It is the inverse of [`event_emitted`](event-emitted.md), and the only built-in rule type that is not evaluated against an incoming event.

## Params

```json
{
  "event_name": "heartbeat",
  "window": "30m"
}
```

| Field | Required | Meaning |
| --- | --- | --- |
| `event_name` | yes | The event that should keep arriving — matched against the event's **first topic**, exactly as `event_emitted` matches it. |
| `window` | yes | How long silence is tolerated, as a Go duration (`"30s"`, `"15m"`, `"2h"`). Must be greater than zero. |

The contract is not part of the params: the monitor already scopes which contracts are watched, so a monitor watching several contracts is quiet only when **none** of them emits the event.

## How it is measured

Unlike every other rule, an absence rule has no event to evaluate — the whole point is that the event never showed up. SoroBeacon keeps a **last-seen clock** per `(rule, event_name)` and compares it against the window:

1. The clock starts when the sweep first sees the rule. A brand-new rule does not alert on creation; the baseline is recorded so you are never told about silence that began before the rule existed.
2. Every arriving event whose name matches re-arms the clock (and only re-arms it — a matching event can never fire an absence rule).
3. The clock is stored in the database, so a restart resumes measuring from where it left off instead of resetting the silence.

The clock is **wall-clock observation time**, not the event's ledger close time — it works the same whether events come from Stellar RPC or upstream (SoroTrail) mode.

## One alert per silent window

The window id is derived from the instant silence began, so a silence that lasts hours produces exactly **one** alert, not one per sweep. The rule re-arms — and can fire again — only once the awaited event returns.

An absence alert has no ledger, transaction or matching event, so the message reports the silence instead:

```text
🔔 SoroBeacon alert: my-contract
Rule: absence_of_event (#3)
No heartbeat for 31m0s
Event ID: absence-1790078400000000000
At: 2026-09-22 12:31:00 UTC
```

The stored payload carries `event_name`, `window_seconds`, `last_seen_at`, `silent_for_seconds`, and `contract_ids`.

## Timing

The sweep runs after each successful poll cycle, so detection latency is up to `window` plus one poll interval — and the sweep is skipped while ingestion is failing, because "we could not look" is not "nothing happened". Set `window` comfortably larger than your poll interval. A 30-second window on a 30-second interval will be noisy.

## Examples

Alert when a keeper stops calling `heartbeat` for 15 minutes:

```sh
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "absence_of_event",
  "params": {"event_name": "heartbeat", "window": "15m"}
}'
```

Alert when an oracle stops publishing prices for two hours:

```sh
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "absence_of_event",
  "params": {"event_name": "price_updated", "window": "2h"}
}'
```

## Validation

Rejected with `400` at create/update time:

- a missing or blank `event_name`
- a missing, unparseable, zero or negative `window`
