# address_watchlist

Matches **SEP-41 token events where a watched address is the sender or the
recipient** — a watchlist of addresses collapsed into one rule, instead of one
[`token_event`](token-event.md) rule per address.

The most common real monitoring question after "did this token move" is "did
*these specific addresses* move anything". A watchlist rule answers it once:
any SEP-41 event on the monitor's contracts whose `from` or `to` slot is on
the list fires the rule. A few dozen addresses as `token_event` rules made the
monitor page unreadable; one `address_watchlist` rule stays readable and
evaluates in constant time regardless of list length.

## Parameters

```json
{
  "addresses": ["GDW6...ACCOUNT", "GBXG...EXCHANGE"],
  "match": "either",
  "event": "transfer"
}
```

| Parameter | Required | Description |
|---|---|---|
| `addresses` | yes | Non-empty list of Stellar account (`G...`) or contract (`C...`) addresses. At most 1024 entries (`MaxAddressWatchlistSize` in `internal/rules/address_watchlist.go`). |
| `match` | no | Which slot(s) to watch: `from`, `to`, or `either` — the default. |
| `event` | no | Restrict to one SEP-41 event: `transfer`, `mint`, `burn`, `clawback`, `set_admin`, or `*` (any of them). Omit to watch all SEP-41 events. |
| `cooldown` | no | Suppress repeat alerts from this rule for a window, e.g. `"5m"`. See [Rule cooldown](cooldown.md). |

## Matching semantics

* **The slots are the same ones [`token_event`](token-event.md) uses.** The
  rule reuses SEP-41's topic layout, so `from` is the semantic outgoing
  address — the sender on `transfer`, the *holder* on `burn`/`clawback`, the
  admin on `mint` — and `to` is the incoming one. You do not hand-count
  topic indices, and a watched address matches wherever it sits in an event.
* **Matching is exact and case-sensitive.** Stellar strkeys are
  case-sensitive, so no partial or prefix matching happens: an address is on
  the list or it is not.
* **Non-SEP-41 events never match**, even when their topic shapes look like
  addresses. Without an `event` restriction the rule watches all five SEP-41
  events; with one, only that event.
* **Non-address topics never match.** Topic values arrive in two decoded
  shapes (a bare string, and the `{"symbol": ...}` / `{"address": ...}`
  wrapper from the RPC's `xdrFormat:"json"` path); both are decoded, and
  anything else — integers, bytes, vectors — simply does not match.

## Performance: the set is built once

A watchlist of hundreds of addresses does not scan linearly per event. The
first evaluation (or `Validate` at create time) builds a Go map set from the
address list and memoises it against the raw params, so every later
evaluation costs one map lookup per slot no matter how long the list is. The
cache is capped and safe for concurrent use; an eviction merely rebuilds the
set on the next evaluation.

## Validation

Rules that can never match are rejected at create time:

- an empty `addresses` list
- any entry that is not a plausible Stellar address (a well-formed `G...`
  account or `C...` contract strkey)
- more than 1024 addresses
- an unknown `match` value (only `from`, `to`, `either`)
- an unknown `event` name

No `event` is needed: a bare watchlist over all SEP-41 events is valid.

## Examples

Watch a set of treasury and exchange addresses, whatever the token event:

```sh
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "address_watchlist",
  "params": {
    "addresses": ["GDW6...TREASURY", "GBXG...EXCHANGE"],
    "match": "either"
  }
}'
```

Alert only when a watched address *sends*, ignoring incoming transfers:

```sh
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "address_watchlist",
  "params": {
    "addresses": ["GDW6...TREASURY"],
    "match": "from",
    "event": "transfer"
  }
}'
```

Track mint recipients for an airdrop audit:

```sh
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "address_watchlist",
  "params": {
    "addresses": ["GA...A", "GA...B", "GA...C"],
    "event": "mint",
    "match": "to"
  }
}'
```
