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

## Bounded state and evaluation isolation

* **Bounded per-rule memory (`DefaultMaxFrequencyMatches = 10,000`)**: Each rule retains at most `10,000` timestamps in memory inside its rolling window (configurable via `WithMaxMatches`). If a wide window receives more matching events than this cap, the oldest timestamps are evicted so one noisy contract cannot grow memory without bound.
* **Atomic state updates & lock-free seeding**: Rebuilding a rule's window from `MatchLog` happens outside the state lock and commits atomically only after evaluation succeeds, so a slow database read or recovered panic never blocks other rules or leaves partial state behind.
* **Per-rule evaluation isolation & quarantine**: Every rule evaluation is bounded by `DefaultRuleEvalTimeout` (`100ms`). A panic or rule-attributed timeout is recovered, counted in Prometheus (`sorobeacon_rule_errors_total`, `sorobeacon_poll_panics_total`), and quarantines the rule after `DefaultQuarantineThreshold` (`3`) consecutive failures with an audit trail (`QuarantineAuditLog`) and recovery via `ReenableRule`. Time spent waiting on `MatchLog` is tracked separately (`RecordDependencyCall`) so database slowdowns never quarantine a rule.

## Escalation

A frequency alert is a good candidate for an [escalation policy](../guides/escalation.md): the first channel may be a chat room that is easy to miss during a storm, and a later step can reach a pager. The policy replaces the monitor's flat fan-out, so include every channel you still want told in its steps.
