# token_event

The `token_event` rule matches events with the names and topic layout used by
the SEP-41 Stellar token interface. It gives you event-aware `from` and `to`
filters and raw amount bounds without having to count topic indexes yourself.
Use it to monitor transfers, spot large token movements, follow activity from
an address, detect minting or burning, and watch administrative changes such
as clawbacks or admin updates.

The rule interprets event data using SEP-41 conventions; it does not verify
that a contract declares or implements SEP-41. The monitor scopes which
contract or contracts are watched. See the [token monitoring guide](../guides/monitoring-a-token.md)
for an end-to-end setup.

## Supported SEP-41 events

The event name is topic `0`; the two address topics are `1` and `2`. Topic
indexes are zero-based, as they are in the evaluator. The value column refers
to the decoded event value, not a topic.

| Event | Description | Address topic positions | Amount available? |
|---|---|---|---|
| `transfer` | Move tokens from one address to another. | Sender: `1`; recipient: `2` | Yes, i128 value |
| `mint` | Mint tokens to a recipient. | Admin: `1`; recipient: `2` | Yes, i128 value |
| `burn` | Burn tokens held by an address. | Admin: `1`; holder: `2` | Yes, i128 value |
| `clawback` | Admin removes tokens from a holder. | Admin: `1`; holder: `2` | Yes, i128 value |
| `set_admin` | Change the token administrator. | Old admin: `1`; new admin: `2` | No; value is void |

## Configuration parameters

Pass these fields in the rule's `params` object. Address and amount filters
combine with AND: every supplied filter must match. Omitted filters do not
constrain a match.

| Parameter | Required? | Accepted values | Description |
|---|---|---|---|
| `event` | Yes | `transfer`, `mint`, `burn`, `clawback`, `set_admin`, or `*` | Exact event name to match, or the supported-event wildcard described below. There is no default. |
| `from` | No | String | Exact address value in this event's `from` slot. The topic position and role depend on the event; see the topic reference. Unset means any address. |
| `to` | No | String | Exact address value in this event's `to` slot. Its topic position and role depend on the event. Unset means any address. |
| `min_amount` | No | Decimal integer string | Inclusive lower bound on the event's raw i128 amount. Unset means no lower bound. |
| `max_amount` | No | Decimal integer string | Inclusive upper bound on the event's raw i128 amount. Unset means no upper bound. |
| `cooldown` | No | Non-negative Go duration string, such as `5m` or `90s` | Cross-cutting rule option that suppresses repeat alerts for the duration. Omitted or empty means no cooldown. |

The event name is required and case-sensitive. Amount bounds can be used
together. Each must be a base-10 integer string; the validator does not
require the minimum to be less than or equal to the maximum, so reversed
bounds simply cannot match an amount. Signed decimal integers are accepted
by validation, although token amounts themselves are non-negative.
Empty strings for `from`, `to`, `min_amount`, and `max_amount` are treated as
omitted, just like leaving those optional fields out.

Address values are compared exactly with the decoded topic text. The rule
does not validate an address's Stellar format when you create it, so a typo
is accepted but will not match the intended address. Use the exact `G...`
account or `C...` contract address emitted in the topic.

## Event wildcard

Set `"event": "*"` to match any of the five supported names in the event
table. It does not match arbitrary contract events. With no amount bound, the
wildcard includes `set_admin` as well as the four amount-bearing events.

Amount bounds work for wildcard rules on events that carry an i128 value.
`set_admin` has no amount, so it does not match a wildcard rule when either
amount bound is present. By contrast, selecting `"event": "set_admin"`
with `min_amount` or `max_amount` is rejected during validation because that
event cannot carry an amount.

The `from` and `to` filters still use the selected event's address mapping
under a wildcard. If an event has fewer than three topics, it does not match.
If a topic needed by an address filter is absent or is not decoded as an
address, that filter does not match; this is a non-match, not an evaluation
error.

## SEP-41 topic positions

Topic indexes are zero-based: topic `0` is the event symbol, and topics `1`
and `2` hold the event-specific addresses. `from` and `to` are semantic
filters, not fixed aliases for topic `1` and topic `2`:

| Event | Topic `0` | Topic `1` | Topic `2` | `from` filter uses | `to` filter uses | Amount |
|---|---|---|---|---|---|---|
| `transfer` | `transfer` | Sender | Recipient | Topic `1` (sender) | Topic `2` (recipient) | Event value (i128) |
| `mint` | `mint` | Admin | Recipient | Topic `1` (admin) | Topic `2` (recipient) | Event value (i128) |
| `burn` | `burn` | Admin | Holder | Topic `2` (holder) | Topic `1` (admin) | Event value (i128) |
| `clawback` | `clawback` | Admin | Holder | Topic `2` (holder) | Topic `1` (admin) | Event value (i128) |
| `set_admin` | `set_admin` | Old admin | New admin | Topic `1` (old admin) | Topic `2` (new admin) | None; value is void |

In particular, `from` is not always topic `1`: for `burn` and `clawback`,
the holder is topic `2` and is the `from` address, while the admin at topic
`1` is the `to` address for this rule's mapping. The evaluator uses these
event-specific slots, so choosing `to` does not automatically test the same
address as `from`.

## Amount filtering

`min_amount` and `max_amount` are decimal strings in **raw token units**.
They are not converted from a human-readable token amount. The decoded
amount is an i128; the evaluator reads its decimal representation and
compares it as an arbitrary-precision integer, so a string preserves large
values exactly.

Token decimals determine the scale: for a token with 7 decimals, `1000000000`
raw units represents 100 tokens. The [value_threshold reference](value-threshold.md)
documents this decimals caveat (including the native XLM 7-decimal example),
and the [token monitoring guide](../guides/monitoring-a-token.md) shows the
same conversion in context. Check the decimals for the specific token you
monitor and enter the corresponding raw integer.

Amount filters only apply to events with an amount: `transfer`, `mint`,
`burn`, and `clawback`. `set_admin` has no amount value.

## token_event versus event_emitted

Use `token_event` when you want SEP-41-aware matching: one of the five
supported event names, the event-specific `from`/`to` address slots, and
optional inclusive amount bounds. It is useful for token transfers, mint or
burn activity, and administrative events.

Use [`event_emitted`](event-emitted.md) for generic contract events. It can
match any event by its first topic (`event_name`) and/or require exact values
at chosen topic indexes (`topic_equals`). It does not interpret SEP-41
address roles and does not filter the event value by amount. It is the better
fit for non-token contracts, custom event names, or topics that do not follow
the layouts above. It can also match a token event when a generic topic-value
condition is what you need rather than the semantic address or amount
filters.

## Worked examples

Create a monitor for `<CONTRACT_ID>` first, then replace `MONITOR_ID` in
these requests with that monitor's ID. The contract ID is set on the monitor;
it is not repeated in a rule request. Replace each address placeholder with
the exact address from the event topic.

### Match any transfer

This matches every supported SEP-41 `transfer` event for the monitor's
contract, regardless of addresses or amount:

```sh
curl -s -X POST localhost:8080/api/v1/monitors/MONITOR_ID/rules -d '{
  "type": "token_event",
  "params": {"event": "transfer"}
}'
```

### Match a large transfer

This matches transfers with at least 1,000,000,000 raw token units. Choose the
bound based on the monitored token's decimals; it is not a human-unit value.

```sh
curl -s -X POST localhost:8080/api/v1/monitors/MONITOR_ID/rules -d '{
  "type": "token_event",
  "params": {
    "event": "transfer",
    "min_amount": "1000000000"
  }
}'
```

### Match transfers from one address

For `transfer`, `from` compares the sender at topic `1`:

```sh
curl -s -X POST localhost:8080/api/v1/monitors/MONITOR_ID/rules -d '{
  "type": "token_event",
  "params": {
    "event": "transfer",
    "from": "<SENDER_ADDRESS>"
  }
}'
```

## Caveats and troubleshooting

* Only `transfer`, `mint`, `burn`, `clawback`, and `set_admin` are recognized.
  A typo, different capitalization, or another event name is rejected when
  the rule is created or updated. The wildcard is limited to those names.
* The evaluator requires at least three topics and checks the name and
  address positions described above. Events that lack a required topic do
  not match; an address filter also fails to match if its topic is missing
  or not decoded as an address.
* Amount filtering requires a decoded i128 amount. Events without a usable
  amount do not match when a bound is set. This is expected for `set_admin`.
* Amounts are raw units. If a threshold appears too high or low, check the
  token's decimals before changing it.
* A missing `event`, an unknown event, a non-decimal amount bound, an amount
  bound on explicit `set_admin`, or an invalid `cooldown` is rejected at
  rule creation/update. The API reports invalid rule parameters as a
  validation error (HTTP 400).
* Address strings are not format-validated by this rule. Verify the exact
  address and its event-specific role if the rule never matches.
* Unknown parameter keys are ignored by the parameter decoder. Check the
  spellings in the table, especially `min_amount` and `max_amount`, if a
  filter appears to have no effect.