# Metrics

`/metrics` serves the pipeline's throughput, latency and failure counters in
Prometheus text format.

## Endpoint

The endpoint is **always on**, on the same listener as the API and dashboard
(`HTTP_ADDR`, default `:8080`) — there is no enable flag. The API and dashboard
are unauthenticated in the MVP, and `/metrics` is treated the same way: run it
on a trusted network, or put it behind a reverse proxy that adds auth or
withholds the path, if it must not be public.

```sh
curl -s localhost:8080/metrics | grep '^sorobeacon_'
```

Minimal scrape config:

```yaml
scrape_configs:
  - job_name: sorobeacon
    static_configs:
      - targets: ["localhost:8080"]
```

## Cardinality

Labels are bounded by construction — metric labels are never a channel ID,
contract ID or event ID, because those grow with the chain and are how a
metrics endpoint takes down the process it is meant to observe:

* `outcome` is a fixed set: `ok` | `error`.
* `channel` is the notifier type: `discord` | `slack` | `telegram` | `email` |
  `webhook`.
* `route`, `method`, `status` on the HTTP histogram use the matched chi route
  pattern (e.g. `/api/v1/monitors/{id}`), never the raw path.

## Metric reference

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `sorobeacon_polls_total` | counter | `outcome` | Poll cycles, by whether they succeeded. |
| `sorobeacon_poll_duration_seconds` | histogram | — | Wall-clock duration of one poll cycle (latency). |
| `sorobeacon_poll_lag_ledgers` | gauge | — | Ingest lag: the node's `latestLedger` minus the checkpoint the poller reached. Alert on this to catch SoroBeacon itself falling behind. |
| `sorobeacon_seconds_since_last_poll` | gauge | — | Seconds since the poller last completed a cycle; climbs without bound when polling has stopped. |
| `sorobeacon_events_scanned_total` | counter | — | Contract events read from the source and evaluated. |
| `sorobeacon_events_matched_total` | counter | — | Events that matched at least one enabled rule. |
| `sorobeacon_rule_evaluations_total` | counter | — | Rule evaluations: each event checked against each of its monitor's enabled rules. |
| `sorobeacon_alerts_fired_total` | counter | — | Alerts actually created (deduplicated and cooldown-suppressed matches are not counted). |
| `sorobeacon_alert_deliveries_total` | counter | `channel`, `outcome` | Delivery attempts, by channel type and outcome — delivery failures are per-channel, never aggregate. |
| `sorobeacon_http_request_duration_seconds` | histogram | `route`, `method`, `status` | HTTP request duration by route pattern (latency). |

Every metric method is nil-safe, so instrumentation is optional in tests and
never load-bearing: with no `Metrics` attached, the pipeline records nothing.
