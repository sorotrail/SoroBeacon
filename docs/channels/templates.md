# Message templates

Every channel that sends the plain-text alert summary — [Discord](discord.md),
[Slack](slack.md), [Telegram](telegram.md) and [Email](email.md) — accepts an
optional `template` in its config. When set, it replaces the built-in message
with your own. It is strictly additive: leave it out and the message is exactly
what it was before the option existed.

The [generic webhook](webhook.md) sends structured JSON instead of the summary
text, so it has no `template` option.

## Example

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "oncall-slack",
  "type": "slack",
  "config": {
    "webhook_url": "https://hooks.slack.com/services/T000/B000/XXXX",
    "template": "<!here> {{.MonitorName}}: *{{.EventName}}* on contract `{{.ContractID}}`\nhttps://stellar.expert/explorer/public/tx/{{.TxHash}}"
  }
}'
```

A terse one-liner:

```json
{"template": "{{.RuleType}} fired on {{.MonitorName}} (ledger {{.Ledger}})"}
```

## Fields

The template is executed with the alert as its data, so `{{.Field}}` names a
field below. This is the contract your template is written against; these are
the exported `notify.Alert` fields.

| Field | Type | Meaning |
| --- | --- | --- |
| `.ID` | integer | Alert id. |
| `.MonitorID` | integer | Monitor that produced the alert. |
| `.MonitorName` | string | Monitor's display name. |
| `.RuleID` | integer | Rule id. |
| `.RuleType` | string | Rule type, e.g. `value_threshold`. |
| `.EventID` | string | Source event's TOID-based id. |
| `.ContractID` | string | Contract that emitted the event. |
| `.EventName` | string | Event name (first topic); may be empty. |
| `.Ledger` | integer | Ledger the event was in. |
| `.TxHash` | string | Transaction hash. |
| `.Payload` | string | The stored alert payload as raw JSON. |
| `.CreatedAt` | time | When the alert was created (`time.Time`; use `.CreatedAt.UTC.Format "2006-01-02 15:04:05"` for a timestamp). |

Standard Go `text/template` syntax and built-in functions (`printf`, `index`,
`if`/`else`, `range`, …) are available. Templates are not sandboxed: treat a
template you are given as you would any code you run, and don't paste untrusted
strings into one.

## Escaping

Templates are parsed with `text/template`, **not** `html/template`, so values
are inserted verbatim with no automatic escaping. **You own escaping for your
destination.** For example, Slack `mrkdwn` and Discord markdown use different
metacharacters; wrap a value in `printf` or your destination's escape syntax if
it can contain characters that matter there.

## Validation and fallback

* A template with a **syntax error** is rejected when the channel is created or
  updated — the API answers `400` and the error names the parse problem — so a
  typo never reaches delivery.
* A template that **parses but fails at execution** (an unknown field, an index
  out of range) falls back to the default message and logs a warning. An alert
  is never dropped because of a formatting mistake.
* The default subject line of an email is built separately from
  [`subject_prefix`](email.md); `template` only overrides the body.
