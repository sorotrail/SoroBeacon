# Rule params reference

Every param of every shipped rule type in one place, taken from the
validators in `internal/rules` (the `Validate` and `parse*` functions there
are the authority). Each rule also has a page of its own with matching
semantics and more examples — [event\_emitted](event-emitted.md),
[value\_threshold](value-threshold.md), [token\_event](token-event.md),
[frequency\_threshold](frequency-threshold.md), [topic\_regex](topic-regex.md),
[address\_watchlist](address-watchlist.md), [topic\_position](topic-position.md),
and the cross-cutting [cooldown](cooldown.md).

## Conventions that apply to every rule type

* **The monitor scopes the contract.** No rule param names a contract ID;
  the rule only ever sees events from the monitor it belongs to.
* **Unknown params are ignored** by evaluators (that is how `cooldown` rides
  along), but a malformed JSON body is rejected.
* **Cooldown applies to all types.** The optional `cooldown` param is
  validated once for every rule type by `Registry.Validate`
  (`internal/rules/rules.go`), not by each evaluator.

| Param | JSON type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `cooldown` | string | no | unset (no cooldown) | Go duration (`"5m"`, `"90s"`); suppresses repeat alerts from the rule for the window. Must parse and be non-negative. See [Rule cooldown](cooldown.md). |

## Numeric amounts are decimal strings

Soroban token amounts are `i128` — up to 39 digits — far beyond both
float64's exact-integer range (53 bits, `9,007,199,254,740,992`) and
`int64`'s ceiling of `9,223,372,036,854,775,807`. An amount of 100 trillion
tokens in stroop (`10^21`, 22 digits) is already outside `int64`, and would
be silently rounded if it ever touched a float.

So wherever an amount is involved, params use a **decimal string** and the
comparison runs on arbitrary-precision integers end to end. The decoded
event value has the same shape — an i128 arrives as
`{"i128": "170141183460469231731687303715884105727"}` — so what you write is
what you compare.

An example large enough to show why — a threshold a float64 would corrupt:

```json
{"event": "transfer", "min_amount": "170141183460469231731687303715884105727"}
```

That is `i128`'s maximum. As a JSON number it would arrive as
`1.7014118346046923e+38` and compare against the wrong integer; as a string
it is exact.

`value_threshold`'s `threshold` is the one place both work: a JSON number is
accepted, but for anything beyond 53 bits — every full-precision token
amount — pass the number as a **string**.

## event\_emitted

Matches by event name (the first topic) and/or exact topic values. At least
one of the two must be set.

| Param | JSON type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `event_name` | string | one of the two | unset | Matches the event's first topic (the event name, by convention). |
| `topic_equals` | object | one of the two | unset | Map of topic **index** (string key, e.g. `"1"`) → expected value. Index `0` is the event name; user topics start at `1`. Keys must parse as integers. All entries must match. |

Complete, valid params document:

```json
{
  "event_name": "transfer",
  "topic_equals": {"1": "GDW6AUTBXTOC7FIKUO5BOO3OGLK4SF7ZPOBLMQHMZDI45J2Z6VXRB5NR"}
}
```

## value\_threshold

Numeric comparison against the event's decoded value (or a path into it).

| Param | JSON type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `comparison` | string | yes | — | One of `gt`, `gte`, `lt`, `lte`, `eq`, `neq`. |
| `threshold` | number or string | yes | — | The comparison value. JSON number, or **string** for integers beyond 53 bits. Parsed as a decimal number in arbitrary precision. |
| `event_name` | string | no | unset (all events) | Only consider events with this name (first topic). |
| `value_path` | string | no | unset (the value itself) | Dot path into the value: map keys and array indexes (`amount`, `price.numerator`, `1.price`). When the contract has a SEP-0048 spec, the path addresses the spec's named fields first, falling back to the raw positional value. A path that doesn't resolve — or resolves to something non-numeric — simply doesn't match; it is not an error. |

Complete, valid params document:

```json
{
  "event_name": "transfer",
  "value_path": "amount",
  "comparison": "gt",
  "threshold": "1000000000"
}
```

## token\_event

SEP-41 token events, with the interface's topic layout built in.

| Param | JSON type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `event` | string | yes | — | `transfer`, `mint`, `burn`, `clawback`, `set_admin`, or `*` (any of the amount-bearing ones). |
| `from` | string | no | unset (any) | Exact address in the outgoing slot — the sender on `transfer`, the holder on `burn`/`clawback`, the admin on `mint`, the old admin on `set_admin`. |
| `to` | string | no | unset (any) | Exact address in the incoming slot. |
| `min_amount` | string | no | unset (no bound) | Inclusive lower bound on the i128 amount, as a decimal string. |
| `max_amount` | string | no | unset (no bound) | Inclusive upper bound on the i128 amount, as a decimal string. |

All filters combine with AND; omitted filters don't constrain. Amount params
must be decimal integers (a leading `-` is accepted by the parser, though
real token amounts are non-negative), and `set_admin` — which carries no
value — rejects them outright.

Complete, valid params document:

```json
{
  "event": "transfer",
  "from": "GDW6AUTBXTOC7FIKUO5BOO3OGLK4SF7ZPOBLMQHMZDI45J2Z6VXRB5NR",
  "min_amount": "1000000000"
}
```

## topic\_regex

RE2 pattern match against a decoded topic — at a position, or any topic.
See the [rule page](topic-regex.md) for matching semantics.

| Param | JSON type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `pattern` | string | yes | — | Go/RE2 regular expression, matched unanchored within a topic's string value. At most 512 bytes. |
| `position` | number | no | unset (any topic) | Topic index to match; `0` is the event name, user topics start at `1`. A position outside the event's topic list simply doesn't match. |

Complete, valid params document:

```json
{
  "pattern": "^swap_",
  "position": 0
}
```

## address\_watchlist

SEP-41 events whose from or to address appears on a configured watchlist.
See the [rule page](address-watchlist.md) for matching semantics.

| Param | JSON type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `addresses` | array of string | yes | — | Non-empty list of Stellar addresses (account `G...` or contract `C...` strkeys). At most 1024 entries. |
| `match` | string | no | `either` | Which slot(s) to watch: `from`, `to`, or `either`. |
| `event` | string | no | unset (all SEP-41 events) | Restrict to one SEP-41 event: `transfer`, `mint`, `burn`, `clawback`, `set_admin`, or `*`. |

Matching is exact and case-sensitive — no partial or prefix matching. The
address set is built once and memoised, so a long watchlist does not scan
linearly per event.

Complete, valid params document:

```json
{
  "addresses": ["GDW6AUTBXTOC7FIKUO5BOO3OGLK4SF7ZPOBLMQHMZDI45J2Z6VXRB5NR"],
  "match": "either",
  "event": "transfer"
}
```

## frequency\_threshold

Rolling-window aggregate: "more than N matching events within M minutes".
See the [rule page](frequency-threshold.md) for re-arm semantics.

| Param | JSON type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `count` | number | yes | — | Fire when this many matching events fall inside `window`. Must be a positive integer. |
| `window` | string | yes | — | Rolling window as a Go duration (`"5m"`, `"90s"`, `"1h"`). Must parse and be positive. |
| `event_name` | string | no | unset (count everything) | Count only events with this name (first topic). |

Complete, valid params document:

```json
{
  "event_name": "transfer",
  "count": 50,
  "window": "5m"
}
```

## topic\_position

Matches when the decoded topic at a fixed position exactly equals a configured
value — the question custom (non-SEP-41) contracts raise with their
positioned topics (a pool ID, a market symbol, an account), which
`event_emitted` (first topic only) and `token_event` (SEP-41 slots only)
cannot ask.

| Param | JSON type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `position` | number | yes | — | Topic index to compare; `0` is the event-name topic. |
| `equals` | string | yes | — | Exact value the decoded topic must equal. |
| `event` | string | no | — | Also require this event name (first topic). |

The comparison is exact string equality against the topic's **decoded string
form**: string topics compare as themselves, numeric topics by their decimal
rendering (`{"i128": "1000000"}` and `42` both compare as `"1000000"`-style
decimal strings), addresses by their strkey. Matching is case-sensitive; a
position beyond the event's topic count is a non-match, never an error.

Complete, valid params document:

```json
{
  "position": 2,
  "equals": "POOL_USDC_XLM",
  "event": "deposit"
}
```

## What validation failure looks like

Rules are validated at create (`POST /api/v1/monitors/{id}/rules`) and
update time; the registry calls the evaluator's `Validate` and then checks
`cooldown`. Failures are HTTP `400` with the API's structured error envelope
(`internal/api` `writeValidation`):

* the top-level `error` is the **first** problem's reason when there is one,
  or the summary `"validation failed"` when there are several;
* `details` lists **every** problem, each as a `field`/`reason` pair, with
  the field path prefixed under `params` (and `rules[i].params.…` in a bulk
  request);
* `code` is the HTTP status text and `request_id` echoes the
  `X-Request-ID` header, so a reported error maps to one log line.

For example, a `frequency_threshold` with a bad `window` and no `count`:

```json
{
  "error": "validation failed",
  "code": "Bad Request",
  "request_id": "b1c8f5a2-…",
  "details": [
    {"field": "params.count",  "reason": "frequency_threshold: count must be a positive integer"},
    {"field": "params.window", "reason": "frequency_threshold: invalid window \"five minutes\" (want a Go duration such as \"5m\")"}
  ]
}
```

An unknown rule type reports on `type` instead (one detail, and the top-level
`error` carries the reason):

```json
{
  "error": "unknown rule type \"cooldown\" (registered: [event_emitted value_threshold token_event frequency_threshold topic_regex address_watchlist topic_position])",
  "code": "Bad Request",
  "request_id": "…",
  "details": [
    {"field": "type", "reason": "unknown rule type \"cooldown\" (registered: [event_emitted value_threshold token_event frequency_threshold topic_regex address_watchlist topic_position])"}
  ]
}
```
