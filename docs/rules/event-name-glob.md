# event\_name\_glob

Matches when the **event's name** — the first topic, the conventional
Soroban event name — fits a glob pattern.

`event_emitted` compares the event name for exact equality, so an operator
watching a DEX that emits `swap_exact_in`, `swap_exact_out`, `pool_deposit`,
… needs one rule per name. `event_name_glob` covers the whole family with a
single pattern in a syntax operators write correctly on the first try:
`swap_*`.

## Params

```json
{
  "patterns": ["swap_*", "pool_deposit"]
}
```

| Field | Required | Meaning |
| --- | --- | --- |
| `patterns` | yes | Non-empty list of glob patterns. The rule matches when **any** of them matches the event name. |
| `cooldown` | no | Suppress repeat alerts from this rule for a window, e.g. `"5m"`. See [Rule cooldown](cooldown.md). |

## Glob syntax

Patterns use Go's [`path.Match`](https://pkg.go.dev/path#Match) syntax:

| Pattern | Matches |
| --- | --- |
| `*` | Any run of characters that are not the path separator (`/`). |
| `?` | Exactly one character that is not the path separator. |
| `[abc]` | One character from the class; `[a-z]` is a range, `[^abc]` excludes. |
| `\*` | A literal `*` (the backslash escapes the next character). |

Event names never contain `/`, so the separator rule never bites in
practice: `swap_*` matches every event name that starts with `swap_`.

## Matching semantics

* **The match is anchored at both ends.** The *whole* name has to fit the
  pattern, so `swap` does **not** match `swap_exact_in` — a substring match
  is the mistake people make with this rule. `swap_*` does.
* **Matching is case-sensitive.** `Swap_*` does not match `swap_exact_in`.
* **Only the event name is matched.** The rule looks at the first topic
  only; the other topics are not considered. To match a pattern *anywhere*
  in the topics, use [`topic_regex`](topic-regex.md).
* **Several patterns are ORed.** The rule matches when any one of them
  matches; there is no "all patterns" mode.
* **An event with no name is a non-match, not an error.** An event with no
  topics, or whose first topic is not a symbol, simply does not match.
* Both decoded topic shapes work: the bare strings the XDR path produces and
  the single-key wrappers (`{"symbol": "swap_exact_in"}`) the RPC's
  `xdrFormat: "json"` path produces.

## Glob or regex?

| Use | When |
| --- | --- |
| [`event_name_glob`](event-name-glob.md) | The event **name** matches a family: `swap_*`, `pool_*`, `*_deposit`. |
| [`topic_regex`](topic-regex.md) | The pattern has to match **any** topic, or a topic at a fixed position, and needs more than a glob: alternation (`swap_\|pool_`), anchors inside the pattern, character classes with ranges. |

A regex is unanchored by default, which is the opposite of a glob and the
usual source of surprise: `^swap_` in `topic_regex` is the equivalent of
`swap_*` in `event_name_glob`.

## Performance and the RPC topic filter

A glob match is cheap, but the rule does not narrow what the poller
fetches. Some rule types can name the concrete events they match
([`token_event`](token-event.md), [`topic_position`](topic-position.md),
[`value_threshold`](value-threshold.md),
[`frequency_threshold`](frequency-threshold.md)), and the poller then asks
the RPC only for those events. A glob stands for a *family* of names, so
`event_name_glob` cannot: the whole contract is fetched and every event is
matched locally.

Because the rules of a monitor are combined, one rule that cannot name its
events leaves the **whole contract** unfiltered — so a monitor that mixes
an `event_name_glob` with a `token_event` still fetches everything the
contract emits. On a busy contract that is bandwidth the filter would have
saved, and it is worth knowing before adding the rule to a monitor that
already names its events.

## Validation

Rules are validated at create/update time (`POST /api/v1/monitors/{id}/rules`),
so a bad pattern is rejected with HTTP `400` before any event is evaluated:

* `patterns` is required and must not be empty —
  `event_name_glob: patterns is required (a non-empty list of glob patterns)`;
* every pattern must be a well-formed glob —
  `event_name_glob: patterns[0] "[" is malformed: syntax error in pattern`.

## Examples

Every event of the swap family:

```sh
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "event_name_glob",
  "params": {
    "patterns": ["swap_*"]
  }
}'
```

Two families at once:

```json
{
  "type": "event_name_glob",
  "params": {
    "patterns": ["swap_*", "pool_*"]
  }
}
```

One exact name and one family:

```json
{
  "type": "event_name_glob",
  "params": {
    "patterns": ["pool_deposit", "pool_withdraw"]
  }
}
```

A single character of difference, matched with `?`:

```json
{
  "type": "event_name_glob",
  "params": {
    "patterns": ["swap_?"]
  }
}
```
