# Prometheus metrics

SoroBeacon exposes its own Prometheus instrumentation on `/metrics`, on the
listener address set by `HTTP_ADDR` (default `:8080`). No configuration is
needed to enable it — the endpoint is always on and serves the standard
Prometheus text exposition format:

```sh
curl -s localhost:8080/metrics
```

A minimal scrape job:

```yaml
scrape_configs:
  - job_name: sorobeacon
    static_configs:
      - targets: ["localhost:8080"]
```

The registry is application-only: every series is prefixed `sorobeacon_`,
and there are no Go runtime or process metrics. Note that `API_TOKEN` does
**not** cover `/metrics` — authentication wraps the JSON API and the
dashboard, not this path — so restrict the port at the network level if the
host is shared.

## The metrics

| Metric | Type | Labels | Healthy value looks like |
| --- | --- | --- | --- |
| `sorobeacon_polls_total` | counter | `outcome` (`ok`\|`error`) | `outcome="ok"` grows once per `POLL_INTERVAL`; `outcome="error"` stays flat |
| `sorobeacon_poll_duration_seconds` | histogram | — | p95 well under `POLL_INTERVAL` |
| `sorobeacon_poll_lag_ledgers` | gauge | — | small and steady; growing means falling behind |
| `sorobeacon_seconds_since_last_poll` | gauge | — | between `0` and `POLL_INTERVAL` (≈5 by default) |
| `sorobeacon_events_scanned_total` | counter | — | grows when watched contracts emit; `0` is fine on a quiet chain |
| `sorobeacon_events_matched_total` | counter | — | grows when rules match, never above scanned |
| `sorobeacon_rule_evaluations_total` | counter | — | one per event x enabled rule; rises with rules per monitor, not just traffic |
| `sorobeacon_alerts_fired_total` | counter | — | grows when alerts are created; can lag matched (dedup and cooldown suppress) |
| `sorobeacon_alert_deliveries_total` | counter | `channel`, `outcome` (`ok`\|`error`) | `outcome="error"` stays a small share per channel type |
| `sorobeacon_dead_letters_total` | gauge | - | `0`; a positive value means failed deliveries are waiting for a redrive |
| `sorobeacon_heartbeat_last_success_timestamp_seconds` | gauge | — | a recent unix timestamp while `HEARTBEAT_URL` is configured; `0` means no ping has succeeded since this process started (the metric only moves when ingest is healthy) |
| `sorobeacon_http_request_duration_seconds` | histogram | `route`, `method`, `status` | request times at millisecond scale |

Details worth knowing:

* `outcome` is exactly `ok` or `error` on both counter vectors, so a
  failure-rate query can filter on it.
* The `channel` label is the notifier type — one of `discord`, `slack`,
  `telegram`, `email`, `webhook`, `matrix`, `pagerduty` — a static set, so
  label cardinality stays bounded no matter how many channels are configured.
* `sorobeacon_alert_deliveries_total` does **not appear in the output at
  all** until the first delivery attempt is recorded (the counter vector
  creates its series lazily). A fresh deployment with no alerts yet shows
  nothing for it; that is normal, not a missing metric.
* `sorobeacon_dead_letters_total` is kept by the delivery path: the
  dispatcher increments it when it writes a dead letter and decrements it
  after a successful dashboard redrive. The API redrive path deletes rows
  without touching the gauge. It is process-local, so a restart starts it at
  zero even when rows remain in the queue, and an alert-retention cascade can
  remove rows without the gauge seeing it. Treat `GET /api/v1/dead-letters`
  as the authoritative queue and this gauge as the signal that the queue is
  non-empty (see [Diagnosing failed alert deliveries](../operations/delivery-failures.md#dead-letter-queue-and-redrive)).
* The `route` label of the HTTP histogram is the matched route pattern (for
  example `/api/v1/monitors/{id}`), not the raw path, for the same
  cardinality reason; `status` is the response code as a string.
* Both histograms use the default buckets — boundaries at 0.005, 0.01,
  0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5 and 10 seconds — and expose
  `_bucket`, `_sum` and `_count` series.

## Example queries

**Poll failure rate** — the share of poll cycles that ended in an error:

```promql
sum(rate(sorobeacon_polls_total{outcome="error"}[5m]))
  / sum(rate(sorobeacon_polls_total[5m]))
```

**Delivery failure rate by channel type** — which notifier kinds are
struggling, ignoring their traffic volume:

```promql
sum by (channel) (rate(sorobeacon_alert_deliveries_total{outcome="error"}[5m]))
  / sum by (channel) (rate(sorobeacon_alert_deliveries_total[5m]))
```

**p95 poll duration** - how close a poll cycle gets to the interval (a p95
approaching `POLL_INTERVAL` means the next poll starts late and lag grows):

```promql
histogram_quantile(0.95,
  sum by (le) (rate(sorobeacon_poll_duration_seconds_bucket[5m])))
```

**Dead letters waiting for a redrive** - any value above zero means a
permanently failed delivery needs an operator:

```promql
sorobeacon_dead_letters_total > 0
```

## Alerting on stalled ingestion

`sorobeacon_seconds_since_last_poll` is the metric to alert on. It is reset
to `0` every time a poll cycle completes and advanced by a timer in
between, so it climbs without bound the moment polling stops — exactly the
signal a stall alert needs:

```yaml
groups:
  - name: sorobeacon
    rules:
      - alert: SoroBeaconIngestionStalled
        expr: sorobeacon_seconds_since_last_poll > 60
        for: 2m
        annotations:
          summary: "SoroBeacon has not completed a poll cycle in over 60s"
```

Pick a threshold a few multiples above `POLL_INTERVAL` (the default 5s
makes 60s generous), and keep `for` long enough to ride out a missed
scrape. The other counters cannot do this job: a stalled poller's
`sorobeacon_poll_lag_ledgers` freezes at its last value — a stale number
that looks healthy — and a flat `sorobeacon_events_scanned_total` is
ambiguous, since a quiet chain looks the same as a dead poller.

Use `sorobeacon_poll_lag_ledgers` for the different problem of *falling
behind while still polling*: a steadily growing value means every cycle
ends further from the chain tip. The same condition can also fail
readiness server-side via `READYZ_LAG_THRESHOLD` (see the configuration
reference).
