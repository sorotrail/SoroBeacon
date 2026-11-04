# Alert Partition Lifecycle Automation

On PostgreSQL deployments, the `alerts` table is range-partitioned by `created_at` into monthly child tables named `alerts_YYYY_MM`, with an `alerts_default` fallback partition.

## Ahead-of-Time Partition Creation

SoroBeacon maintains a rolling runway of monthly partitions (default `3` consecutive UTC months starting from the current UTC month: the active month plus two upcoming months):

1. **Startup Verification:** During boot, SoroBeacon ensures the 3-month runway exists (`EnsureAlertPartitions`), verifies the runway via `CheckAlertPartitionRunway`, and exports the initial runway gauge.
2. **Continuous Leader-Gated Loop (`RunAlertPartitionLifecycle`):** Even when `ALERT_RETENTION` is unset, the leader instance runs `RunAlertPartitionLifecycle` every `6h` (`DefaultPartitionCheckInterval`) so long-lived processes automatically create future partitions well ahead of month boundaries.
3. **Boundary-Safe UTC Arithmetic:** Month transitions are anchored to `monthStart(t)` (`YYYY-MM-01T00:00:00Z`) before applying month offsets (`addUTCMonths`), preventing day-overflow bugs across 28/29/30/31-day months, leap years, year boundaries, and non-UTC/DST timestamps.

## Retention-Safe Partition Dropping & Audit Trail

When `ALERT_RETENTION` is enabled (and `ARCHIVE_URL` is unset, since object-storage tiering requires row-by-row archiving before deletion):

- **Conservative Multi-Workspace Cutoff (`EffectiveRetentionCutoff`):** Because dropping a monthly partition removes rows for all workspaces in that month, SoroBeacon computes the effective cutoff from the **longest** positive retention policy across the global retention and any per-workspace retention overrides. If any workspace is configured for indefinite retention (`<= 0`), whole-partition drops are disabled.
- **Bounded Lock Timeout & Load Yielding:** Partition DDL executes inside a transaction with `SET LOCAL lock_timeout = '2000ms'` (`DefaultPartitionLockTimeout`) and caps drops at `3` partitions per cycle (`DefaultPartitionMaxDrops`), yielding immediately if active queries or alert ingestion hold conflicting locks.
- **Cascade Cleanup & Audit Log:** Before detaching and dropping `alerts_YYYY_MM`, SoroBeacon deletes referencing rows from `delivery_attempts` and `alert_dedup` and inserts an `audit_log` entry (`actor = "system:partition_lifecycle"`, `action = "delete"`, `target_type = "alert_partition"`, `target_id = YYYYMM`) recording the partition bounds, cutoff, and number of alerts dropped.
- **Dry-Run Mode:** Calling `MaintainAlertPartitions` with `DryRun: true` inspects `pg_catalog`, calculates `RunwayMonths`, lists missing partitions in `ToCreate`, and counts rows in `ToDrop` without executing DDL or writing audit records.

## Metrics & Alerts

| Metric | Type | Description |
|--------|------|-------------|
| `sorobeacon_alert_partition_runway_months` | Gauge | Consecutive monthly `alerts` partitions present starting from the current UTC month (`-1` on unpartitioned backends). |
| `sorobeacon_alert_partition_errors_total{operation}` | Counter | Failed partition lifecycle operations (`inspect`, `create`, `drop`, `runway`). |
| `sorobeacon_alert_partitions_created_total` | Counter | Monthly `alerts` partitions created by lifecycle automation. |
| `sorobeacon_alert_partitions_dropped_total` | Counter | Expired monthly `alerts` partitions detached and dropped by retention. |

Bundled Prometheus alert rules in `deploy/prometheus/rules/sorobeacon_alerts.yaml`:

- **`SoroBeaconAlertPartitionRunwayLow` (`warning`):** Fires when `sorobeacon_alert_partition_runway_months >= 0 and sorobeacon_alert_partition_runway_months < 2` for `15m`.
- **`SoroBeaconAlertPartitionMaintenanceFailed` (`critical`):** Fires when `increase(sorobeacon_alert_partition_errors_total[15m]) > 0`.
