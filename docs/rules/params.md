# Rule params reference

Every param of every shipped rule type in one place, taken from the
validators in `internal/rules` (the `Validate` and `parse*` functions there
are the authority). Each rule also has a page of its own with matching
semantics and more examples — [event\_emitted](event-emitted.md),
[value\_threshold](value-threshold.md), [token\_event](token-event.md),
[frequency\_threshold](frequency-threshold.md),
[composite](composite.md), and the cross-cutting [cooldown](cooldown.md).

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

## composite

Combines child rules with a boolean operator. Children are full rules
(`type` + `params`) validated recursively through the registry; nesting is
capped at five levels.

| Param | JSON type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `op` | string | yes | — | One of `and`, `or`, `not`. |
| `rules` | array | yes | — | Child rules, each `{"type": ..., "params": {...}}`. `and`/`or` need at least one; `not` needs exactly one. |

Complete, valid params document:

```json
{
  "op": "and",
  "rules": [
    {"type": "token_event", "params": {"event": "transfer", "min_amount": "1000000"}},
    {"type": "event_emitted", "params": {"topic_equals": {"1": "GDW6AUTBXTOC7FIKUO5BOO3OGLK4SF7ZPOBLMQHMZDI45J2Z6VXRB5NR"}}}
  ]
}
```

A child error is reported with the path to it, for example
`rules[1].params.comparison`, so a malformed grandchild names its exact
location.

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
  "error": "unknown rule type \"cooldown\" (registered: [event_emitted value_threshold token_event frequency_threshold composite])",
  "code": "Bad Request",
  "request_id": "…",
  "details": [
    {"field": "type", "reason": "unknown rule type \"cooldown\" (registered: [event_emitted value_threshold token_event frequency_threshold composite])"}
  ]
}
```
