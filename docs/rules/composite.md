# composite

Combines child rules with a boolean operator — `and`, `or` or `not`. A composite is what makes "a large transfer **and** the recipient is on my watchlist" one rule instead of two monitors that a human has to correlate.

## Params

```json
{
  "op": "and",
  "rules": [
    {"type": "token_event", "params": {"event": "transfer", "min_amount": "1000000"}},
    {"type": "event_emitted", "params": {"topic_equals": {"1": "GDW6...SENDER"}}}
  ]
}
```

| Field | Required | Meaning |
| --- | --- | --- |
| `op` | yes | `and`, `or`, or `not`. `not` is unary. |
| `rules` | yes | The child rules, each `{"type": ..., "params": {...}}`. `and`/`or` need at least one; `not` needs exactly one. |
| `cooldown` | no | Suppress repeat alerts from the composite for a window. See [Rule cooldown](cooldown.md). |

Children are ordinary rule params validated through the registry, so a composite accepts every rule type the registry knows — including another `composite`, up to the nesting limit below.

## Matching semantics

* **`and`** matches when every child matches; it stops at the first child that doesn't.
* **`or`** matches when any child matches; it stops at the first child that does.
* **`not`** matches when its single child does not.
* Evaluation short-circuits, so a child whose result cannot change the outcome is never evaluated. A stateful child such as `frequency_threshold` therefore only counts the events it actually gets asked about.

## Validation

* Every child is validated recursively at create time. An unknown child type is a **validation error**, never a runtime panic.
* A malformed child is reported with the path to it, so a bad grandchild names itself exactly — for example `rules[1].params` or `rules[0].params.rules[0].params`.
* Nesting is capped at **five composite levels**. A sixth level is rejected with `composite: rules may not nest more than 5 levels deep`, which bounds validation and evaluation on user-supplied params.
* `cooldown` applies to the composite rule itself; a `cooldown` on an inline child has no effect, because children are not stored as rules.

## Examples

A `transfer` of at least 1,000,000 from a watched sender:

```json
{
  "op": "and",
  "rules": [
    {"type": "token_event", "params": {"event": "transfer", "min_amount": "1000000"}},
    {"type": "event_emitted", "params": {"topic_equals": {"1": "GDW6AUTBXTOC7FIKUO5BOO3OGLK4SF7ZPOBLMQHMZDI45J2Z6VXRB5NR"}}}
  ]
}
```

Anything that is *not* a `mint` or `burn` — a `not` around an `or`:

```json
{
  "op": "not",
  "rules": [
    {"type": "composite", "params": {"op": "or", "rules": [
      {"type": "event_emitted", "params": {"event_name": "mint"}},
      {"type": "event_emitted", "params": {"event_name": "burn"}}
    ]}}
  ]
}
```
