# SLOs and Alert Library

SoroBeacon is an alerting daemon: if it degrades silently, its users' own monitoring is blind. This document defines SoroBeacon's four user-facing Service Level Indicators (SLIs), default Service Level Objectives (SLOs), multi-window multi-burn-rate Prometheus rule library (`deploy/prometheus/rules/`), dead-man's switch setup, and per-alert triage runbooks.

---

## Service Level Objectives (SLOs)

| SLI | Definition | Default Target | Error Budget (30d) | Recording Rule Prefix |
| --- | --- | --- | --- | --- |
| **Evaluation Freshness** | Fraction of poll cycles where the latest evaluated ledger is within `K = 10` ledgers of the RPC tip (`sorobeacon_poll_lag_ledgers{mode!="backfill"} <= 10`). Historical catch-up (`mode="backfill"`) is explicitly excluded so deliberate backfills never burn error budget. | **99.0%** within 10 ledgers | 1.0% (7.2h / 30d) | `sorobeacon:sli_evaluation_freshness:*` |
| **Delivery Success** | Fraction of notification delivery attempts that succeed (`sorobeacon_deliveries_total{status="success"}` / total `sorobeacon_deliveries_total`). | **99.5%** | 0.5% (3.6h / 30d) | `sorobeacon:sli_delivery_success:*` |
| **Delivery Latency** | Fraction of poll and dispatch cycles completing within `T = 10s` (`sorobeacon_poll_duration_seconds_bucket{le="10"}` / `sorobeacon_poll_duration_seconds_count`). | **99.0%** within 10s | 1.0% (7.2h / 30d) | `sorobeacon:sli_delivery_latency:*` |
| **HTTP Read Availability** | Fraction of `GET` requests (`/api/v1/*` and `/health`) returning a non-`5xx` status (`sorobeacon_http_request_duration_seconds_count{method="GET",status!~"5.."}`). | **99.9%** | 0.1% (43.2m / 30d) | `sorobeacon:sli_http_read_availability:*` |

### Adjusting Targets for Your Deployment

To tune targets without rewriting alert logic:

1. **Ledger Lag Threshold (`K`)**: Edit `> bool 10` in `deploy/prometheus/rules/sorobeacon_slos.yaml` under `sorobeacon:sli_evaluation_freshness:*` (e.g. `> bool 20` for relaxed archival deployments).
2. **Error Budget Multiplier**: In `deploy/prometheus/rules/sorobeacon_alerts.yaml`, each burn-rate rule multiplies the burn factor (`14.4` for fast page, `6.0` for slow ticket) by `(1 - SLO)`. Updating the `(1 - SLO)` constant adjusts the alert sensitivity proportionally.

---

## Multi-Window Multi-Burn-Rate Alerting

Instead of firing on brief threshold spikes that recover before an operator unlocks their phone, SoroBeacon uses the Google SRE Workbook multi-window, multi-burn-rate pattern:

- **Fast Burn (`severity="page"`, 14.4x burn rate)**: Fires only when **both** the `1h` window and the `5m` window exceed `14.4 × error_budget` (2% of the 30-day budget consumed in 1 hour). When the underlying fault clears, the `5m` window drops below threshold within minutes so the alert resolves promptly.
- **Slow Burn (`severity="ticket"`, 6.0x burn rate)**: Fires when **both** the `6h` window and the `30m` window exceed `6.0 × error_budget` (5% of the 30-day budget consumed in 6 hours). Routes to a ticket queue for next-business-day investigation.

---

## Dead-Man's Switch Setup

An alerting system that crashes or loses network connectivity stops emitting metrics altogether. Three complementary signals protect against silent failure:

1. **`SoroBeaconWatchdog` (`vector(1)`)**: Always firing (`severity="none"`). Route this alert in Alertmanager to an external heartbeat receiver (PagerDuty Dead Man's Snitch, Healthchecks.io, or Cronitor). If the external receiver stops receiving periodic pings, it pages on-call that Prometheus or Alertmanager itself is down.
2. **`SoroBeaconScrapeMissing` (`absent(up{job=~".*sorobeacon.*"} == 1)`)**: Pages on-call if Prometheus cannot scrape any healthy SoroBeacon target for 2 minutes.
3. **`SoroBeaconPollerStalled`**: Pages on-call if SoroBeacon is up and serving `/metrics` but `sorobeacon_last_poll_age_seconds > 120` or zero polls have completed over 5 minutes.

---

## Alert Runbook Reference

### `SoroBeaconEvaluationFreshnessBurnFast` {#sorobeaconevaluationfreshnessburnfast}

- **Severity**: `page`
- **Meaning**: Live poller lag (`mode!="backfill"`) has exceeded 10 ledgers for >14.4% of samples over both the last 1h and 5m.
- **Triage Steps**:
  1. Open the **SoroBeacon — Overview** and **SoroBeacon — Evaluation** dashboards.
  2. Check `sorobeacon_polls_total{outcome="error"}` and `sorobeacon_poll_duration_seconds` p99 to determine whether the Stellar RPC upstream is timing out or rate-limiting.
  3. Check `sorobeacon_poll_lag_ledgers_by_priority` to verify if high-priority contracts are also lagging.
  4. Verify RPC endpoint health and consider failing over to a secondary Soroban RPC provider.

### `SoroBeaconEvaluationFreshnessBurnSlow` {#sorobeaconevaluationfreshnessburnslow}

- **Severity**: `ticket`
- **Meaning**: Sustained ledger lag above 10 ledgers over 6h and 30m windows (>6x budget burn).
- **Triage Steps**:
  1. Inspect **SoroBeacon — Evaluation** for rising `sorobeacon_events_scanned_total` or slow `sorobeacon_rule_eval_duration_seconds` p95 by `rule_type`.
  2. Check if newly added monitors or wide wildcard rules increased per-poll evaluation time beyond the ledger close interval (~5s).

### `SoroBeaconDeliverySuccessBurnFast` {#sorobeacondeliverysuccessburnfast}

- **Severity**: `page`
- **Meaning**: Notification delivery failure rate exceeds 7.2% (14.4x of the 0.5% budget) over both 1h and 5m.
- **Triage Steps**:
  1. Open **SoroBeacon — Delivery** and **SoroBeacon — Channels**.
  2. Group `rate(sorobeacon_deliveries_total{status="failure"}[5m])` by `channel_type` to isolate whether failures are concentrated on one provider (`slack`, `discord`, `webhook`, `email`, `telegram`, `pagerduty`).
  3. Inspect `sorobeacon_channel_breaker_state` and `sorobeacon_dead_letters_current`.

### `SoroBeaconDeliverySuccessBurnSlow` {#sorobeacondeliverysuccessburnslow}

- **Severity**: `ticket`
- **Meaning**: Notification delivery failure rate exceeds 3.0% (6x of the 0.5% budget) over 6h and 30m.
- **Triage Steps**:
  1. Check **SoroBeacon — Channels** for intermittent channel failures or misconfigured tenant webhook URLs.
  2. Review dead-letter entries via `GET /api/v1/dead-letters` and disable or repair broken user channels.

### `SoroBeaconDeliveryLatencyBurnFast` {#sorobeacondeliverylatencyburnfast}

- **Severity**: `page`
- **Meaning**: More than 14.4% of poll-and-dispatch cycles take longer than 10 seconds over both 1h and 5m.
- **Triage Steps**:
  1. Open **SoroBeacon — Evaluation** and **SoroBeacon — Delivery**.
  2. Compare `sorobeacon_poll_duration_seconds` p95 against `sorobeacon_rule_eval_duration_seconds` p95 to see whether time is spent in RPC fetch, rule evaluation, or synchronous webhook timeouts.

### `SoroBeaconDeliveryLatencyBurnSlow` {#sorobeacondeliverylatencyburnslow}

- **Severity**: `ticket`
- **Meaning**: More than 6.0% of poll-and-dispatch cycles exceed 10 seconds over 6h and 30m.
- **Triage Steps**:
  1. Review long-tail latency on **SoroBeacon — Overview** and tune poll concurrency or channel HTTP client timeouts.

### `SoroBeaconHTTPReadAvailabilityBurnFast` {#sorobeaconhttpreadavailabilityburnfast}

- **Severity**: `page`
- **Meaning**: `GET` API requests are returning `5xx` responses at >1.44% (14.4x of the 0.1% error budget) over 1h and 5m.
- **Triage Steps**:
  1. Open **SoroBeacon — Overview** and **SoroBeacon — Store & Retention**.
  2. Check `sorobeacon_http_request_duration_seconds_count{status=~"5.."}` by `route` and correlate with `sorobeacon_store_replica_fallbacks_total` and PostgreSQL connection health.

### `SoroBeaconHTTPReadAvailabilityBurnSlow` {#sorobeaconhttpreadavailabilityburnslow}

- **Severity**: `ticket`
- **Meaning**: `GET` API requests are returning `5xx` responses at >0.6% over 6h and 30m.
- **Triage Steps**:
  1. Inspect slow or failing read routes in **SoroBeacon — Store & Retention** and check database lock contention during retention sweeps.

### `SoroBeaconWatchdog` {#sorobeaconwatchdog}

- **Severity**: `none`
- **Meaning**: Continuous heartbeat signal proving the Prometheus rule evaluator and Alertmanager pipeline are alive.
- **Triage Steps**: No action required when firing; page on-call only if the downstream dead-man receiver reports silence.

### `SoroBeaconScrapeMissing` {#sorobeaconscrapemissing}

- **Severity**: `page`
- **Meaning**: Prometheus sees no `up == 1` target matching `job=~".*sorobeacon.*"` for at least 2 minutes.
- **Triage Steps**:
  1. Check container/pod status (`kubectl get pods` / `docker ps`) and recent OOMKills or crash loops.
  2. Probe `GET /health` and `GET /metrics` directly from the Prometheus network namespace.

### `SoroBeaconPollerStalled` {#sorobeaconpollerstalled}

- **Severity**: `page`
- **Meaning**: Process is up, but `sorobeacon_last_poll_age_seconds > 120` or zero polls completed in 5 minutes.
- **Triage Steps**:
  1. Capture a goroutine dump or check structured logs for a blocked RPC call or database lock.
  2. Check `sorobeacon_panics_total` on **SoroBeacon — Overview**.

### `SoroBeaconPanicDetected` {#sorobeaconpanicdetected}

- **Severity**: `page`
- **Meaning**: `sorobeacon_panics_total` incremented in the last 5 minutes.
- **Triage Steps**:
  1. Search structured logs for `level=ERROR` with panic stack traces and correlate via `request_id` / trace ID.

### `SoroBeaconDeadLetterQueueNonZero` {#sorobeacondeadletterqueuenonzero}

- **Severity**: `ticket`
- **Meaning**: `sorobeacon_dead_letters_current > 0` for at least 5 minutes.
- **Triage Steps**:
  1. Inspect failed deliveries via the dead-letter API, fix the downstream channel credentials/endpoint, and replay or clear the dead letters.

### `SoroBeaconChannelCircuitBreakerOpen` {#sorobeaconchannelcircuitbreakeropen}

- **Severity**: `ticket`
- **Meaning**: `sorobeacon_channel_breaker_state == 2` (open) for at least 5 minutes on one or more channels.
- **Triage Steps**:
  1. Open **SoroBeacon — Channels** to identify the `channel_id` and `channel_type` with an open breaker.
  2. Verify downstream webhook endpoint reachability and rate limits.

### `SoroBeaconChannelNeverAcknowledged` {#sorobeaconchannelneveracknowledged}

- **Severity**: `ticket`
- **Meaning**: `sorobeacon_channel_unacknowledged_flagged == 1` for at least 15 minutes. The channel accepts deliveries (HTTP 200 or provider acceptance), but zero delivered alerts have been acknowledged within the evaluation SLA window.
- **Triage Steps**:
  1. Open **SoroBeacon — Channels** and check the channel flagged as `unacknowledged`.
  2. Verify that the destination (Slack channel, Discord channel, email inbox, or webhook recipient) has active human watchers.
  3. Send a test alert using the **Send test** button on the Channels dashboard to verify reachability.
  4. Review alert routing and escalation policies to confirm the right team is notified.
  5. *Note on limits*: Unacknowledged does not prove unseen; operators may have acted without clicking acknowledge. Never automatically disable the channel; raise with channel owners to confirm visibility.
