# Alert enrichment

An alert tells you *that* something happened. Enrichment lets your own HTTP
service answer *what it means* — attach a token's name and price, a counterparty
label, a runbook link — by fetching that context the moment an alert is created
and carrying it through to delivery.

Enrichment is optional and off by default. When it is off, or the service is
slow or broken, alerts flow exactly as they did without it: enrichment is never
on the critical path.

## How it works

At alert creation (`internal/poller/ingest.go`), before the alert is stored and
delivered, SoroBeacon calls the configured endpoint:

```
GET <ALERT_ENRICHMENT_URL>?contract_id=<C...>&event_name=<name>
```

- The two query parameters are appended to your URL; any query string already
  on it is preserved, so you can put an API token in the URL and it rides along.
  No headers are added.
- A 2xx response whose body is a JSON **object** (not an array or a scalar)
  no larger than 64 KiB is accepted. The keys are yours — SoroBeacon stores the
  object as-is and never interprets its fields.
- The response is stored on the alert as `enrichment`, a separate JSON object.
  It does not merge into the alert's own fields and cannot override them.

From there the enrichment travels with the alert: message templates can print
it, the [generic webhook](../channels/webhook.md) and the JSON template channel
can forward it, the live SSE feed and `GET /api/v1/alerts` include it, and the
dashboard shows it.

## Failure behaviour

Every failure is soft. If the call times out, the service answers non-2xx, the
body is oversized, malformed, or not a JSON object, the alert is delivered
**unenriched** — never held, never dropped. The log gets one warning line
(`alert enrichment failed; continuing without enrichment`) with the reason.

There is no retry: the next alert for the same contract is a fresh call (or a
cache hit, see below).

## Caching

Enrichment responses are cached **in memory**, per process:

- The cache key is the **contract ID**. A different event for the same contract
  within the TTL reuses the cached response — `event_name` is not part of the
  key, so enrichment that varies by event name only refreshes when the TTL
  expires.
- The TTL is `ALERT_ENRICHMENT_CACHE_TTL` (default `5m`). When an enrichment
  value changes, the new value reaches alerts once the cached entry ages out —
  or immediately on a restart, since the cache does not survive one.
- Each SoroBeacon replica holds its own cache; N replicas make N calls per
  contract per TTL.

## Configuration

| Variable | Type | Default | What it does |
| --- | --- | --- | --- |
| `ALERT_ENRICHMENT_URL` | absolute `http(s)` URL | empty (off) | The endpoint called for enrichment. Must be an absolute HTTP(S) URL; anything else fails at startup. |
| `ALERT_ENRICHMENT_TIMEOUT` | Go duration | `2s` | How long one enrichment call may take. Values `<= 0` fall back to the default. |
| `ALERT_ENRICHMENT_CACHE_TTL` | Go duration | `5m` | How long a response is reused per contract. Values `<= 0` fall back to the default. |

An example `.env`:

```sh
ALERT_ENRICHMENT_URL=https://enrich.internal.example.com/enrich
ALERT_ENRICHMENT_TIMEOUT=1s
ALERT_ENRICHMENT_CACHE_TTL=10m
```

## Using enrichment in messages

Channels that send the plain-text summary accept a [`template`](../channels/templates.md);
`{{.Enrichment}}` inserts the enrichment object as raw JSON:

```json
{"template": "⚠ {{.MonitorName}}: {{.EventName}}\nenrichment: {{.Enrichment}}"}
```

`text/template` has no accessor for individual JSON fields — `{{.Enrichment}}`
always prints the whole object — so shape the service's response for the
message you want: a single `display` key with a ready-made sentence reads well
in a chat channel.

Consumers that need fields, not text, should take the structured path: the
[generic webhook](../channels/webhook.md) payload carries `enrichment` as a
JSON field, and so do the SSE feed and the alerts API.

Remember that templates are executed with `text/template`, not `html/template`:
values are inserted verbatim and escaping is yours. Enrichment values are
operator-controlled data, but treat them like the alert payload — escape them
for the channel you are sending to.

## A minimal enrichment service

Any language works; this is a complete one in Python that adds a summary and a
runbook link:

```python
import json
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import parse_qs, urlparse

RUNBOOK = "https://runbooks.example.com/token-alerts"

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        query = parse_qs(urlparse(self.path).query)
        contract = query.get("contract_id", ["unknown"])[0]
        body = json.dumps({
            "display": f"{contract} matched — see {RUNBOOK}",
            "runbook": RUNBOOK,
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

HTTPServer(("127.0.0.1", 8787), Handler).serve_forever()
```

Point `ALERT_ENRICHMENT_URL` at it and the next alert carries the enrichment.
The service should answer well inside `ALERT_ENRICHMENT_TIMEOUT`; anything
slower costs the enrichment, not the alert.

## Security notes

- **The URL may carry credentials.** If yours embeds an API token — in the
  userinfo or the query string — `ALERT_ENRICHMENT_URL` is a secret: keep the
  `.env` at mode `0600` and out of version control, like `API_TOKEN`. It is
  marked in the [environment reference](../configuration.md#secrets).
- **What leaves the process is the contract ID and the event name.** The alert
  payload and transaction details are not sent to the enrichment service. The
  service does learn *which* contracts you monitor and *when* they alert —
  enough to infer your monitoring footprint, so point it at infrastructure you
  trust.
- **The response reappears in notifications.** Enrichment is data you chose to
  fetch, but it is rendered into messages verbatim; a compromised enrichment
  service can put arbitrary text in your on-call's chat. Templates cannot
  address channel config, so the blast radius is message text — but treat the
  enrichment endpoint as part of your delivery trust boundary.

## Contract and token context

The enrichment object can carry `label` (a human name for the contract),
`token_symbol` and `token_decimals`. When present, the default message adds a
`Label:` line and an `Amount:` line, and templates can use `.ContractLabel`,
`.TokenSymbol`, `.EnrichmentSource`, `.EnrichmentStale`, `.PrimaryAmount` and
`.AmountDisplay "<payload field>"`.

- **Exact integer first.** The amount line always shows the exact integer from
  the event payload. The decimal rendering is added beside it, built from the
  integer's digits by string arithmetic, never through a float, so a wide i128
  keeps every digit. It is labelled as unverified metadata and names its source.
- **Untrusted text.** Label and symbol are control and bidirectional-format
  characters stripped, length bounded, and escaped for the destination: Slack
  mrkdwn for Slack and Markdown for Discord. Email is plain text and is not
  escaped. A symbol that imitates a well-known token is not detected; the
  `unverified` label and the named source are the defence.
- **Latency budget.** The lookup has a hard deadline (`ALERT_ENRICHMENT_TIMEOUT`,
  default 2s) and runs while the alert is created; past it the alert is created
  unenriched.
- **Cache.** In-process, at most 1024 contracts, expired by `ALERT_ENRICHMENT_CACHE_TTL`.
  If the source fails, an answer up to 24h past its TTL is reused and marked
  `_stale`; older than that the alert is unenriched. The `_source` and `_stale`
  keys are set by SoroBeacon and override anything the source sends. There is
  no cross-replica invalidation channel in the codebase, so each replica picks
  up a changed label when its own entry expires.

### Limitations

- The source is the operator-supplied endpoint only. Field names from contract
  specs are not added by this path; spec-decoded payloads are unchanged.
- Escaping is wired for Slack and Discord default and custom templates. Other
  channels receive the cleaned text unescaped.
