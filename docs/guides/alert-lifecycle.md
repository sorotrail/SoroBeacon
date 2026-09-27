# The life of an alert

[Architecture](../reference/architecture.md) names the components; this page
follows one event through them, end to end, and says what can go wrong at each
step and where to look. Every "why didn't it…" question is a question about
one of these steps.

```
Stellar event
  │
  ▼
1 poll & decode        internal/poller (RPCSource / sorotrail) + internal/stellar
  ▼
2 evaluate rules       internal/rules.Registry.Evaluate   ── several may match ──┐
  ▼                                                                              │
3 suppression gates    internal/store.CreateAlert (dedup + cooldown)            │
  ▼                                                                              │
4 alert row written    alerts table, unique on (rule_id, event_id)              │
  ▼                                                                              │
5 channel fan-out      internal/notify.Dispatcher.Dispatch ◀───────────────────┘
  ▼
6 retry & backoff      notify.Dispatcher.deliver → delivery_attempts
  ▼
7 read it back         GET /api/v1/alerts, dashboard, Prometheus
```

## 1. Polled and decoded

Every `POLL_INTERVAL` the poller runs `Poller.Poll` (`internal/poller/poller.go`):
it collects the contract IDs of all **enabled** monitors, and pages its
`EventSource` (`internal/poller/source.go`) from the checkpoint. Cold start
begins at the source's tip; warm start resumes at `last_ledger + 1`. The RPC
source (`internal/poller/rpcsource.go`) calls `getEvents` and decodes each
event with `stellar.DefaultDecoder.DecodeEvent` (`internal/stellar/decoder.go`),
preferring the RPC's `xdrFormat:"json"` output and falling back to base64 XDR.

**What can go wrong**

* **A monitor's contract ID is malformed.** It is skipped and never polled —
  log `skipping invalid contract id` (`contract_id`). Fix the monitor.
* **The source errors.** The poll fails and retries with exponential backoff
  capped at 10× the interval — log `poll failed` with `retry_in`. Check
  `/api/v1/readyz`, `/metrics` (lag, seconds since last poll).
* **An event will not decode.** The RPC source drops it (`continue` in
  `FetchEvents`) — there is no per-event decode log, and it never reaches the
  scanned count. If exactly one event never fires, suspect decode.

## 2. Evaluated against enabled rules

`Poller.handleEvent` runs every **enabled** rule of every monitor watching the
event's contract, via `rules.Registry.Evaluate`
(`internal/rules/rules.go`). A rule whose `Evaluate` returns an error is logged
(`rule evaluation failed`) and skipped — one bad rule does not stall the event.

**When several rules on one monitor match:** each match is evaluated and fired
**independently** — `handleEvent` loops the rule list and calls `fireAlert`
once per matching rule, so one event produces one alert **per matching rule**,
not one alert for the event. All of those alerts are then delivered to the same
set of the monitor's channels. They are *not* deduplicated against each other:
dedup is keyed on `(rule_id, event_id)`, so two different rules matching the
same event are two distinct rows. The same rule can never fire twice for the
same event, though — see step 3.

A stateful evaluator (the [frequency rule](../rules/frequency-threshold.md)) may
override the dedup key via `Registry.AlertEventID` so every crossing in one
episode shares a synthetic `event_id`.

**What can go wrong**

* A rule is disabled, or its `type` is unknown to the registry — it is simply
  not evaluated. Check the rule's `enabled` and `GET /api/v1/monitors/{id}/rules`.
* An evaluator errors (bad params at runtime) — log `rule evaluation failed`.

## 3. Suppression: what can stop an alert being created

Two gates live in `store.CreateAlert` (`internal/store/postgres.go`), both under
the same transaction and rule-row lock, and both reported by the outcome
(`AlertCreated`, `AlertDuplicate`, `AlertSuppressed` in `internal/store/store.go`):

* **Dedup** — `ON CONFLICT (rule_id, event_id) DO NOTHING`. A replayed event
  returns `AlertDuplicate`; nothing is written.
* **Cooldown** — when the rule carries a `cooldown`, a match inside the window
  returns `AlertSuppressed`: the match is counted, not stored (log
  `alert suppressed by cooldown`). See [Rule cooldown](../rules/cooldown.md).

Other things that prevent an alert earlier in the pipeline: a monitor disabled
(contracts not polled), a rule disabled (not evaluated), and the server-side
topic filter the poller derives from rules that can name their events — that
filter only narrows what is fetched; client-side evaluation in step 2 stays the
source of truth.

**What can go wrong:** a real match silently becomes no alert. The outcome is
the answer — a duplicate is expected on replay, a suppression means cooldown,
and anything else (a `create alert` error in the poller log, a database
problem) means the row was never written. Note a store error here is **not**
retried: the poller moves past the ledger, so that alert is lost. Watch the
`create alert` error log and DB health.

## 4. The alert row

On `AlertCreated`, `CreateAlert` inserts one row into `alerts` with the decoded
event as JSON in `payload` (contract, event name, ledger, tx hash, topics,
value, and `fields` when a contract spec was available), folds in
`suppressed_since_last` when applicable, stamps `monitors.last_matched_at` with
the event's ledger close time, and — when `cooldown` is set — records
`last_alert_at` and resets the suppression counter. Migrations are embedded and
applied at startup ([architecture](../reference/architecture.md#data-model)).

**What can go wrong:** nothing here needs operator attention normally. An
insert is idempotent under retry because of the `(rule_id, event_id)` unique
constraint.

## 5. Channel selection and fan-out

`Poller.fireAlert` hands the alert to `notify.Dispatcher.Dispatch`
(`internal/notify/dispatcher.go`). Delivery is **synchronous with the ingest
loop** — `Dispatch` is an ordinary call in the poll cycle, not a queue — and
walks `store.ListChannelsForMonitor`, which returns the monitor's **enabled**
channels only (`AND c.enabled`), calling `deliver` for each, one at a time.A slow or dead destination therefore delays the next poll by that channel's
retry budget — two backoff sleeps (1s then 2s) plus each attempt's own send
time — in exchange for no queue that can silently lose an alert between step 4
and step 6.

**What can go wrong:** the monitor has no channels attached, or the only ones
are disabled — no delivery, and no attempt recorded. Check the monitor's
channel wiring (`GET /api/v1/monitors/{id}` and the dashboard).

## 6. Retry, backoff, and the delivery attempt record

`Dispatcher.deliver` builds a `Notifier` from the channel's config via
`Factory.New` (`internal/notify/notify.go`) and calls `Notifier.Send`. On
success it stops. On failure it retries up to **3 attempts** with exponential
backoff (**1s → 2s → 4s**, `MaxAttempts`/`BaseBackoff`), then gives up. Every
attempt — success or failure — is recorded by `Dispatcher.record` into
`delivery_attempts` with a `status` and a `response_snippet` (capped at 500
chars), and counted in `/metrics` per channel and outcome.

**What can go wrong**

* **Bad channel config** — `Factory.New` fails, one `failed` attempt is
  recorded and there is no retry (retrying bad config is pointless). Log
  `build notifier`.
* **The destination is down or slow** — up to three `failed` attempts with the
  destination's error as the snippet. Log `alert delivery failed` per attempt;
  inspect `GET /api/v1/alerts/{id}/deliveries`.
* **Shutdown mid-retry** — a cancelled context stops the loop early; the
  attempts so far are already recorded.
* Secrets never appear in snippets or logs; if a channel misbehaves, fix it in
  the dashboard/API and use the manual retry below rather than reading config.

## 7. What you see afterwards

* `GET /api/v1/alerts` — filterable, keyset-paged history with the full decoded
  `payload`; `GET /api/v1/alerts.csv` for the filtered set.
* `GET /api/v1/alerts/{id}/deliveries` — every attempt for one alert.
* `POST /api/v1/alerts/{id}/deliveries/{channelID}/retry` — re-send once; gated
  by `notify.GateRetry` (no attempt yet, already succeeded, channel disabled, or
  inside the 30s retry cooldown). Same gate the dashboard's retry button uses.
* `/api/v1/stats`, `/api/v1/stats/alerts-daily`, and `/metrics` — counts and the
  scanned → matched → alerted funnel. See the [HTTP API](../reference/api.md).
* The **dashboard** (`/alerts`) shows the same history, expandable payloads, a
  retry button, and CSV export — see [The dashboard](dashboard.md).
  `monitors.last_matched_at` drives the "silent" marker; the Overview chart uses
  `alerts` over 30 days.

## "Why didn't it…" quick map

| Symptom | Most likely step | First thing to check |
| --- | --- | --- |
| Never fired at all | 1 (ingest) or 2 (rule) | `poll failed` / lag in `/metrics`; rule `enabled`; `rule evaluation failed` |
| Fired once, then went quiet | 3 (cooldown) or 2 (dedup) | Rule `cooldown`; `alert suppressed by cooldown` log |
| Fired twice for one event | 2 (two rules matched) | Two rule rows on the monitor — expected, one alert each |
| Fired but never arrived | 5 (fan-out) or 6 (delivery) | Monitor's enabled channels; `GET /alerts/{id}/deliveries` |
| Arrived, then failed to retry | 6 (backoff) / 7 (retry gate) | `failed` attempts and their snippets; 30s retry cooldown |
| Alert row vanished later | retention | `ALERT_RETENTION` (see [configuration](../configuration.md)) |

For the pieces this page does not repeat in full, see the
[architecture reference](../reference/architecture.md), the
[HTTP API](../reference/api.md), and [Monitors & alerts](monitors-and-alerts.md).
