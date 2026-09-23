# self_transfer

Matches SEP-41 `transfer` events whose **from and to slots hold the same address**.

A transfer from an address to itself is almost always either a contract bug
or deliberate wash activity used to fake volume. Neither is expressible with
[`token_event`](token-event.md) without writing one rule per address pair —
this rule expresses it once.

## Parameters

```json
{
  "min_amount": "1000000"
}
```

| Parameter | Required | Description |
|---|---|---|
| `min_amount` | no | Inclusive lower bound on the i128 amount, as a decimal string. Omit it to match any amount. |

## What matches

Only the SEP-41 `transfer` event is considered. A `mint`/`burn`/`clawback`
is admin/holder bookkeeping rather than a transfer, and events such as
`set_admin` have admin slots, not from/to slots — none of them match, and
they are not errors either.

The address comparison is exact string equality on the decoded address. An
event whose address slots are missing (or empty) does not match.

## Amounts are decimal strings

Amounts are compared as big integers, so pass `min_amount` as a **decimal
string** — a JSON number would lose precision for wide i128 values:

```json
"min_amount": "170141183460469231731687303715884105727"
```

## Validation

`min_amount`, when present, must be a decimal integer.

## Examples

Alert on any self-transfer of a token:

```sh
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "self_transfer",
  "params": {}
}'
```

Ignore trivial round-trips and only catch large ones:

```sh
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "self_transfer",
  "params": {"min_amount": "1000000000"}
}'
```
