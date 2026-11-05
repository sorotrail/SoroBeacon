# Operator Dashboard Suite

SoroBeacon ships a focused suite of five Grafana dashboards provisioned as code under `deploy/grafana/dashboards/` and `deploy/grafana/provisioning/dashboards/sorobeacon.yaml`.

## Design Principles

1. **Symptom to Subsystem Navigation**: The entry point (`overview.json`) answers *"is it working, and if not, where?"* using service-level objectives rather than dumping every raw counter on one screen. Each panel links directly to the subsystem dashboard and operations runbook that diagnoses that symptom.
2. **CI-Enforced Metric Existence Gate**: `internal/metrics/dashboard_test.go` (`TestDashboardsReferenceOnlyExportedMetrics`) parses every dashboard JSON file on `go test ./...` and verifies that every `sorobeacon_*` metric referenced in any panel or template variable exists in `metrics.ExportedMetricNames()`, which itself is verified against the live Prometheus registry (`TestExportedMetricNamesMatchesRegistry`). A renamed metric fails CI immediately rather than silently rendering a blank panel.
3. **Annotated Thresholds**: Every panel description states what "good" looks like so an on-call responder can interpret the graph under pressure without prior context.
4. **Global Default Scope**: Template variables (`$network`, `$channel`, `$channel_type`, `$route`) always include and default to `All` (`$__all`), preventing a single-slice filter from masquerading as a healthy global view.

## Shipped Dashboards

| Dashboard | File | UID | Primary Question Answered |
|-----------|------|-----|---------------------------|
| **Overview (SLO & Health)** | `deploy/grafana/dashboards/overview.json` | `sorobeacon-overview` | Are evaluation freshness, delivery success, DLQ backlog, and API latency within objective? |
| **Poller & Rule Evaluation** | `deploy/grafana/dashboards/evaluation.json` | `sorobeacon-evaluation` | Is the RPC poller keeping up with the chain tip across priority tiers, and which rule types are slow or failing? |
| **Alert Delivery & DLQ** | `deploy/grafana/dashboards/delivery.json` | `sorobeacon-delivery` | Are fired alerts being delivered, throttled, dropped on SSE streams, or accumulating in the dead-letter queue? |
| **Notification Channels & Breakers** | `deploy/grafana/dashboards/channels.json` | `sorobeacon-channels` | Which notification channels have open or half-open circuit breakers or elevated downstream error rates? |
| **Store, Replica Routing & API Latency** | `deploy/grafana/dashboards/store-and-retention.json` | `sorobeacon-store` | Are read-only queries routing to the read replica, are replica fallbacks firing, and which HTTP routes show scan pressure? |

## Provisioning

Mount `deploy/grafana/provisioning/dashboards/sorobeacon.yaml` into `/etc/grafana/provisioning/dashboards/` and `deploy/grafana/dashboards/` into `/etc/grafana/provisioning/dashboards/sorobeacon/`. All dashboards use the default Prometheus datasource and require no external tracing or proprietary plugin datasources.
