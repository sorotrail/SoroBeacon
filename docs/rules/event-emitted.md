# event\_emitted

Matches when a contract emits an event — by name, by exact topic values, or both. This is the workhorse rule: "tell me when `transfer` happens", "tell me when this specific address is the sender".

## Params

```json
{
  "event_name": "transfer",
  "topic_equals": {
    "1": "GDW6AUTBXTOC7FIKUO5BOO3OGLK4SF7ZPOBLMQHMZDI45J2Z6VXRB5NR"
  }
}
```

| Field | Required | Meaning |
| --- | --- | --- |
| `event_name` | one of the two | Matches the event's **first topic** (the event name, by Soroban convention). |
| `topic_equals` | one of the two | Map of topic **index** → expected value. Index `0` is the event name; user topics start at `1`. All entries must match. |

At least one of the two fields must be set. The contract itself is not part of the params — the monitor already scopes which contracts are watched.

## Matching semantics

* Values are compared **canonically**: a topic decoded as a 128-bit integer equals the JSON number or numeric string you wrote in params; symbols and strings compare as plain text; addresses compare as their `G...`/`C...` strkey.
* A topic index outside the event's topic list simply doesn't match (no error).
* Comparison is exact equality — prefix/regex matching is an open contributor idea.

## Examples

Every event named `mint`:

```json
{"event_name": "mint"}
```

`transfer` events where the recipient (topic 2) is the treasury:

```json
{
  "event_name": "transfer",
  "topic_equals": {"2": "GTREASURY..."}
}
```

Any event whose topic 1 equals the integer 42, regardless of name:

```json
{"topic_equals": {"1": 42}}
```
