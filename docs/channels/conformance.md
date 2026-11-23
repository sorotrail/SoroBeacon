# Notification Channel Conformance Contract

Every notification channel registered in `notify.DefaultFactory()` is verified
by `TestChannelConformance` in `internal/notify/conformance_test.go`.

The suite drives its subtests dynamically from
`DefaultFactory().WithFederationOrigin("https://beacon.example").Types()`.
Adding a new channel constructor to `DefaultFactory()` without adding a
matching entry in `channelConformanceFixtures` fails `TestChannelConformance`
immediately in CI.

## The 7-Point Conformance Contract

For every registered channel type (`discord`, `slack`, `telegram`, `email`,
`webhook`, `ntfy`, `matrix`, `pagerduty`, `federation`, `twilio`, `signal`,
`webex`, `lark`, `dingtalk`, `mattermost`, `rocketchat`, `zulip`, `pushover`,
`googlechat`, `opsgenie`, `gotify`, `sns`, `jsontemplate`), the suite verifies:

1. **Delivery success (`DeliverySuccess`)**:
   - Builds the channel via `Factory.New(type, config)` against an in-process
     endpoint (`httptest.NewTLSServer` for HTTP/HTTPS and AWS SNS, or
     `newSMTPTestServer` for SMTP).
   - Delivers an `Alert` containing JSON/form/HTML/Markdown special characters
     (`"`, `<`, `>`, `&`, `\`) and verifies `Send(ctx, alert)` returns `nil`
     after a single outbound request with a well-formed payload.
2. **Permanent failure classification (`PermanentFailureClassification`)**:
   - Exercises HTTP `401 Unauthorized`, `403 Forbidden`, `404 Not Found`, and
     `400 Bad Request` (or SMTP `535 5.7.8 Authentication credentials invalid`
     for `email`).
   - Verifies how `IsPermanent(err)` and `Retryable(err)` classify each error
     for channel health tracking (`CHANNEL_DISABLE_AFTER_FAILURES`) and dead-letter
     short-circuiting.
3. **Transient failure and retryability (`TransientFailureAndRetryable`)**:
   - Exercises HTTP `500`, `502`, `503`, and `429` (or SMTP `450` for `email`).
   - Verifies that `Send` returns a non-nil error, `IsPermanent(err)` is `false`
     (transient outages never count toward auto-disabling a channel), and
     `Retryable(err)` is `true`.
4. **Rate-limit and `Retry-After` handling (`RateLimitAndRetryAfter`)**:
   - Drives a full `Dispatcher.Dispatch` where attempt 1 receives HTTP `429`
     with `Retry-After: 1` (or SMTP `421` on connection 1) and attempt 2
     receives `200 OK` (or SMTP `250`).
   - Verifies that the dispatcher records the initial failed attempt, retries,
     and records `success` and healthy channel status on attempt 2.
5. **Timeout enforcement and context cancellation (`TimeoutAndCancellation`)**:
   - Calls `Send` with an already-canceled `context.Context` and against a
     stalled upstream server with a `40ms` context deadline.
   - Verifies that `Send` aborts promptly when the context expires and that
     timeout/cancellation errors are never classified as permanent (`!IsPermanent(err)`).
6. **Zero secret leakage (`SecretNonLeakage`)**:
   - Seeds a recognizable canary credential (`CONFORMANCE-SECRET-<TYPE>-DO-NOT-LEAK-9F8E7D6C`)
     in the channel's secret config field (webhook URL path/query, bearer token,
     bot token, API key, routing key, basic auth password, HMAC secret, SMTP
     password, or AWS secret access key).
   - Triggers both an upstream auth rejection (where the upstream body also
     echoes `token=<secret>`) and a transport-level connection refusal (where
     `net/url.Error` embeds the full request URL).
   - Verifies the secret never appears in `Send()` error strings,
     `TestHealthUpdate(err, now).Error`, `DeliveryAttempt.ResponseSnippet`,
     `DeadLetter.LastError`, `ChannelHealthUpdate.Error`, or `slog` JSON logs.
7. **Template rendering and payload escaping (`TemplateRenderingAndEscaping`)**:
   - For the 14 channels that accept a custom template (`template` or
     `body_template`), verifies that invalid template syntax is rejected at
     `Factory.New` construction time and that custom templates render alert
     fields without breaking JSON or form encoding.
   - For the 9 fixed-schema channels, verifies that special characters in alert
     fields are escaped cleanly in the wire payload.

## Conformance Matrix & Documented Findings

Running the conformance suite across all 23 channels surfaced the following
behavioral differences, which are pinned explicitly on each channel's
`channelConformanceFixture` so any future change is deliberate:

| Channel | Transport | Custom Template | `IsPermanent` on `401`/`403` | `IsPermanent` on `404` | `Retryable` on `400` | Notes / Findings |
| --- | --- | --- | --- | --- | --- | --- |
| `discord` | HTTP (`postJSON`) | `template` | `true` | `true` | `false` | Full `*StatusError` wrapping |
| `slack` | HTTP (`postJSON`) | `template` | `true` | `true` | `false` | Full `*StatusError` wrapping |
| `telegram` | HTTP (`postJSON`) | `template` | `true` | `true` | `false` | Full `*StatusError` wrapping |
| `email` | SMTP (`net/smtp`) | `template` | `true` (SMTP `535`) | N/A | N/A | Uses `*textproto.Error` (`Code >= 500` is permanent) |
| `webhook` | HTTP (`postJSON`) | No (raw `Alert` JSON) | `true` | `true` | `false` | HMAC-SHA256 signed payload |
| `ntfy` | HTTP (`postJSON`) | No (`RenderText`) | `true` | `true` | `false` | Plain-text body with `Title`/`Priority` headers |
| `matrix` | HTTP (`requestJSON`) | No (`RenderText`) | `true` | `true` | `false` | Idempotent `PUT` keyed by alert ID |
| `pagerduty` | HTTP (`postJSON`) | No (Events v2 schema) | `true` | `true` | `false` | Dedup key `(rule_id, event_id)` |
| `federation` | HTTP (`postJSON`) | No (`FederatedAlert`) | `true` | `true` | `false` | Requires `WithFederationOrigin` on `Factory` |
| `signal` | HTTP (`postJSON`) | `template` | `true` | `true` | `false` | Full `*StatusError` wrapping |
| `mattermost` | HTTP (`postJSON`) | `template` | `true` | `true` | `false` | Requires `https://` `webhook_url` |
| `zulip` | HTTP (`postForm`) | `template` | `true` | `true` | `false` | Requires `https://` `site`; uses `postForm` (`*StatusError`) |
| `pushover` | HTTP (`postForm`) | `template` | `true` | `true` | `false` | Uses `postForm` (`*StatusError`) |
| `googlechat` | HTTP (`postJSON`) | No (`cardsV2` schema) | `true` | `true` | `false` | Full `*StatusError` wrapping |
| `opsgenie` | HTTP (`postJSON`) | No (Alerts v2 schema) | `true` | `true` | `false` | Full `*StatusError` wrapping |
| `gotify` | HTTP (`postJSON`) | No (structured JSON) | `true` | `true` | `false` | Token passed in query string; redacted on transport failure |
| `jsontemplate` | HTTP (`requestJSON`) | `body_template` | `true` | `true` | `false` | Full `*StatusError` wrapping |
| `webex` | HTTP (`requestJSON`) | `template` | **`false`** (finding #1) | `true` | `false` | Replaces `401`/`403` `*StatusError` with untyped hint error |
| `rocketchat` | HTTP (`postJSON`) | `template` | **`false`** (finding #2) | **`false`** | **`true`** | Replaces `*StatusError` with untyped `fmt.Errorf("rocketchat: status %d", se.Code)` |
| `twilio` | HTTP (`httpClient.Do`) | `template` | **`false`** (finding #3) | **`false`** | **`true`** | Returns untyped `fmt.Errorf("twilio: status %d: ...")` |
| `dingtalk` | HTTP (`postJSONWithResponse`) | `template` | **`false`** (finding #3) | **`false`** | **`true`** | `requestJSONWithResponse` returns untyped `fmt.Errorf("status %d: ...")`; also uses HTTP 200 `errcode != 0` |
| `lark` | HTTP (`httpClient.Do`) | `template` | **`false`** (finding #3) | **`false`** | **`true`** | Returns untyped `fmt.Errorf("lark: status %d: ...")`; also uses HTTP 200 `code != 0` |
| `sns` | AWS SDK (`sns.Client`) | No (`snsMessage` JSON) | **`false`** (finding #4) | **`false`** | **`true`** | Wraps AWS SDK `*smithy.OperationError` rather than `*StatusError` |

### Detailed Findings

1. **`webex` strips `*StatusError` on HTTP `401` and `403` (`internal/notify/webex.go:64-66`)**:
   - When `requestJSON` returns a `401` or `403`, `Webex.Send` returns
     `fmt.Errorf("webex: check the bot token and that the bot is a member of the room")`
     without wrapping the underlying `*StatusError`. Because `IsPermanent` and
     `Retryable` inspect `errors.As(err, &statusErr)`, a revoked Webex token is
     treated as transient (`IsPermanent == false`) and retried (`Retryable == true`).
2. **`rocketchat` strips `*StatusError` to suppress response body echo (`internal/notify/rocketchat.go:77-80`)**:
   - `RocketChat.Send` extracts `se.Code` and returns `fmt.Errorf("rocketchat: status %d", se.Code)`
     instead of `&StatusError{Code: se.Code}` or `&HTTPStatusError{StatusCode: se.Code}`.
3. **`twilio`, `dingtalk`, and `lark` return untyped HTTP status errors**:
   - `requestJSONWithResponse` (`internal/notify/http.go:194`), `Twilio.Send`
     (`internal/notify/twilio.go:117`), and `Lark.Send` (`internal/notify/lark.go:108`)
     format non-2xx responses with `fmt.Errorf` instead of returning `*StatusError`
     or `*HTTPStatusError`.
4. **`sns` errors come from `aws-sdk-go-v2` (`internal/notify/sns.go:104`)**:
   - `IsPermanent` and `Retryable` in `internal/notify/health.go` do not unwrap
     smithy HTTP response errors from the AWS SDK.
5. **`Retry-After` header duration vs. fixed exponential backoff (`internal/notify/dispatcher.go:620-650`)**:
   - `ParseRetryAfter` is implemented in `internal/notify/ratelimit.go:133`, and
     `Dispatcher.deliver` records throttle metrics when `429` or `Retry-After`
     appears in the error string, but `requestJSON` does not attach the
     `Retry-After` response header to `StatusError` and `Dispatcher.deliver`
     sleeps for `d.BaseBackoff` rather than honoring the server's `Retry-After`
     delay.
