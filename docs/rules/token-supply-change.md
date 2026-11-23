# token_supply_change

Matches **SEP-41 mint and burn events** — the rule most people want for "tell me when supply changes" — and a reader would currently have to find it in the dashboard dropdown and guess.

## What it matches

`token_supply_change` matches SEP-41 `mint` and `burn` events (direction: `mint`, `burn` or `any`), optionally above an optional `min_amount` threshold. This is the rule most people want when they want to be notified of any supply change.

Events that do **not** count as supply changes:

- `clawback` — moves tokens out of a holder's balance back to the issuer; it does not create or destroy supply
- `transfer` — moves between accounts without changing total supply
- `set_admin` — changes the admin without changing supply

| Direction | Description |
|---|---|
| `mint` | Only mint events |
| `burn` | Only burn events |
| `any` | Both mint and burn (default) |

## Parameters

```json
{
  "direction": "any",         // optional: mint|burn|any (default any)
  "min_amount": "1000000"     // optional: i128 amount >= this (decimal string)
}
```

| Parameter | Required | Description |
|---|---|---|
| `direction` | no | Filter events by direction: `mint`, `burn`, or `any` (default `any`) |
| `min_amount` | no | Inclusive lower bound on the i128 amount as a decimal string. Events with amount below this are ignored. |

All filters combine with AND. Omitted filters don't constrain.

## Amounts are decimal strings

SEP-41 amounts are `i128`, and JSON numbers are float64 — a wide amount would lose precision in any comparison that touched a float. Amounts are compared as big integers end to end; pass thresholds as **decimal strings**, matching the decoded value shape:

```json
"min_amount": "170141183460469231731687303715884105727"
```

That is `i128`'s maximum. As a JSON number it would arrive as `1.7014118346046923e+38` and compare against the wrong integer; as a string it is exact.

## Examples

Alert on every mint that changes supply:

```sh
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "token_supply_change",
  "params": {"direction": "mint"}
}'
```

Alert on every burn above 1 million units:

```sh
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "token_supply_change",
  "params": {"direction": "burn", "min_amount": "1000000"}
}'
```

Alert on any supply change above 1000 units (default direction):

```sh
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "token_supply_change",
  "params": {"min_amount": "1000"}
}'
```

## How it relates to token_event with event: mint

`token_event` can match individual `mint` or `burn` events, but cannot express "any supply change above N" in one rule — you would need two separate rules (one for mint, one for burn). `token_supply_change` combines both directions into a single rule and makes the `direction` available for the alert payload, so you can distinguish mint vs burn alerts in your notification handler.

Also unlike `token_event`, `token_supply_change` does **not** fire on `clawback` events, which move tokens back to the issuer without changing total supply.## Validation

Rules that can never match are rejected at create time:

- `min_amount` that isn't a decimal integer
- unknown `direction` values

## See also

- [params reference](params.md) — shared parameter conventions
- [token_event](token-event.md) — match individual SEP-41 events by name
- [End-to-end: monitoring a token contract](../guides/monitoring-a-token.md)
