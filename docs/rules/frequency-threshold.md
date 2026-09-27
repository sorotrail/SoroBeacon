# frequency\_threshold

Fires when the **count of matching events inside a rolling window** reaches a threshold — "more than 50 transfers within 5 minutes", the shape of a mint storm, a drain attack or an oracle that keeps flapping. It complements [`value_threshold`](value-threshold.md), which judges a single event: frequency matters for conditions that are unremarkable one event at a time but abuse in aggregate.

## Params

```json
{
  "event_name": "transfer",
  "count": 50,
  "window": "5m"
}
```

| Field | Required | Meaning |
| --- | --- | --- |
| `count` | yes | Fire when this many matching events fall inside `window`. Must be a positive integer. |
| `window` | yes | Rolling window as a Go duration (`"5m"`, `"90s"`, `"1h"`). Must be positive. |
| `event_name` | no | Count only events with this name (first topic). Omit to count every event. |
| `cooldown` | no | The cross-cutting suppression window, applied on top. See [Rule cooldown](cooldown.md). |

## Re-arm semantics

The question every rule author asks is *"when does it fire again?"* The answer
is deliberately conservative:

* The rule fires **once** each time the count **crosses** the threshold.
* After firing it stays quiet for **one full `window`**. A sustained condition
  — the count keeps meeting the threshold — therefore alerts **at most once
  per window**, never once per event.
* When the window has elapsed and the count is met again, the rule fires once
  more with a new alert. If the burst has passed and the count is below the
  threshold, nothing fires; the rule is simply re-armed for next time.

Matches that fall outside the rolling window are dropped as new ones arrive, so
a slow trickle (e.g. one event every 40 seconds against `count: 3, window:
"1m"`) can never accumulate into a false positive.

## One alert per episode

A firing is stored under a **synthetic `event_id`** derived from the window
start (`frequency:<timestamp>`), not the event that happened to cross the
threshold. Every crossing inside one episode therefore maps to the same
`(rule_id, event_id)` dedup key, so a replayed event or a poller restart
cannot duplicate the alert.

The rolling window lives in memory, keyed per rule, and is **rebuilt from the
`alerts` table** the first time the rule is evaluated after a restart — the
count and the "already fired" state survive the process instead of silently
resetting.
