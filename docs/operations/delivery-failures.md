# Diagnosing Failed Alert Deliveries

When an alert fires but never arrives at its destination, follow this triage order to identify the root cause. The system records every delivery attempt with structured data — use that instead of guessing.

## Triage Order

1. **Did the alert fire at all?**
2. **Was a delivery attempted?**
3. **Did the target accept it?**

Do not skip steps. Each step narrows the problem space and uses recorded data, not credentials.

---

## Step 1: Did the Alert Fire?

Check the **monitor's last matched time** and the **alert list**.

### Dashboard
- Open the monitor detail page → `Last matched` timestamp
- Go to **Alerts** → filter by monitor → confirm the alert exists with `Created At` near the expected time

### API
```
GET /api/v1/monitors/{id}
GET /api/v1/alerts?monitor_id={id}&limit=10
```

If no alert exists, the rule didn't match. Check:
- Rule params (event name, thresholds, etc.)
- Contract IDs on the monitor
- Poller logs for evaluation errors

---

## Step 2: Was a Delivery Attempted?

Every alert delivery creates a **delivery attempt record**. This is the single source of truth.

### Dashboard
- Open the alert detail → **Delivery attempts** section
- Shows: channel name, status, response code, timestamp, error snippet

### API
```
GET /api/v1/alerts/{id}/deliveries
```

Response includes:
```json
{
  "id": 123,
  "alert_id": 456,
  "channel_id": 789,
  "channel_name": "ops-slack",
  "status": "failed",
  "response_code": 401,
  "response_snippet": "invalid_auth",
  "created_at": "2026-09-30T12:34:56Z"
}
```

**Key fields:**
| Field | Meaning |
|-------|---------|
| `status` | `sent`, `failed`, `pending`, `suppressed` |
| `response_code` | HTTP status from target (2xx = accepted, 4xx/5xx = rejected) |
| `response_snippet` | First ~200 chars of response body (sanitized) |
| `channel_name` | Which channel was used |

If **no delivery attempt exists** for a channel:
- Channel may not be attached to the monitor
- Channel may be disabled
- Monitor may have no enabled channels
- Check `GET /api/v1/monitors/{id}` → `channel_ids`

---

## Step 3: Did the Target Accept It?

Read the `response_code` and `response_snippet` from the delivery attempt.

### Common Failure Modes by Channel Type

| Channel | Typical Failure Codes | Common Causes |
|---------|----------------------|---------------|
| **Slack** | 401, 403, 404 | Expired/invalid webhook URL, revoked app, deleted channel |
| **Discord** | 401, 403, 404 | Invalid webhook, missing permissions, deleted channel |
| **Telegram** | 401, 403, 400 | Revoked bot token, blocked bot, invalid chat_id |
| **Email (SMTP)** | 550, 554, 421 | Relay rejects sender, auth failed, greylisting |
| **Generic Webhook** | 401, 403, 404, 500 | Target service down, auth expired, payload too large |
| **PagerDuty** | 401, 403, 422 | Invalid integration key, malformed payload |
| **Matrix** | 401, 403, 404 | Expired access token, room not found |
| **Opsgenie** | 401, 403 | Invalid API key |
| **Gotify** | 401, 403 | Expired token |
| **All** | 0 (no response) | Network timeout, DNS failure, firewall |

### Interpreting Response Codes

| Code Range | Meaning | Action |
|------------|---------|--------|
| 2xx | **Success** — target accepted delivery | If still not visible, check target service (spam folder, Slack thread, etc.) |
| 401/403 | **Auth failure** — credential invalid/revoked | Rotate the webhook URL / bot token / API key in channel config |
| 404 | **Not found** — endpoint/channel deleted | Recreate the webhook/channel and update config |
| 429 | **Rate limited** — target rejecting volume | Check channel rate limit config; enable digest mode |
| 5xx | **Target error** — service unavailable | Retry later; check target status page |
| 0 / timeout | **Network failure** | Check connectivity, DNS, firewall |

---

## Per-Channel Failure Modes

### Slack
- **401/403**: Webhook URL revoked or app uninstalled → Generate new webhook in Slack app settings
- **404**: Channel deleted → Create new channel or update webhook to different channel
- **Rate limits**: Slack allows ~1 msg/sec per webhook → Use digest mode or increase `NOTIFY_RATE_LIMIT_SLACK_RPS`

### Discord
- **401/403**: Webhook deleted or missing permissions → Recreate webhook in Server Settings → Integrations
- **400**: Payload too large (>2000 chars) → Check message template length

### Telegram
- **401**: Bot token revoked → Get new token from @BotFather
- **403**: Bot blocked by user or not in group → Unblock or add bot to group
- **400**: Invalid chat_id → Verify chat_id in channel config

### Email (SMTP)
- **550/554**: Sender rejected → Check `FROM` address, SPF/DKIM, relay allowlist
- **421**: Greylisting / temporary deferral → Retry usually succeeds; check `ChannelDisableAfterFailures`
- **Auth failure**: Wrong username/password → Update SMTP credentials

### Generic Webhook
- Check target service docs for expected payload format
- Verify `Content-Type` header (configured in channel template)
- Check payload size limits

---

## Channel Health & Auto-Disable

SoroBeacon tracks **consecutive permanent failures** (401, 403, 404) per channel.

### Configuration
```
CHANNEL_DISABLE_AFTER_FAILURES=5  # default: 0 (disabled)
```

When threshold is reached:
- Channel is marked `enabled=false` automatically
- `disabled_at` timestamp recorded
- No further deliveries attempted until re-enabled

### Check Channel Health
```
GET /api/v1/channels
```
Response includes:
```json
{
  "id": 1,
  "name": "ops-slack",
  "enabled": false,
  "consecutive_failures": 7,
  "consecutive_permanent_failures": 7,
  "last_error": "status 401: Unauthorized",
  "last_error_at": "2026-09-30T12:00:00Z",
  "disabled_at": "2026-09-30T12:05:00Z"
}
```

**Never print channel config to debug** — it contains secrets (webhook URLs, bot tokens, SMTP passwords). The attempt record and health fields have everything you need.

---

## Retry a Failed Delivery

After fixing the root cause (e.g., rotated webhook URL), re-send the alert:

```
POST /api/v1/alerts/{alert_id}/deliveries/{channel_id}/retry
```

### When Retry Is Safe
- ✅ Fixed invalid credential (rotated webhook/token)
- ✅ Target service recovered from outage
- ✅ Network issue resolved

### When Retry Is Unsafe
- ❌ Alert was suppressed by cooldown (would create duplicate)
- ❌ Alert was duplicate (dedup already applied)
- ❌ You haven't fixed the root cause (will fail again)

The retry endpoint creates a **new delivery attempt** for the same alert/channel pair. It does not re-evaluate rules or create new alerts.

---

## Dead-Letter Queue and Redrive

When a channel's automatic retries are exhausted, SoroBeacon does not throw the
alert away. It writes a **dead letter**: a durable row that records what could
not be delivered, so an operator can fix the destination and send it again
without reconstructing the alert by hand.

### When a delivery is dead-lettered

Every direct delivery is tried up to **`MaxAttempts`** times (default **3**),
with exponential backoff between attempts (`BaseBackoff`, default 1s, doubling:
1s, then 2s). If the last attempt still fails, the dispatcher writes one dead
letter for that alert/channel pair and stops.

The dead letter is written **after the final attempt**, so a row means the
automatic budget is gone; there is no background process that retries it later.

These outcomes do **not** produce a dead letter:

* **Invalid channel config.** If the notifier cannot be constructed, the
  dispatcher records one failed attempt and returns immediately. A broken
  config cannot be fixed by retrying, and the channel is counted as a
  permanent failure instead. Edit the channel and use the channel test
  endpoint before relying on it again.
* **Circuit breaker open.** The delivery is skipped before an attempt is made
  (the channel is already known to be failing), so there is no failed attempt
  to dead-letter.
* **Shutdown or breaker trip between attempts.** The loop returns without
  writing a dead letter; the delivery is incomplete rather than permanently
  failed.
* **Permanent HTTP errors still retry.** A 401/403/404 does not shorten the
  `MaxAttempts` loop. It marks the channel's health as a permanent failure
  (which can trigger `CHANNEL_DISABLE_AFTER_FAILURES`), but the remaining
  attempts still run before the dead letter is written.

### What is stored

A dead letter carries `alert_id`, `channel_id`, `last_error`, `attempt_count`,
`last_status` and `created_at`:

* `last_error` is sanitized - webhook URLs and tokens are redacted - so it is
  safe to read in the dashboard and logs.
* `last_status` is the HTTP status of the final attempt, or `0` for a
  transport error (DNS failure, connection refused, timeout).
* `attempt_count` is the attempt that exhausted the budget, normally 3.

The rows are held in the `dead_letters` table in either store backend. They
are **kept until one of these happens**:

* a redrive succeeds and deletes the row;
* the parent alert is removed (alert retention or monitor deletion), which
  cascades;
* the channel is deleted, which also cascades.

There is no separate dead-letter TTL: a row that is never redriven and whose
alert is never expired stays in the queue.

### Finding the queue

**Dashboard:** the **Dead letters** page (`/dead-letters`) lists the queue in
oldest-first order and gives every row a **Redrive** button. It accepts
`channel_id` and `alert_id` query parameters and shows up to 100 rows.

**API:**

```
GET /api/v1/dead-letters?channel_id={id}&alert_id={id}&limit=50&after_id={id}
```

The response is an array of dead-letter objects:

```json
[
  {
    "id": 17,
    "alert_id": 412,
    "channel_id": 3,
    "last_error": "status 404: Not Found",
    "attempt_count": 3,
    "last_status": 404,
    "created_at": "2026-10-20T07:15:02Z"
  }
]
```

`limit` defaults to 50; `after_id` is the keyset cursor (rows are ordered by
`id`). Reading the queue needs the `alerts:read` scope, and redriving needs
`alerts:write`.

### Redrive

```
POST /api/v1/dead-letters/{id}/redrive
```

A redrive sends the **stored alert** to the **stored channel** once. It does
not re-evaluate rules, does not create a new alert, and does not use the
automatic retry loop - it is one delivery attempt. On success the endpoint
returns `204`, records a successful delivery attempt, and deletes the dead
letter. On failure it returns `502`, records the failed attempt, and leaves
the dead letter in place so it can be tried again.

The dashboard performs the same operation and redirects back to
`/dead-letters` on success. The dashboard path also decrements the
dead-letter gauge; the API path does not update it today (see the known
differences below).

Before redriving, fix the destination. A redrive against the same broken
destination just fails again and keeps the row.

### Worked recovery: a webhook URL was wrong for an hour

A channel's webhook URL pointed at a typo'd host for an hour. The poller kept
firing alerts, every delivery attempt failed, and the queue filled up. Here is
the recovery, in order:

1. **Confirm the failure mode.** Read the channel's health and recent
   attempts; the last error on a DNS failure is a transport error with
   `last_status: 0`, while a wrong path is a 404:

   ```
   GET /api/v1/channels/{id}
   GET /api/v1/alerts/{alert_id}/deliveries
   ```

2. **Fix the channel.** `GET /channels` never returns `config`, so send the
   corrected URL with `PATCH`:

   ```sh
   curl -s -X PATCH localhost:8080/api/v1/channels/3 \
     -H "Authorization: Bearer $API_TOKEN" \
     -d '{"config":{"url":"https://hooks.example.com/correct-path"},"enabled":true}'
   ```

   Setting `enabled: true` is also what clears an auto-disable (when
   `CHANNEL_DISABLE_AFTER_FAILURES` is set) and resets its failure counters.

3. **Prove the channel works** before touching the backlog:

   ```sh
   curl -s -X POST localhost:8080/api/v1/channels/3/test \
     -H "Authorization: Bearer $API_TOKEN"
   # {"status":"sent"}
   ```

4. **Redrive the backlog, oldest first.** The queue is in id order, so page
   it and call each row:

   ```sh
   curl -s 'localhost:8080/api/v1/dead-letters?channel_id=3&limit=100' \
     -H "Authorization: Bearer $API_TOKEN"
   curl -s -X POST localhost:8080/api/v1/dead-letters/17/redrive \
     -H "Authorization: Bearer $API_TOKEN"
   ```

   A `204` deletes that row; a `502` leaves it queued. Fix anything the
   failures reveal before retrying the rest, rather than replaying an hour of
   alerts into a destination that is still unhealthy.

5. **Verify.** The queue empties, and `GET /api/v1/alerts/{alert_id}/deliveries`
   shows a new `success` attempt per redriven alert. When you redrive from
   the dashboard, `sorobeacon_dead_letters_total` falls as rows are cleared;
   the API redrive path does not update the gauge, and the gauge is
   process-local, so use the queue endpoint as the source of truth (see the
   [metrics reference](../reference/metrics.md#the-metrics)).

### Manual retry versus redrive

They look similar but operate on different objects:

| | Manual retry | Redrive |
| --- | --- | --- |
| Endpoint | `POST /alerts/{alert_id}/deliveries/{channel_id}/retry` | `POST /dead-letters/{id}/redrive` |
| Identifies | An alert/channel pair | One dead-letter row |
| Requires a delivery attempt first | Yes; no prior attempt is `404` | A dead letter exists; unknown id is `404` |
| Removes a dead letter on success | No | Yes |
| Uses the retry cooldown | Yes (`429` while cooling down) | No |
| Re-runs the automatic retry loop | No, one attempt | No, one attempt |
| Re-evaluates rules or creates an alert | No | No |

Use **retry** when you want to re-send an alert that may not have a dead
letter (for example after a one-off timeout), and **redrive** when you are
working through the queue of permanently failed deliveries.

### Known difference between the API and dashboard redrive paths

The two entry points do not yet behave identically. This is documented as it
works today rather than glossed over:

* **Disabled channel.** Both paths refuse to redrive a disabled channel, so
  re-enable it first. The dashboard reports `502`; the API reports `400`.
* **Already succeeded.** Both paths delete the dead letter first. The
  dashboard then returns `502`, because the dispatcher treats "already
  succeeded" as an error it surfaces after cleanup; the API returns `409`, and
  a repeat request is `404` because the row is gone. Neither path sends the
  alert again.
* **Unbuildable channel config.** The API reports `500`; the dashboard reports
  `502`.
* **Missing channel.** The API returns `404`; the dashboard returns `502`.
  A missing alert or dead letter is `404` on both paths.
* **Dead-letter gauge.** The dashboard path decrements
  `sorobeacon_dead_letters_total` after a successful redrive; the API path
  deletes the row without touching the gauge.

None of these differences changes the safe recovery order above: fix the
channel, verify it with the test endpoint, then redrive.

---

## API Reference

| Endpoint | Purpose |
|----------|---------|
| `GET /api/v1/alerts/{id}/deliveries` | List all delivery attempts for an alert |
| `POST /api/v1/alerts/{id}/deliveries/{channel_id}/retry` | Retry one failed delivery |
| `GET /api/v1/dead-letters` | List dead-lettered deliveries, filtered by `channel_id` / `alert_id` |
| `POST /api/v1/dead-letters/{id}/redrive` | Redrive one dead letter; deletes it on success |
| `GET /api/v1/channels` | List channels with health counters |
| `GET /api/v1/monitors/{id}` | Verify monitor has channels attached |

---

## Dashboard Reference

| Page | What to Check |
|------|---------------|
| **Alert Detail** | Delivery attempts table with status, response code, snippet |
| **Channel List** | Health counters, `disabled_at`, last error |
| **Monitor Detail** | Attached channels, last matched time |
| **Dead Letters** | Permanently failed deliveries with a per-row **Redrive** button |

---

## Prevention

- Set `CHANNEL_DISABLE_AFTER_FAILURES` to auto-disable broken channels
- Use **digest mode** for high-volume channels to avoid rate limits
- Configure **maintenance windows** to silence expected downtime
- Monitor `/health` and `/readyz` endpoints for poller health
- Rotate credentials proactively before they expire

---

## Summary Checklist

- [ ] Alert exists in `/api/v1/alerts?monitor_id={id}`
- [ ] Delivery attempt exists in `/api/v1/alerts/{id}/deliveries`
- [ ] `response_code` indicates the failure type
- [ ] Fix root cause (rotate credential, recreate webhook, etc.)
- [ ] Channel health shows `enabled: true` (or re-enable via PATCH)
- [ ] Retry delivery via `POST /api/v1/alerts/{id}/deliveries/{channel_id}/retry`
- [ ] If retries are exhausted, check `/api/v1/dead-letters` (or the dashboard's **Dead letters** page)
- [ ] Redrive the dead letter after the destination is fixed, then confirm the row disappeared
- [ ] Verify new attempt shows `status: sent` with 2xx response