# Query Plan Regression Gate

SoroBeacon gates the PostgreSQL execution plans of its hot read paths in [`internal/store/query_plan_test.go`](../internal/store/query_plan_test.go) so a schema change, query rewrite, or dropped index cannot silently turn an indexed lookup or partition-pruned range scan into a sequential scan in production.

---

## What Is Gated

The regression suite enforces three layers of protection:

### 1. Migration Index Catalog Gate (`TestMigrationIndexesMatchQueryPlanCatalog`)
Runs unconditionally on every `go test ./internal/store` invocation (no database required). It replays all embedded `internal/store/migrations/*.up.sql` files and verifies that every index in `queryPlanIndexCatalog` remains present on its expected table:

| Migration Index | Table | Plan Token | Protected Query Family |
|---|---|---|---|
| `alerts_workspace_created_idx` | `alerts` | `workspace_id_created_at_id_idx` | Tenant-scoped alert feeds, 24h counters, and keyset pagination |
| `alerts_monitor_created_idx` | `alerts` | `monitor_id_created_at_id_idx` | Monitor-filtered alert feeds, per-monitor stats, and keyset pagination |
| `alerts_rule_created_idx` | `alerts` | `rule_id_created_at_id_idx` | Rule-filtered alert feeds, inhibition checks, and per-rule alert counts |
| `alerts_contract_created_idx` | `alerts` | `created_at_id_idx` | Contract-filtered alert feeds (`payload->>'contract_id'`) |
| `alerts_monitor_rule_created_idx` | `alerts` | `monitor_id_rule_id_created_at_id_idx` | Combined `monitor_id + rule_id` alert feeds |
| `alerts_network_created_idx` | `alerts` | `network_created_at_id_idx` | Multi-network alert feeds |
| `alerts_created_at_idx` | `alerts` | `created_at` | Time-bounded scans and retention sweeps |
| `delivery_attempts_alert_idx` | `delivery_attempts` | `delivery_attempts_alert_idx` | Per-alert delivery attempt history (`ListDeliveryAttempts`) |
| `delivery_attempts_channel_attempted_idx` | `delivery_attempts` | `delivery_attempts_channel_attempted_idx` | Windowed per-channel delivery statistics (`GetChannelDeliveryStats`) |
| `monitors_workspace_idx` | `monitors` | `monitors_workspace_idx` | Workspace monitor listings and `id DESC` cursor pagination |
| `monitors_workspace_name_idx` | `monitors` | `monitors_workspace_name_idx` | Workspace monitor listings sorted by `lower(name), id` |
| `channels_workspace_idx` | `channels` | `channels_workspace_idx` | Workspace channel listings and `id DESC` cursor pagination |
| `audit_log_created_at_idx` | `audit_log` | `audit_log` | Time-bounded audit log queries |
| `audit_log_target_idx` | `audit_log` | `audit_log_target_idx` | Target-scoped audit log queries (`target_type, target_id, created_at DESC`) |

### 2. Plan Evaluator & Partition-Pruning Analyzer (`TestQueryPlanGateAnalyzer`)
Runs unconditionally in unit tests to verify that `evaluateQueryPlan`:
- Accepts `Index Scan`, `Index Only Scan`, and `Bitmap Index Scan` plans that touch only in-range partitions.
- Rejects any `Seq Scan` over a data-bearing relation and prints the query, expected path, reason, and indented `EXPLAIN (FORMAT JSON)` plan.
- Rejects plans where out-of-range monthly partitions (`alerts_YYYY_MM`) appear in the plan tree when a time window should have pruned them.

### 3. Live Postgres `EXPLAIN (FORMAT JSON)` Gate (`TestPostgresQueryPlanRegressions`)
Runs whenever `TEST_DATABASE_URL` points at a PostgreSQL instance (including CI integration jobs). It:
1. Creates three consecutive monthly partitions (`alerts_2025_01`, `alerts_2025_02`, `alerts_2025_03`) plus current-month partitions.
2. Seeds realistic multi-tenant, multi-network, multi-severity, acknowledged/unacknowledged alerts across all partitions, along with thousands of `delivery_attempts` and `audit_log` rows.
3. Runs `ANALYZE` so the planner uses real relation cardinalities and histograms rather than empty-table defaults.
4. Executes `EXPLAIN (FORMAT JSON)` against the exact queries built by the store for:
   - **Alerts listing filters:** `workspace_id`, `monitor_id`, `rule_id`, `contract_id`, `network`, `severity`, `acknowledged=true`, `acknowledged=false`, and combined multi-predicate filters.
   - **Partition pruning:** verifies that a query bounded within `2025-02` scans only `alerts_2025_02` and prunes `alerts_2025_01` and `alerts_2025_03`, and a two-month query (`2025-02..2025-03`) prunes `alerts_2025_01`.
   - **Alert statistics:** `GetStats` 24-hour counter, `GetMonitorStats` aggregate subqueries, and `GetMonitorStats` per-rule counts.
   - **Delivery-attempt history & channel stats:** `ListDeliveryAttempts` (unfiltered and status-filtered) and `GetChannelDeliveryStats`.
   - **Cursor-paged reads:** `ListAlerts` (`AfterID` descending and ascending), `ListMonitorsPage` (`id DESC` and `lower(name) ASC`), `ListChannelsPage` (`id DESC`), and `ListAuditEntries` (`id DESC` and target-filtered).

---

## Running Locally

Run the fast catalog and analyzer unit tests (no database required):

```bash
go test -v -run 'TestMigrationIndexesMatchQueryPlanCatalog|TestQueryPlanGateAnalyzer' ./internal/store
```

Run the full PostgreSQL `EXPLAIN (FORMAT JSON)` regression gate against a local PostgreSQL instance:

```bash
export TEST_DATABASE_URL="postgres://postgres:postgres@localhost:5432/sorobeacon_test?sslmode=disable"
go test -v -run TestPostgresQueryPlanRegressions ./internal/store
```

---

## Updating Expectations for a Deliberate Plan Change

If a schema migration or query change intentionally alters an access path:

1. **Update `queryPlanIndexCatalog`** in `internal/store/query_plan_test.go` if an index was added, renamed, or replaced, and document why in `ProtectedReason`.
2. **Update the corresponding `QueryPlanExpectation`** in `TestPostgresQueryPlanRegressions` and set its `Reason` field to explain the new index or join strategy and why it is safe at production scale.
3. **Update the table in `docs/query-plans.md`** in the same pull request so reviewers can see the deliberate plan change alongside the migration or query diff.
