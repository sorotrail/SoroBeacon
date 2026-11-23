# numeric\_range

Matches when a numeric value in an event falls **between two bounds** — "alert
when a transfer is between 1,000 and 10,000 tokens", "alert when the reported
price leaves its normal band". A range expresses in one rule what
`value_threshold` needs two rules for, and — with `outside` — it also
expresses exclusion without a second rule and a client-side negation.

## Params

```json
{
  "event_name": "price_update",
  "value_path": "price",
  "min": "5000000",
  "max": "7000000"
}
```

| Field | Required | Meaning |
| --- | --- | --- |
| `min` | one of `min`/`max` | Lower bound as a decimal integer **string** (or a JSON integer). There is no default. |
| `max` | one of `min`/`max` | Upper bound as a decimal integer **string** (or a JSON integer). There is no default. |
| `inclusive` | no | Whether a value equal to a bound counts as **inside**. Defaults to `true`. |
| `outside` | no | Invert the result: match when the value is **outside** the band. Defaults to `false`. |
| `value_path` | no | Dot path into the event value: map keys and array indexes, e.g. `amount`, `price.numerator`, `0`. Omit when the value itself is the number. |
| `event_name` | no | Only consider events with this name (first topic). |
| `cooldown` | no | Suppress repeat alerts from this rule for a window, e.g. `"5m"`. See [Rule cooldown](cooldown.md). |

At least one of `min` and `max` is required. A bound may be omitted to leave
that side open: `{"min": "1000000000000"}` alerts on every transfer at or
above one trillion stroops, and `{"max": "100"}` alerts on dust. When both
are set, `min` must not be greater than `max`. Invalid params are rejected at
create time with a `400`; see [the validation example](params.md#what-validation-failure-looks-like).

## Boundary semantics

`inclusive` and `outside` compose, and the composition is easiest to read as
two steps: first decide whether the value is *inside* the band, then invert
that answer when `outside` is set.

| Value | `inclusive` | `outside` | Matches? |
| --- | --- | --- | --- |
| equal to `min` or `max` | `true` | `false` | yes |
| equal to `min` or `max` | `false` | `false` | no — strictly inside is required |
| equal to `min` or `max` | `true` | `true` | no — the bounds are inside, so inverted is outside |
| equal to `min` or `max` | `false` | `true` | yes — the bounds are *not* inside, so inverted they match |
| between the bounds | either | `false` | yes |
| between the bounds | either | `true` | no |
| beyond the bounds | either | `false` | no |
| beyond the bounds | either | `true` | yes |

The last two rows are the point of `outside`: with
`{"min": "1000", "max": "10000", "outside": true}` both a 10-stroop dust
transfer and a 1,000,000-stroop whale transfer match, while the normal band
stays quiet.

## Why the bounds are decimal strings

Soroban token amounts are `i128` — up to 39 digits — which is far beyond
both `float64`'s exact-integer range (53 bits) and `int64`'s ceiling
(`9,223,372,036,854,775,807`). A bound that went through a float would be
silently rounded and could compare against the wrong integer.

So bounds are parsed as arbitrary-precision integers (`math/big.Int`), and a
**string** carries the full value exactly:

```json
{
  "event_name": "transfer",
  "min": "170141183460469231731687303715884105727"
}
```

That is `i128`'s maximum. As a JSON number it would arrive as
`1.7014118346046923e+38`; as a string it is exact. JSON integers within the
safe range are accepted too, but the string form is the one to use for token
amounts.

Event values are coerced to integers without passing through `float64`:
decoded `*big.Int` values, integer strings, JSON numbers, and the RPC's
single-key wrappers (`{"i128": "..."}`, `{"u64": 7}`) all work. A value that
is missing, non-numeric, or fractional simply **doesn't match** — live chain
data never makes a rule error.

## How `value_path` is resolved

When the contract exports a SEP-0048 spec, `value_path` first addresses the
spec's **named fields** (both topic-located and data-located parameters, e.g.
`amount`, `price.numerator`), so a rule can be written from the contract's
documentation instead of counting topic positions. A path the spec does not
name — and an omitted `value_path` — still falls back to the raw positional
value, so rules written before the spec was available keep working.

If the path resolves to something that is not an integer, the event is a
non-match rather than an error.

## Examples

A price band — fire only while the reported price is between 5 and 7 (in the
token's smallest unit):

```json
{
  "event_name": "price_update",
  "value_path": "price",
  "min": "5000000",
  "max": "7000000"
}
```

An "outside the normal range" alert — fire for both dust and whales, but not
for ordinary transfers:

```json
{
  "event_name": "transfer",
  "value_path": "amount",
  "min": "1000000",
  "max": "10000000",
  "outside": true
}
```

A strict, open-ended band — fire only above one trillion, and not at exactly
one trillion:

```json
{
  "event_name": "transfer",
  "value_path": "amount",
  "min": "1000000000000",
  "inclusive": false
}
```

## How it differs from `value_threshold`

`value_threshold` compares one value against one threshold with an operator
(`gt`, `gte`, `lt`, `lte`, `eq`, `neq`) and handles arbitrary decimals with
`big.Float`. `numeric_range` compares one integer value against **two**
bounds at once and adds the `outside` inversion:

* "between 1k and 10k" is one `numeric_range`, or two `value_threshold`
  rules that must both be combined and negated by hand.
* "outside 1k–10k" is one `numeric_range` with `outside: true`, rather than
  a composite rule.
* Bounds and values are integers only. For fractional thresholds (prices
  reported as decimals), use [`value_threshold`](value-threshold.md).
