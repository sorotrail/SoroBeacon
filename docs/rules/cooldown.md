# Rule cooldown

A busy contract plus a broad rule — `event_emitted` on `transfer` for a
popular token — produces one alert per event: a firehose that can exceed a
channel's rate limit and get the integration throttled or blocked. The
optional `cooldown` param suppresses repeat alerts from a rule for a window
after it fires, and reports how many matches it swallowed so the cooldown
never hides the scale of what happened.

`cooldown` is **cross-cutting**: it works with every rule type
([event_emitted](event-emitted.md), [value_threshold](value-threshold.md),
[token_event](token-event.md), [frequency_threshold](frequency-threshold.md))
and sits alongside that rule's own params.

## Params

| Field | Required | Meaning |
| --- | --- | --- |
| `cooldown` | no | A Go duration string (`"30s"`, `"5m"`, `"1h"`). Omit for no cooldown. |

## Example

Alert at most once every five minutes for large transfers:

```sh
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "token_event",
  "params": {"event": "transfer", "min_amount": "1000000000", "cooldown": "5m"}
}'
```

## Behaviour

* The first match in a window fires normally and opens the window. Every
  further match from that rule inside the window is **counted and dropped** —
  no alert row, no delivery, and no channel rate limit spent.
* When the window closes, the next match fires and its payload (and the
  notification built from it) carries **`suppressed_since_last`**: how many
  matches the previous window dropped. So an operator sees the burst instead
  of a silent gap.
* A rule without `cooldown` behaves exactly as before: every match alerts.

### Where the clock starts: the last alert, not the last match

The window is anchored to the rule's **last firing**, not its last match.
`rules.last_alert_at` is written **only when an alert row is created**; a
suppressed match increments the counter and leaves the anchor untouched.
The consequence is worth stating plainly: a suppressed match never extends
or reopens the window, so a sustained burst cannot push the next alert out
forever — it alerts once per window measured from the last firing, and each
alert reports everything that window swallowed.

The window is compared against the database clock (`now()`), not the event's
ledger time, so a replayed or backfilled event cannot reopen a stale window.

### Scope: per rule, not per monitor

Cooldown state lives on the **rule row** (`rules.last_alert_at`,
`rules.suppressed_since_last`), keyed by `rule_id`. It is per rule: two rules
on the same monitor each keep their own window and counter, so a cooldown on
one rule never suppresses another. It is not per monitor, per channel or per
event — when a rule fires, its alert still fans out to every channel the
monitor is attached to.

### The fate of a suppressed match — stated plainly

A suppressed match is **not stored**. There is no alert row, no payload and no
delivery attempt for it, so the API cannot later list *which* events were
suppressed. What survives is only:

* an integer counter on the rule (incremented per suppressed match, reset to
  zero when the rule next fires), surfaced on the next alert as
  `suppressed_since_last`, and
* a single `alert suppressed by cooldown` line per suppressed match at the
  poller's `info` log level (the default).

If you need the individual events themselves, don't put a `cooldown` on that
rule — or read the poller logs, which are the only per-event record.

### Durability and concurrency

The window and counter are enforced in the database next to the dedup guard,
under the same `SELECT … FOR UPDATE` row lock on the rule. That means the
state survives a poller restart and two pollers racing on the same rule still
produce exactly one alert per window.

Replayed events are still deduplicated: the same `(rule, event)` never alerts
twice and never inflates the suppressed count.

## Worked example

One rule on one monitor — a `token_event`/`transfer` rule with
`"cooldown": "5m"` — and a burst of six matching transfers:

| Time | Match | Result |
| --- | --- | --- |
| `T+0m` | `ev-1` | **Alert #1 fires.** Window opens until `T+5m`. |
| `T+0m` | `ev-2`, `ev-3`, `ev-4` | Suppressed. Counter = 3. |
| `T+4m` | `ev-5` | Still inside the window → suppressed. Counter = 4. |
| `T+5m` | — | Window elapses. |
| `T+6m` | `ev-6` | **Alert #2 fires**, reporting `suppressed_since_last: 4` (`ev-2`…`ev-5`). Counter resets to 0 and a new window runs to `T+11m`. |

Note that `ev-5` arriving at `T+4m` does **not** move the `T+5m` boundary:
suppressed matches never reset the clock. Also note that the alert that opened
the window (`ev-1`) is not counted as suppressed if it is delivered again — a
replay is a duplicate, not a new match.

## When cooldown is the wrong tool

Cooldown paces *how often* a rule alerts; it does not change *what* the rule
matches and it does not summarise events in real time. Reach for something
else when:

* **You care about rate or volume** — mint storms, drain attacks, oracle
  flapping. Use [`frequency_threshold`](frequency-threshold.md): it detects
  "more than N in M minutes" and fires on the aggregate, whereas cooldown
  only quiets a rule that already matched every event.
* **The rule is too broad and matches benign traffic.** Narrow the rule
  (`event_name`, `topic_equals`, `min_amount`) instead of masking it. Cooldown
  still *counts* those benign matches and reports them in
  `suppressed_since_last`, which muddies the signal you actually want.
* **Several different rules are each noisy.** Cooldown is per rule, so each
  needs its own window; it will not throttle a monitor or a channel across
  rules.
* **You need to see every matching event.** Cooldown's whole purpose is to
  drop them; there is no "alert once but list the rest" mode. Raise the rule's
  specificity, or split the rule.

(Don't confuse this with the delivery **retry** cooldown on
`POST /alerts/{id}/deliveries`, which rate-limits manually re-sending one
alert to a channel. That is a separate mechanism with its own 30s bound.)

## Validation

A `cooldown` that is not a valid, non-negative duration is rejected when the
rule is created or updated (HTTP `400` on `params.cooldown`), so a typo can't
silently leave a busy rule with no suppression.

## Where this is enforced

For contributors tracing a claim back to the code:

| Claim | Established by |
| --- | --- |
| Param parsing, "duration string", non-negative validation | `internal/rules/cooldown.go` (`ParseCooldown`) |
| Cross-cutting: validated for every rule type | `internal/rules/rules.go` (`Registry.Validate`, `NewRegistry`) |
| Per-rule state columns | `internal/store/migrations/0006_rule_cooldown.up.sql` |
| Window anchored to last firing; counted-and-dropped; `FOR UPDATE`; reset on fire; dedup not counted | `internal/store/postgres.go` (`CreateAlert`) |
| Suppressed match not dispatched; `alert suppressed by cooldown` log line | `internal/poller/poller.go` (`fireAlert`) |
| `suppressed_since_last` folded into the alert payload | `internal/store/postgres.go` (`WithSuppressed`) |
| Behaviour pinned by tests | `internal/rules/cooldown_test.go`, `internal/poller/poller_test.go`, `internal/store/postgres_test.go`, `internal/api/validation_test.go` |
