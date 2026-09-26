# Choosing between the notification channels

Seven channel types ship with SoroBeacon. Each has a setup page — those are
linked below and not repeated here. This page is the one to read *before*
them: how the channels differ in the ways that matter operationally, and
which to attach to which job.

Everything on this page is described from `internal/notify/` —
`dispatcher.go` for retries and attempt records, `http.go` for the shared
HTTP behaviour, and each channel's own file for its failure mode.

## The comparison

| Type | Setup effort | Credential needed | When the target is down | What lands in the delivery attempt record |
| --- | --- | --- | --- | --- |
| [Discord](discord.md) | trivial | incoming webhook URL (secret) | 15s timeout, then retried; repeated failures are one `failed` row per try | `status %d: <truncated response body>` from Discord, or the redacted network error |
| [Slack](slack.md) | trivial | incoming webhook URL (secret) | same HTTP path as Discord | same shape — Slack's response body in the snippet |
| [Telegram](telegram.md) | easy | bot token (secret) + chat id | same HTTP path; the bot token is in the request URL but never in errors | `telegram: status %d: <body>` or redacted network error |
| [Matrix](matrix.md) | moderate | homeserver URL, access token, room id | same HTTP path; delivery is idempotent (PUT + transaction id derived from the alert id), so a retry reuses the event instead of posting twice | `matrix: status %d: <body>` or redacted network error |
| [Email](email.md) | moderate | SMTP host + credentials, from, to list | `net/smtp` has no context support — the send runs to completion (or the 15s HTTP timeout does not apply); the dispatcher's backoff still applies | `email: <smtp error>` — often connection-level text, not a status code |
| [PagerDuty](pagerduty.md) | moderate | Events API v2 routing key (secret) | same HTTP path; `dedup_key` is `(rule_id, event_id)`, so redeliveries do not open a second incident | `pagerduty: status %d: <body>` or redacted network error |
| [Webhook](webhook.md) | easy, but the receiver is yours | endpoint URL + HMAC secret (both secret) | whatever your endpoint does is what you get: slow endpoint, slow delivery (up to the 15s timeout, per attempt) | the raw status and a 300-byte slice of your endpoint's response body |

`trivial` means one paste-a-URL step; `easy` one bot creation step;
`moderate` means several required config fields or an account on the target
service.

## Which one for which job

**A team chat channel** — Discord, Slack, Telegram, Matrix. The right
default for "the team should see this contract's events". Setup is minutes,
failures are visible in chat tooling you already watch, and repeated alerting
inside a chat channel is normal rather than noisy. Attach the channel for
the monitor, add a `cooldown` on busy rules, and you are done.

**An on-call path** — PagerDuty. The only channel that wakes someone: it
creates (and deduplicates) incidents rather than messages, carries the alert
context as incident `custom_details`, and takes a `severity`. Use it for the
monitors whose alerts justify a page — a monitor watching a treasury
contract, an oracle feed — and let the chat channels carry everything else.
PagerDuty's dedup key means the store-level dedup and the incident are
aligned: one alert, one incident, however many retries.

**A generic webhook into someone else's system** — the `webhook` type. The
escape hatch when the destination is your own code: an internal incident
bot, a ticketing bridge, a data pipeline. It POSTs the full structured alert
as JSON with an HMAC-SHA256 signature your receiver can verify (rotation
supported via a second header). This is also the right choice for anything
not listed above — point it at a small adapter and implement whatever
protocol the target speaks. It asks more of you than the chat channels do:
you own the receiver's uptime, its latency budget and its response bodies,
and every one of those shows up in SoroBeacon's delivery records.

**Email** — for humans who are not on call and not in chat: weekly reviewers,
external stakeholders, compliance archives. It is the slowest and least
structured path (no status codes, no rate-limit feedback) and mail itself is
lossy; do not build an on-call flow on it.

A common shape: Discord/Slack for the team, PagerDuty on the few monitors
that page, and a webhook into the ops data pipeline — all on the same
monitor. See [attaching several channels](#several-channels-on-one-monitor).

## What is shared by all of them

Described from `internal/notify/dispatcher.go` and `internal/notify/http.go`:

* **Request timeout.** Every HTTP-backed channel (Discord, Slack, Telegram,
  Matrix, PagerDuty, webhook) shares one `http.Client` with a **15-second
  timeout**. Email is the exception: `net/smtp` has no context support, so
  the SMTP conversation is only bounded coarsely by the dispatcher's context.
* **Retries.** The dispatcher gives each channel **3 attempts with
  exponential backoff — 1s, then 2s** (`MaxAttempts 3`, `BaseBackoff 1s`,
  doubled per retry). Only a successful send ends the loop early; a failure
  is recorded per attempt and the next channel is never affected. This is
  the *current* behaviour: the dispatcher's fields are exported, so a
  deployment can tune them, but there is no per-channel retry configuration.
  Manual re-sends from the dashboard/API are a single attempt, gated by a
  30-second cooldown so a double-click cannot double-notify.
* **The redaction guarantee.** Channel config — webhook URLs, bot tokens,
  SMTP passwords, routing keys — is never logged, never returned by the API
  and never written to a delivery attempt. Errors from the HTTP layer are
  stripped of the request URL (webhook URLs and bot-token paths are
  secrets), non-2xx snippets are truncated, and response snippets are capped
  at 500 characters. What a failure looks like is *the target's response
  body* or a redacted network error — never your credentials.
* **Retries are per channel, and nothing is fatal.** One misbehaving channel
  cannot block another channel's delivery or the poller; every attempt,
  success or failure, is recorded in `delivery_attempts`.

## Several channels on one monitor

A monitor can carry any mix of channels: `Dispatch` looks up every *enabled*
channel attached to the monitor and delivers to each in turn, independently.
That costs wall-clock time, not correctness — deliveries to one channel
happen sequentially after another within an alert, and a channel that needs
its full retry ladder (3 attempts + backoff ≈ 3+ seconds of waiting) delays
the *next* channel's first attempt for that same alert, though never beyond
the poller's own loop, which does not wait for dispatch.

Practical consequences:

* Attach the fast chat channel and the paging channel to the same monitor
  freely — a down webhook will not prevent the page.
* The alert history in the dashboard shows one attempt set per channel, so
  "the page went out but the Slack webhook 500ed" is visible per channel.
* More channels per alert means more outbound requests per match — this is
  where a busy rule's firehose is felt first. Suppress at the rule with a
  [cooldown](../rules/cooldown.md) before thinning channels.
