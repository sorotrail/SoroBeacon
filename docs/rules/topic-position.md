# topic\_position

Matches when the decoded topic at a **fixed position** **exactly equals** a
configured value, optionally restricted to one event name.

Custom (non-SEP-41) contracts put meaningful values in fixed topic positions:
a pool ID, a market symbol, an account. `event_emitted` only compares the
event name, and `token_event` only understands SEP-41's address slots, so
watching "position 2 equals this pool ID" previously needed a regex hack or a
code change. A `topic_position` rule asks that question directly:

```json
{
  "position": 2,
  "equals": "POOL_USDC_XLM",
  "event": "deposit"
}
```

## Params

| Field | Required | Meaning |
| --- | --- | --- |
| `position` | yes | Topic index to compare. Index `0` is the event name; user topics start at `1`. |
| `equals` | yes | Exact value the decoded topic must equal. |
| `event` | no | Also require this event name (the first topic), like `event_emitted`'s `event_name`. |
| `cooldown` | no | Suppress repeat alerts from this rule for a window, e.g. `"5m"`. See [Rule cooldown](cooldown.md). |

## Matching semantics

* The comparison is **exact string equality** against the topic's decoded
  string form — there is no substring, prefix or case-insensitive matching.
  For the pattern questions, use [`topic_regex`](topic-regex.md) instead.
* **String topics compare as themselves**: symbols, strings and addresses
  arrive as strings, so `"equals": "GA..."` matches the address topic by its
  strkey text.
* **Numeric topics compare by their decoded string form**: the decoder
  renders every integer width (`u32`/`i32`/`u64`/`i64`/`u128`/`i128`/
  `u256`/`i256`) as a decimal string, so a topic carrying `{"i128":
  "1000000"}` matches `"equals": "1000000"` and never its scientific or
  hex notation.
* Both decoded topic shapes work: the bare values the XDR path produces and
  the single-key wrappers (`{"symbol": "deposit"}`, `{"address": "G..."}`,
  `{"i128": "1000000"}`) the RPC's `xdrFormat: "json"` path produces.
* A `position` **outside the event's topic list simply doesn't match** — it
  is not an error. An event with fewer topics than the rule's position just
  never fires that rule, so one rule can sit on events of varying arity.
* `event`, when set, filters before the position check: an event whose first
  topic is not that name never matches, even if another topic carries the
  configured value.

## Validation

Rules are validated at create/update time (`POST /api/v1/monitors/{id}/rules`),
so bad params are rejected with HTTP `400` before any event is evaluated:

* `position` must not be negative;
* `equals` must be non-empty.

There is no upper bound on `position`: the event's topic count is not known
at create time, and a position beyond it is a rule that matches nothing, not
an invalid rule.

## Examples

Only deposits into one specific pool:

```json
{"position": 2, "equals": "POOL_USDC_XLM", "event": "deposit"}
```

Any event whose topic 1 carries a market symbol:

```json
{"position": 1, "equals": "XLM/USDC"}
```

Pinning the exact event name via position 0 (what `event_emitted`'s
`event_name` does, combined with the position check):

```json
{"position": 0, "equals": "deposit"}
```

A numeric topic by its decimal form (a fixed fee or limit in topic 1):

```json
{"position": 1, "equals": "1000000"}
```
