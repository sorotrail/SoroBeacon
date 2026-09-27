# value\_threshold

Matches when a numeric value in an event's **data** crosses a threshold — "alert when a transfer exceeds 1M tokens", "alert when the reported price drops below X".

## Params

```json
{
  "event_name": "transfer",
  "value_path": "amount",
  "comparison": "gt",
  "threshold": "1000000000"
}
```

| Field | Required | Meaning |
| --- | --- | --- |
| `comparison` | yes | One of `gt`, `gte`, `lt`, `lte`, `eq`, `neq`. |
| `threshold` | yes | A JSON number, or a **string** for integers beyond 53 bits (Soroban's `i128`/`u128` amounts routinely are). |
| `value_path` | no | Dot path into the event value: map keys and array indexes, e.g. `amount`, `price.numerator`, `0`. Omit when the value itself is the number. |
| `event_name` | no | Only consider events with this name (first topic). |
| `cooldown` | no | Suppress repeat alerts from this rule for a window, e.g. `"5m"`. See [Rule cooldown](cooldown.md). |

When the contract exports a SEP-0048 spec, `value_path` first addresses the
spec's **named fields** (both topic-located and data-located parameters, e.g.
`amount`, `from`), so a rule can be written from the contract's documentation
instead of counting topic positions. A path the spec does not name — and an
omitted `value_path` — still falls back to the raw positional value, so rules
written before the spec was available keep working.

## Matching semantics

* Comparison happens in arbitrary precision (`big.Float`) — no float53 truncation on token amounts.
* If the path doesn't resolve, or resolves to something non-numeric, the event simply **doesn't match**. Rules never error on the shape of live chain data; they only error on invalid params (which the API rejects at create time anyway).

{% hint style="info" %}
Token amounts on Soroban are integers scaled by the token's decimals (7 for the native XLM contract). A threshold of "100 XLM" is the string `"1000000000"`.
{% endhint %}

## Examples

Bare integer value (the whole event value is the number):

```json
{"comparison": "gte", "threshold": 10}
```

A field inside a map value, only on `swap` events:

```json
{
  "event_name": "swap",
  "value_path": "amount_out",
  "comparison": "lt",
  "threshold": "5000000"
}
```

Nested path — second element of a vector, then a map key:

```json
{"value_path": "1.price", "comparison": "neq", "threshold": 0}
```
