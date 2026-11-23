# value_threshold

Matches when a numeric value in an event's data or fields crosses a threshold — "alert when a transfer exceeds 1M tokens", "alert when the reported price drops below X".

## Params

| Field | JSON Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `comparison` | string | yes | — | Operator for comparison. Must be one of `gt`, `gte`, `lt`, `lte`, `eq`, `neq`. |
| `threshold` | number or string | yes | — | The threshold value. Can be a JSON number, or a **decimal string** for numbers exceeding 53 bits (e.g., Soroban `i128`/`u128` token amounts). |
| `value_path` | string | optional | unset (the value itself) | Dot-separated path into the event value or spec fields (e.g. `amount`, `price.numerator`, `1.price`). Omit when the root event value itself is numeric. |
| `event_name` | string | optional | unset (all events) | Filter to evaluate only events with this name (first topic). |
| `cooldown` | string | optional | unset | Go duration string (e.g., `"5m"`, `"90s"`) to suppress repeat alerts from this rule for the given duration. See [Rule cooldown](cooldown.md). |

## Validation (`Validate`)

The rule's `Validate` method rejects rule creation or update requests with HTTP `400 Bad Request` in the following cases:

1. **Invalid `comparison` operator:** `comparison` is not one of `gt`, `gte`, `lt`, `lte`, `eq`, or `neq`.
   - Field error: `comparison` → `"value_threshold: invalid comparison \"<value>\" (want gt|gte|lt|lte|eq|neq)"`
2. **Missing or non-numeric `threshold`:** `threshold` is missing, `null`, or cannot be parsed into a numeric representation.
   - Field error: `threshold` → `"value_threshold: threshold must be a number (or a numeric string)"`
3. **Invalid JSON structure or parameters:** Malformed JSON bodies that cannot be unmarshaled into the rule parameter schema.

## Threshold Bounds & Comparison Inclusiveness

Comparisons are evaluated using arbitrary precision floating point arithmetic (`big.Float`). The inclusiveness of each comparison operator is as follows:

| Operator | Bound Inclusiveness | Condition |
| --- | --- | --- |
| `gt` | Strict / Exclusive | `value > threshold` |
| `gte` | Inclusive | `value >= threshold` |
| `lt` | Strict / Exclusive | `value < threshold` |
| `lte` | Inclusive | `value <= threshold` |
| `eq` | Strict Equality | `value == threshold` |
| `neq` | Not Equal | `value != threshold` |

## Token Decimals & Raw Integer Amounts (`i128`)

Token amounts in Soroban smart contracts are stored as raw `i128` or `u128` integers scaled by the token's decimal precision.

### Worked Example: 100 USDC at 7 Decimals

For a standard token with 7 decimal places (such as USDC on Stellar/Soroban):

$$\text{Raw Amount} = \text{Token Amount} \times 10^{\text{decimals}}$$

To match transfers of **100 USDC or greater** on a token contract with 7 decimals:

$$\text{Raw Amount} = 100 \times 10^7 = 1,000,000,000 \text{ stroops}$$

Therefore, set `threshold` to `"1000000000"`.

## Decimal Strings vs. JSON Numbers

Why must thresholds for token amounts be passed as **decimal strings** rather than JSON numbers?

Standard JavaScript/JSON numbers use IEEE 754 double-precision floats, which only represent integers precisely up to $2^{53} - 1$ (9,007,199,254,740,991, ~15–17 decimal digits).

Soroban amounts use 128-bit integers (`i128`), which can reach up to 39 decimal digits ($1.7 \times 10^{38}$). JSON numbers for large amounts would lose precision or be rounded when parsed. Using a decimal string (e.g., `"170141183460469231731687303715884105727"`) preserves full arbitrary precision when evaluated via `big.Float`.

## Missing, Non-Numeric, or Unresolvable Values

If `value_path` is specified but does not exist on the event (or if the event's resolved value is non-numeric, e.g. a string or struct that cannot be converted to a number):

- The evaluator silently returns `false` (does not match).
- It **does not return an error** or cause the monitoring poller to fail.

## Rule-Creation `curl` Examples

### 1. Alert on Transfers Greater Than 100 Tokens (7 Decimals)

Create a rule that fires when a `transfer` event emits an `amount` field greater than $1,000,000,000$ (100 units at 7 decimals):

```sh
curl -s -X POST http://localhost:8080/api/v1/monitors/1/rules \
  -H "Content-Type: application/json" \
  -d '{
    "type": "value_threshold",
    "params": {
      "event_name": "transfer",
      "value_path": "amount",
      "comparison": "gt",
      "threshold": "1000000000"
    }
  }'
```

### 2. Alert on Bare Numeric Event Value Less Than or Equal to 500

Create a rule that checks a contract whose root event value is a bare number (no `value_path` required), alerting when the value is less than or equal to 500:

```sh
curl -s -X POST http://localhost:8080/api/v1/monitors/1/rules \
  -H "Content-Type: application/json" \
  -d '{
    "type": "value_threshold",
    "params": {
      "comparison": "lte",
      "threshold": "500",
      "cooldown": "1m"
    }
  }'
```
