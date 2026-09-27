# JSON template channel

POSTs a **custom JSON body** rendered from a Go `text/template` to any HTTP
endpoint. Use this when the receiving API expects a specific JSON shape that
differs from the [generic webhook](webhook.md) payload — Slack, PagerDuty,
GitHub, incident.io, a custom internal service, anything.

No Go code or translation proxy required; the whole integration lives in
configuration.

## Setup

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "incident-api",
  "type": "jsontemplate",
  "config": {
    "url": "https://api.example.com/v1/incidents",
    "method": "POST",
    "headers": {
      "Authorization": "Bearer <token>",
      "Content-Type": "application/json"
    },
    "body_template": "{\"title\": \"{{.MonitorName}}\", \"ref\": \"{{.EventID}}\", \"severity\": \"high\"}"
  }
}'
```

## Config

| Key | Required | Description |
| --- | --- | --- |
| `url` | yes | Endpoint to send the rendered JSON to. Treated as a secret (it often embeds tokens). |
| `method` | no | HTTP method: `POST` (default), `PUT`, or `PATCH`. Case-insensitive. |
| `headers` | no | Map of header name → value. Values are secrets and never logged. |
| `body_template` | yes | Go `text/template` executed against the alert. Must produce valid JSON. |

## Template fields

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
| `.CreatedAt` | time | When the alert was created (`time.Time`; use `.CreatedAt.Unix` for a Unix timestamp, or `.CreatedAt.UTC.Format \"2006-01-02 15:04:05\"` for a formatted string). |

Standard Go `text/template` syntax and built-in functions (`printf`, `index`,
`if`/`else`, `range`, …) are available. Templates are not sandboxed: treat a
template you are given as you would any code you run, and don't paste untrusted
strings into one.

## Escaping

Templates are parsed with `text/template`, **not** `html/template`, so values
are inserted verbatim with no automatic escaping. **You own escaping for your
destination.** Because the channel expects the template to produce valid JSON,
you must ensure string values are properly JSON-escaped. The safest approach is
to let the template produce JSON structure and only interpolate *values* that
are already JSON-safe (numbers, booleans, or pre-escaped strings). If you must
interpolate a raw string into a JSON string context, wrap it with `printf
\"%q\"` to JSON-escape it:

```json
"body_template": "{\"message\": {{printf \"%q\" .MonitorName}}}"
```

## Worked examples

### Example 1: Minimal incident payload for a ticketing system

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "jira-bridge",
  "type": "jsontemplate",
  "config": {
    "url": "https://jira.example.com/rest/api/2/issue/",
    "method": "POST",
    "headers": {
      "Authorization": "Basic <base64-credentials>",
      "Content-Type": "application/json"
    },
    "body_template": "{\"fields\": {\"project\": {\"key\": \"OPS\"}, \"summary\": \"{{.MonitorName}}: {{.RuleType}}\", \"description\": \"Event {{.EventID}} on contract {{.ContractID}} at ledger {{.Ledger}}\", \"issuetype\": {\"name\": \"Task\"}}}"
  }
}'
```

### Example 2: Slack-compatible payload for a custom Slack app

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "slack-custom",
  "type": "jsontemplate",
  "config": {
    "url": "https://slack.com/api/chat.postMessage",
    "method": "POST",
    "headers": {
      "Authorization": "Bearer xoxb-...",
      "Content-Type": "application/json"
    },
    "body_template": "{\"channel\": \"#alerts\", \"text\": \"{{printf \"%q\" .MonitorName}} fired {{.RuleType}} (event {{.EventID}})\", \"blocks\": [{\"type\": \"section\", \"text\": {\"type\": \"mrkdwn\", \"text\": \"*{{.MonitorName}}*\\nRule: {{.RuleType}}\\nContract: {{.ContractID}}\\nLedger: {{.Ledger}}\\nTx: {{.TxHash}}\"}}]}"
  }
}'
```

> **Note:** The Slack example uses `printf "%q"` to JSON-escape the monitor
> name in the plain-text fallback, and manual `\n` escapes inside the markdown
> block because the template produces JSON, not plain text.

## Validation and fallback

* A template with a **syntax error** is rejected when the channel is created or
  updated — the API answers `400` and the error names the parse problem — so a
  typo never reaches delivery.
* A template that **parses but fails at execution** (an unknown field, an index
  out of range) returns an error at send time; the alert is not dropped
  silently, but the delivery is recorded as failed.
* The template is parsed **once at channel construction**, not per-delivery.

## Delivery semantics

Non-2xx responses count as failures and are retried (3 attempts, exponential
backoff); make your receiver idempotent on `event_id` + `rule_id` if possible.

Header values are treated as secrets: they never appear in logs, error
messages, or delivery `response_snippets`. The request URL is also redacted
from errors.