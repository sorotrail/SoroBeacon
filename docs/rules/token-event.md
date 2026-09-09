# token_event

Matches **SEP-41 token events** — Stellar's token contract interface — with the interface's topic layout built in.

SEP-41 tokens emit these events, with topics at fixed positions:

| Event | Topics | Value |
|---|---|---|
| `transfer` | `transfer`, from, to | i128 amount |
| `mint` | `mint`, admin, to | i128 amount |
| `burn` | `burn`, admin, holder | i128 amount |
| `clawback` | `clawback`, admin, holder | i128 amount |
| `set_admin` | `set_admin`, old, new | — |

The generic [`event_emitted`](event-emitted.md) rule can express all of this by hand-counting topic indices. `token_event` exists so you don't have to: it knows which slot is the sender and which the recipient on each event, so `from` means the same thing everywhere.

## Parameters

```json
{
  "event": "transfer",
  "from": "GDW6...SENDER",
  "to": "GBXG...RECIPIENT",
  "min_amount": "1000000000",
  "max_amount": "100000000000"
}
```

| Parameter | Required | Description |
|---|---|---|
| `event` | yes | `transfer`, `mint`, `burn`, `clawback`, `set_admin`, or `*` (any of them) |
| `from` | no | Exact address in the outgoing slot — the sender on `transfer`, the holder on `burn`/`clawback` |
| `to` | no | Exact address in the incoming slot |
| `min_amount` | no | Inclusive lower bound on the i128 amount |
| `max_amount` | no | Inclusive upper bound on the i128 amount |

All filters combine with AND. Omitted filters don't constrain.

## Amounts are decimal strings

SEP-41 amounts are `i128`, and JSON numbers are float64 — a wide amount
would lose precision in any comparison that touched a float. Amounts are
compared as big integers end to end; pass thresholds as **decimal strings**,
matching the decoded value shape:

```json
"min_amount": "170141183460469231731687303715884105727"
```

## Validation

Rules that can never match are rejected at create time:

- an unknown `event` name
- `min_amount`/`max_amount` that aren't decimal integers
- amount filters on `set_admin`, which carries no value

## Examples

Alert on every large transfer of a token:

```sh
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "token_event",
  "params": {"event": "transfer", "min_amount": "1000000000"}
}'
```

Watch everything a specific account does with the token:

```sh
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "token_event",
  "params": {"event": "*", "from": "GDW6...ACCOUNT"}
}'
```

Catch any supply change — minting and burning both, either direction:

```sh
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "token_event",
  "params": {"event": "mint"}
}'
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "token_event",
  "params": {"event": "burn"}
}'
```
