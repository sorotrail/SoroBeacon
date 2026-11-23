# contract\_allowlist

Matches when the **event's contract** is one of a configured list of contract
IDs — or, with `exclude`, when it is **not** one of them.

Every other rule type sees the events of the contracts its monitor watches
and cannot tell them apart: a monitor on a protocol's ten contracts applies
all of its rules to all ten. `contract_allowlist` is the rule that says
"of the contracts I watch, this one". That is why it is at its best inside
a [composite](composite.md) rule, AND-combined with a rule that looks at
the payload — "large transfers, but only on the treasury contract" — instead
of a second monitor with a duplicated set of channels.

## Parameters

```json
{
  "contract_ids": ["CA7Q...TREASURY", "CDLZ...POOL"],
  "exclude": false
}
```

| Parameter | Required | Description |
|---|---|---|
| `contract_ids` | yes | Non-empty list of Stellar **contract** IDs (`C...` strkeys). Each entry is checked with the same validator the monitor create path uses, so a mistyped or truncated ID is rejected at create time. |
| `exclude` | no | Invert the rule into a denylist: the rule matches when the contract is **not** in the list. `false` by default. |
| `cooldown` | no | Suppress repeat alerts from this rule for a window, e.g. `"5m"`. See [Rule cooldown](cooldown.md). |

## Matching semantics

* **Matching is exact and case-sensitive.** Stellar strkeys are
  case-sensitive, so a contract is on the list or it is not; there is no
  prefix or substring matching.
* **The rule looks at the contract, not the event.** It ignores the event's
  topics and its value: the same contract matches or does not match
  regardless of what it emitted.
* **`exclude: true` inverts the match, it does not add a second rule.** With
  `exclude`, the rule fires for every contract the monitor watches *except*
  the listed ones.
* **The list may name contracts the monitor does not watch.** Such entries
  are simply never seen by the rule; the rule never widens a monitor's
  watch list.

## Performance: the set is built once

The list does not get scanned per event. The first evaluation (or `Validate`
at create time) builds a Go map set from the contract IDs and memoises it
against the raw params, so every later evaluation costs one map lookup no
matter how long the list is. The cache is capped and safe for concurrent
use; an eviction merely rebuilds the set on the next evaluation.

## Validation

Rules that can never match are rejected at create time:

- an empty or missing `contract_ids` list —
  `contract_allowlist: contract_ids is required (a non-empty list of contract IDs)`
- an entry that is not a valid contract strkey —
  `contract_allowlist: contract_ids[0] "CDLZ..." is not a valid contract ID`

The failure names the offending index, so a long list is easy to fix.

## Examples

Watch one of two contracts on a monitor that follows a whole protocol:

```sh
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "contract_allowlist",
  "params": {
    "contract_ids": [
      "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"
    ]
  }
}'
```

Everything **except** the treasury contract:

```json
{
  "type": "contract_allowlist",
  "params": {
    "contract_ids": ["CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"],
    "exclude": true
  }
}
```

### Combining it with a payload rule

On its own the rule says nothing about *what* happened, so it rarely stands
alone. A [composite](composite.md) rule AND-combines it with a rule that
looks at the event's payload — here, a transfer of more than 1,000,000
stroops on the treasury contract only:

```json
{
  "type": "composite",
  "params": {
    "op": "and",
    "rules": [
      {
        "type": "contract_allowlist",
        "params": {
          "contract_ids": ["CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"]
        }
      },
      {
        "type": "token_event",
        "params": {
          "event": "transfer",
          "min_amount": "1000000"
        }
      }
    ]
  }
}
```

`and` stops at the first child that does not match, so with the allowlist
listed first the amount check only runs for the events that came from the
treasury contract. Children are validated recursively when the composite is
created, so a mistyped contract ID is a create-time error rather than a
runtime surprise.

A [`value_threshold`](value-threshold.md) child works the same way for a
contract whose value is the number itself, and the same shape works with an
[`event_name_glob`](event-name-glob.md) child, which narrows the composite
to the events of that contract whose names match a pattern.
