# Choosing and combining rule types

The [per-rule reference pages](#the-rule-types) document each rule type in
isolation. This page answers the two questions those pages cannot: **which
rule do I reach for?** and — the one that generates most support traffic —
**what happens when a monitor carries several rules at once?** The short
answer to the second is *not* "they AND together", and the section below
explains what actually happens, straight from the code.

## The decision guide

| You want to know… | Reach for |
| --- | --- |
| "Did event `X` happen (with these topic values)?" | [`event_emitted`](../rules/event-emitted.md) |
| "Did a number in the event's data cross a threshold?" | [`value_threshold`](../rules/value-threshold.md) |
| "Did a SEP-41 token do something (transfer/mint/burn/…, optionally to/from/amount)?" | [`token_event`](../rules/token-event.md) |
| "Is it happening *too often*?" | [`frequency_threshold`](../rules/frequency-threshold.md) |
| "Did any event in a *family* happen (a pattern, not one exact name)?" | [`topic_regex`](../rules/topic-regex.md) |
| "Does the topic at position N equal exactly this value (a pool ID, a market symbol)?" | [`topic_position`](../rules/topic-position.md) |
| "Did any of *these addresses* move anything?" | [`address_watchlist`](../rules/address-watchlist.md) |

More precisely:

* **`event_emitted` is enough** when the answer is a *shape* question: an
  event with this name fired, or a topic equals this address or integer. It
  is fully general — any contract, any event shape — and it is the only tool
  for contracts that are not tokens. It cannot judge *values*: it compares
  topics for equality, never numbers against thresholds.
* **You need `value_threshold`** when the question involves a *quantity in
  the event data*: an amount, a price, a ratio above/below a bound. It reads
  a number out of the decoded value (optionally by a dot path, or by field
  name when the contract exports a spec) and compares it in arbitrary
  precision. Pair it with `event_name` when the contract emits several
  events, so you only judge the ones shaped like what you expect.
* **`token_event` is the right tool for SEP-41 tokens** — not because
  `event_emitted` can't express it, but because `token_event` knows the
  topic layout: `from` means the sender on `transfer` and the *holder* on
  `burn`, without you hand-counting indices per event. It also takes
  `min_amount`/`max_amount` as decimal strings, which covers the full
  `i128` range that a JSON number would corrupt.
* **`frequency_threshold` is for aggregates**, the shape a mint storm, a
  drain attack or oracle flapping actually has: "more than N matching events
  within M minutes". It complements `value_threshold`, which judges a single
  event. It fires once per crossing, stays quiet one full window, and
  rebuilds its window from the alerts table after a restart — see its
  [reference page](../rules/frequency-threshold.md) for the re-arm details.
* **`topic_regex` is for event families** and unknown topic conventions:
  when a DEX emits `swap_exact_in`, `swap_exact_out`, `pool_deposit`, …, one
  `^swap_`-style pattern covers the family instead of one `event_emitted`
  rule per name. Patterns are unanchored RE2 matched against a topic's
  string value; non-string topics never match. See its
  [reference page](../rules/topic-regex.md).
* **`address_watchlist` is for a set of addresses**: one rule replaces what
  would otherwise be one `token_event` rule per address, watching the from
  and/or to slot of any SEP-41 event against a list that can run to hundreds
  of addresses. See its [reference page](../rules/address-watchlist.md).
* **`topic_position` is for custom contracts' positioned topics**: a pool
  ID, market symbol or account at a known index, compared by exact equality
  against the topic's decoded string form — the question neither
  `event_emitted` (first topic only) nor `topic_regex` (fuzzy, unanchored)
  asks directly. See its [reference page](../rules/topic-position.md).

Any rule type can carry a `cooldown` — see [rule cooldown](../rules/cooldown.md).

## How multiple rules on one monitor combine

Read this twice, because it is the opposite of what most people assume:

> **Rules on one monitor OR together — the monitor alerts if *any one* of
> them matches. They do not AND.**

What the code actually does
(`internal/poller/poller.go`, `handleEvent` — every decoded event is checked
against **every enabled rule** of every monitor watching the contract, and
each rule that matches fires its own alert independently):

* There is **no cross-rule state and no combining logic**. Each rule is
  evaluated on its own by its own evaluator; a match from rule 1 does not
  consult, suppress or require rule 2. One event can therefore produce two
  alerts from one monitor, one from each rule.
* Rule types do not restrict each other. A monitor with `event_emitted`
  (name `transfer`) and `value_threshold` (amount ≥ X) alerts on **every**
  `transfer` — from the first rule — *and* on large amounts — from the
  second. That is OR-like behaviour, and it is why "add a second rule to
  narrow the first" does not do what people expect.
* Cooldowns and the dedup guard are per-rule too: each alert carries the
  rule that fired, and the `(rule_id, event_id)` uniqueness is what stops a
  replay from double-firing one rule, not a group of rules.

### How to express AND anyway

The AND people usually want is available **inside a single rule's params**,
not across rules:

* `event_emitted` with both `event_name` and `topic_equals` requires the
  name *and* the topics to match.
* `value_threshold` with `event_name` set judges only that event's value —
  the event-name condition and the threshold combine with AND.
* `token_event` combines `event`, `from`, `to` and amount bounds with AND.
* `frequency_threshold` with `event_name` counts only that event inside its
  window.

The one AND that cannot be expressed inside params is "rule A matched *and*
rule B matched for the same event". If you need that, it is a contributor
issue (a combining rule type), not a params trick.

### Server-side filtering consequence

One subtlety worth knowing (`internal/poller/topics.go`): when *every*
enabled rule watching a contract can name the concrete events it matches,
the poller narrows the RPC `getEvents` request to those names; if *any* rule
cannot (no `event_name`, a `token_event` wildcard, and so on), the contract
is fetched **unfiltered**. This is only a bandwidth optimisation — client
side evaluation is the source of truth — but it explains why one broad rule
makes a contract's ingest less efficient than an equivalent set of narrow
ones.

## Three worked examples

All three were created against a local instance (monitor created via the
API, rule `POST`ed, `201` confirmed) on the commit that introduced this
page. The contract id is the SoroBeacon quickstart example.

### 1. Large transfers of a token, at most one alert per five minutes

One rule, `token_event`. The amount bound and the cooldown live in the same
params:

```sh
curl -s -X POST localhost:8080/api/v1/monitors -d '{
  "name": "Token large transfers",
  "contract_ids": ["CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"]
}'
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "token_event",
  "params": {"event": "transfer", "min_amount": "1000000000", "cooldown": "5m"}
}'
```

`min_amount` is a **decimal string** (`"1000000000"`), not a JSON number —
see [mistakes](#common-mistakes). Verified: `201` with the rule echoed back.

### 2. "Mints of at least X" — one monitor, two rules, OR on purpose

Two rules on one monitor, and they OR:

```sh
curl -s -X POST localhost:8080/api/v1/monitors -d '{
  "name": "Token mint watch",
  "contract_ids": ["CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"]
}'
# Rule A: any mint at all
curl -s -X POST localhost:8080/api/v1/monitors/2/rules -d '{
  "type": "event_emitted",
  "params": {"event_name": "mint"}
}'
# Rule B: mints whose decoded amount reaches the threshold
curl -s -X POST localhost:8080/api/v1/monitors/2/rules -d '{
  "type": "value_threshold",
  "params": {"event_name": "mint", "value_path": "amount", "comparison": "gte", "threshold": "1000000000"}
}'
```

Rule A alone already alerts on every mint, so rule B adds nothing on this
contract — exactly the trap this page exists to explain. If you only want
large mints, delete rule A and keep rule B. If you want "small mints are
interesting, large mints page someone", put the two rules on **different
monitors** wired to **different channels**. Verified: both rules `201` on
monitor 2.

### 3. A non-token contract: named event plus topic equality

`event_emitted` for a contract that is not a SEP-41 token — the name *and*
an address topic must both match (this is the AND that lives inside one
rule):

```sh
curl -s -X POST localhost:8080/api/v1/monitors/2/rules -d '{
  "type": "event_emitted",
  "params": {
    "event_name": "swap",
    "topic_equals": {"2": "GTREASURY..."}
  }
}'
```

Topic index `0` is the event name; user topics start at `1`, so `topic 2`
here is the second user topic. Verified: `201`.

## Common mistakes

* **Wrong topic position.** `topic_equals` is indexed from `0` = the event
  name, so the first user topic is `"1"`. Writing the recipient address
  under `"1"` for a SEP-41 `transfer` (whose slots are `1`=from, `2`=to)
  makes the rule silently never match — a non-matching index is not an
  error, the event just doesn't match
  (`internal/rules/event_emitted.go`). For tokens, prefer `token_event`'s
  `from`/`to` and never count indices at all.
* **Threshold as a number instead of a decimal string.** `"min_amount":
  1000000000` is rejected at create time (`400` — verified against a local
  instance) because amounts are `i128` compared as big integers and a JSON
  number is a float64
  (`internal/rules/token_event.go`'s validation). Write
  `"min_amount": "1000000000"`. The same string-not-number discipline
  applies to `value_threshold` once values exceed 53 bits.
* **Matching on a contract that emits nothing.** A rule scoped to an event
  name the contract never emits is *accepted* at create time (verified:
  `201`) and then simply never matches — there is no schema check against
  the chain at rule-creation time. Confirm the event name against the
  contract's documentation or a manual `getEvents` query before concluding
  "monitoring is broken".

## The rule types

* [`event_emitted`](../rules/event-emitted.md)
* [`value_threshold`](../rules/value-threshold.md)
* [`token_event`](../rules/token-event.md)
* [`frequency_threshold`](../rules/frequency-threshold.md)
* [`topic_regex`](../rules/topic-regex.md)
* [`topic_position`](../rules/topic-position.md)
* [`address_watchlist`](../rules/address-watchlist.md)
* [`cooldown` (cross-cutting)](../rules/cooldown.md)
