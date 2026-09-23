# time_window

Matches when an event's **ledger close time** falls inside (or, with
`outside: true`, outside) a recurring **UTC** window.

"Alert me only if this happens outside business hours" is a standard
monitoring requirement, and for contract activity it is a genuine signal: a
treasury movement at 3am on a Sunday deserves different attention than one at
2pm on a Tuesday.

## Parameters

```json
{
  "start": "09:00",
  "end": "17:00",
  "days": ["mon", "tue", "wed", "thu", "fri"],
  "outside": true
}
```

| Parameter | Required | Description |
|---|---|---|
| `start` | yes | Window start, `HH:MM` in UTC. Inclusive. |
| `end` | yes | Window end, `HH:MM` in UTC. Exclusive. |
| `days` | no | Day names to include. Omit for every day. Empty array is rejected. |
| `outside` | no | When `true`, match events **not** in the window. Default `false`. |

The window is **half-open**: an event exactly at `start` is inside it, an
event exactly at `end` is outside it.

## Time source

The rule evaluates against `DecodedEvent.LedgerClosedAt` — the ledger close
time of the event — not wall-clock `time.Now()`. Replayed or backfilled
events therefore evaluate against when they actually happened, which is the
whole point of a time window.

Events with no ledger close time (synthetic or malformed) never match.

## Crossing midnight

A window whose `end` is earlier than its `start` crosses midnight rather
than being rejected, so `22:00`–`06:00` works as expected.

## Timezones

Everything is UTC. Per-monitor timezones are deliberately out of scope; if
your team is not on UTC, convert your local window to UTC before configuring
the rule.

## Combining with other rules

A `time_window` rule is meant to be read alongside your other rules: today,
several rules on one monitor are evaluated independently and each match
alerts. A dedicated composite (AND/OR) rule type would let you gate a
`token_event` on a `time_window` in a single rule; until that exists, use
`outside: true` and a scoped monitor to get the common "only page me at
night" behaviour.

## Validation

Rules that can never match, or are ambiguous, are rejected at create time:

- a `start` or `end` that is not a strict `HH:MM` value
- an unknown day name in `days`
- an empty `days` array (omit the key to match every day)

## Examples

Page on any activity outside the Monday–Friday 09:00–17:00 UTC window:

```sh
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "time_window",
  "params": {
    "start": "09:00",
    "end": "17:00",
    "days": ["mon", "tue", "wed", "thu", "fri"],
    "outside": true
  }
}'
```

Only fire during an overnight maintenance window:

```sh
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "time_window",
  "params": {"start": "22:00", "end": "06:00"}
}'
```
