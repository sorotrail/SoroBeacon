# topic\_regex

Matches when a **regular expression** matches a decoded topic — the topic at a
given position, or **any topic** when no position is given.

`event_emitted` compares topics for exact equality, so an operator wanting
"any swap" on a DEX that emits `swap_exact_in`, `swap_exact_out`,
`pool_deposit`, … needs one rule per event name. A `topic_regex` rule covers
the whole family with one pattern — and the long tail of custom contracts
whose topic conventions SoroBeacon cannot know in advance.

## Params

```json
{
  "pattern": "^swap_",
  "position": 0
}
```

| Field | Required | Meaning |
| --- | --- | --- |
| `pattern` | yes | Go/RE2 regular expression matched **within** a topic's string value (unanchored, like `regexp.MatchString`). |
| `position` | no | Topic index to match. Index `0` is the event name; user topics start at `1`. **Omitted matches any topic.** |
| `cooldown` | no | Suppress repeat alerts from this rule for a window, e.g. `"5m"`. See [Rule cooldown](cooldown.md). |

## Matching semantics

* Patterns are **unanchored**: `"swap"` matches `swap_exact_in` anywhere in
  the topic. Anchor with `^`/`$` when you want prefix/suffix semantics —
  `"^swap_"` matches the family but not `"unswap"` (which `"swap"` would).
* A `position` **outside the event's topic list simply doesn't match** — it is
  not an error. An event with fewer topics than the rule's position just never
  fires that rule.
* Only topics that decoded to a **string** can match: symbols, strings and
  addresses arrive as strings. Integer, byte and vector topics never match a
  pattern; they are skipped, not errors, so a rule can sweep every topic
  safely.
* Both decoded topic shapes work: the bare strings the XDR path produces and
  the single-key wrappers (`{"symbol": "transfer"}`,
  `{"address": "G..."}`) the RPC's `xdrFormat: "json"` path produces.
* The pattern is evaluated with Go's `regexp` (RE2). Lookarounds and
  backreferences are not supported; alternation (`swap_|pool_`) is.

## Validation

Rules are validated at create/update time (`POST /api/v1/monitors/{id}/rules`),
so a bad pattern is rejected with HTTP `400` before any event is evaluated:

* `pattern` is required and must compile as a RE2 expression;
* `pattern` must be at most **512 bytes** (`MaxTopicRegexPatternLength` in
  `internal/rules/topic_regex.go`). The cap keeps a pathological or
  machine-generated pattern from stalling the poller's per-event evaluation;
* `position`, when given, must not be negative.

## Examples

Any event in the swap family:

```json
{"pattern": "^swap_"}
```

Only events whose name (topic 0) is in the family — ignoring address topics
that happen to contain the same text:

```json
{"pattern": "^swap_", "position": 0}
```

Any topic holding a specific pool contract:

```json
{"pattern": "^C poolid"}
```

Everything a vault contract emits, by its namespaced convention:

```json
{"pattern": "^vault\\."}
```
