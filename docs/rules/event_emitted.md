# event_emitted

Matches when a contract emits an event — by name, by exact topic values, or both. This is the workhorse rule: "tell me when `transfer` happens", "tell me when this specific address is the sender".

## Params

| Field | JSON Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `event_name` | string | optional* | unset | Matches the event's first topic (the event name, by Soroban convention). |
| `topic_equals` | object | optional* | unset | Map of topic **index** (string key, e.g. `"1"`) → expected value. Index `0` is the event name; user topics start at `1`. All entries must match. |
| `cooldown` | string | optional | unset | Go duration string (e.g., `"5m"`, `"90s"`) to suppress repeat alerts from this rule for the given duration. See [Rule cooldown](cooldown.md). |

\* At least one of `event_name` or `topic_equals` must be provided. Setting neither causes validation to fail.

The contract ID itself is not specified in the rule parameters; the monitor containing the rule already scopes which contract(s) are watched.

## Validation (`Validate`)

The rule's `Validate` method rejects rule creation or update requests with HTTP `400 Bad Request` in the following cases:

1. **Missing both matching criteria:** Neither `event_name` nor `topic_equals` is provided (`p.EventName == ""` and `len(p.TopicEquals) == 0`).
   - Field error: `event_name` → `"event_emitted: set event_name and/or topic_equals"`
2. **Invalid topic index key:** Any key in the `topic_equals` object cannot be parsed as an integer (e.g. `"first"` instead of `"1"`).
   - Field error: `topic_equals.<key>` → `"event_emitted: topic_equals key \"<key>\" is not a topic index"`
3. **Invalid JSON structure or parameters:** Malformed JSON bodies that cannot be unmarshaled into the rule parameter schema.

## Matching Semantics & Event Shapes

- **Topic Comparison:** Event topics are compared against expected values using **exact equality** after canonical formatting via `stellar.Canon`.
  - Symbol and string topics compare as plain text strings.
  - Address topics compare as their strkey representation (e.g., `G...` or `C...`).
  - Integer topics compare canonically by their decimal representation.
  - Topic matching is **exact** — prefix or wildcard/partial matching is not supported by `event_emitted`.
- **Topic Bounds:** A topic index in `topic_equals` that is out of bounds for an event's topic array (e.g. key `"3"` when an event only has 2 topics) results in a non-match (`false`), never a runtime error.
- **Soroban First-Topic Symbol Convention & Event Shapes:**
  By Soroban convention, the first topic (index `0`) represents the event name as a symbol. SoroBeacon handles both decoded event shapes transparently when accessing `ev.EventName()`:
  - **Decoded String Shape:** The first topic is decoded directly as a string (e.g., `"transfer"`).
  - **JSON Wrapper Shape:** The first topic arrives wrapped as a JSON object (e.g., `{"symbol": "transfer"}`).

## Rule-Creation `curl` Examples

### 1. Match by Event Name (`transfer`)

Create a rule that fires whenever a contract emits a `transfer` event:

```sh
curl -s -X POST http://localhost:8080/api/v1/monitors/1/rules \
  -H "Content-Type: application/json" \
  -d '{
    "type": "event_emitted",
    "params": {
      "event_name": "transfer"
    }
  }'
```

### 2. Match by Event Name and Specific Topic Value

Create a rule that fires when a `transfer` event is emitted with a specific sender address in topic position 1:

```sh
curl -s -X POST http://localhost:8080/api/v1/monitors/1/rules \
  -H "Content-Type: application/json" \
  -d '{
    "type": "event_emitted",
    "params": {
      "event_name": "transfer",
      "topic_equals": {
        "1": "GDW6AUTBXTOC7FIKUO5BOO3OGLK4SF7ZPOBLMQHMZDI45J2Z6VXRB5NR"
      },
      "cooldown": "5m"
    }
  }'
```
