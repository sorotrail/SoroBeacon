# Per-Workspace Usage Metering

SoroBeacon attributes operational consumption and storage footprint to each tenant workspace so operators can enforce quotas, attribute infrastructure cost, and generate per-period billing or capacity reports without sacrificing hot-path throughput or `/metrics` cardinality safety.

## Metered Categories

Usage is tracked per workspace across five categories:

1. **Alerts Raised (`alerts_raised`)**
   - Incremented whenever a monitor's rule evaluation creates a new alert row (`AlertCreated`) in either live ingestion (`internal/poller`), historical backfill (`internal/poller.Ingestor`), or absence sweeps (`SweepAbsence`).
   - Deduplicated (`AlertDuplicate`) and cooldown-suppressed (`AlertSuppressed`) evaluations do not increment `alerts_raised`.

2. **Rule Evaluations (`rule_evaluations`)**
   - Incremented once per enabled rule evaluated against a decoded contract event for a monitor owned by the workspace.

3. **Notifications Sent (`notifications`)**
   - Broken down by `channel_type` (e.g. `webhook`, `slack`, `discord`, `telegram`, `email`, `pagerduty`, `opsgenie`, `teams`, `sms`, `voice`, `pubsub`) and `outcome`:
     - `ok`: the notification provider accepted the delivery (`2xx` or provider-level success), including digest flushes, manual retries, and dead-letter redrives.
     - `error`: the notification attempt failed (network error, `4xx`/`5xx` response, or invalid channel configuration).
     - `throttled`: the attempt was rate-limited by the per-channel rate limiter or rejected with `429 Too Many Requests` / `Retry-After`.

4. **API Requests (`api_requests`)**
   - Incremented once per authenticated HTTP API request served under `/api/v1`, attributed to the workspace resolved from the caller's credential (or `default` when authentication is disabled).
   - Instance-level health probes (`/health`, `/livez`, `/readyz`) are excluded so orchestrator probes do not inflate tenant request counts.

5. **Stored Rows (`stored_rows`)**
   - Exact live row counts per workspace across tenant-owned tables:
     - `monitors`: active (non-soft-deleted) monitors owned by the workspace.
     - `rules`: rules attached to active monitors owned by the workspace.
     - `channels`: notification channels owned by the workspace.
     - `alerts`: alert rows belonging to the workspace.
     - `delivery_attempts`: delivery attempt records associated with the workspace's alerts.
     - `dead_letters`: dead-letter queue entries associated with the workspace's alerts.

## Shared-Contract Counting Rule

Multiple workspaces may register monitors watching the same Soroban contract ID (`C...`):

- **Upstream RPC fetch happens once per contract** across the shared watch list in the poller cycle.
- **Rule evaluation, alert creation, and notification fan-out are attributed independently to each workspace that owns a matching monitor.**
- Specifically, if workspace `acme` has a monitor with 2 rules watching contract `CSHARED` and workspace `beta` has a monitor with 1 rule watching `CSHARED`, a single event emitted by `CSHARED` attributes `2` rule evaluations to `acme` and `1` rule evaluation to `beta`. Any resulting alerts and channel deliveries are attributed to the workspace that owns the corresponding monitor and channel.

## Hot-Path Buffering & Durable Rollups

To avoid a synchronous database write on every event evaluation or HTTP request, `internal/usage.Meter` accumulates counter deltas in memory and flushes aggregated UTC hourly buckets (`[period_start, period_end)`) into two durable tables:

- `workspace_usage_rollups (workspace_id, period_start, period_end, alerts_raised, rule_evaluations, api_requests, updated_at)`
- `workspace_notification_usage_rollups (workspace_id, period_start, period_end, channel_type, outcome, count, updated_at)`

Flushes use atomic `ON CONFLICT ... DO UPDATE` upserts (`RecordWorkspaceUsageBatch`) so counters aggregate losslessly across flushes and process restarts. If a database flush encounters a transient error, drained deltas are merged back into memory so no usage is lost.

## Prometheus Metrics & Cardinality Bound

Per-workspace metrics are exposed on `/metrics`:

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `sorobeacon_workspace_alerts_raised_total` | Counter | `workspace` | Total alerts raised per workspace (opaque hashed label). |
| `sorobeacon_workspace_rule_evaluations_total` | Counter | `workspace` | Total rule evaluations executed per workspace (opaque hashed label). |
| `sorobeacon_workspace_notifications_total` | Counter | `workspace`, `channel_type`, `outcome` | Total notification attempts per workspace by channel type and outcome (`ok`, `error`, `throttled`). |
| `sorobeacon_workspace_api_requests_total` | Counter | `workspace` | Total authenticated API requests served per workspace (opaque hashed label). |
| `sorobeacon_workspace_stored_rows` | Gauge | `workspace`, `table` | Current stored row count per workspace and table (`monitors`, `rules`, `channels`, `alerts`, `delivery_attempts`, `dead_letters`). |

### Opaque Label Hashing & Overflow Bucket

- **Opaque workspace labels:** Raw workspace identifiers are never emitted on `/metrics`. Each workspace ID is hashed via `metrics.HashWorkspaceLabel(id)` to `ws_<first 12 hex chars of SHA-256(id)>`, preventing tenant name disclosure on shared monitoring endpoints.
- **Bounded cardinality:** Each `Metrics` registry tracks at most `MaxTrackedWorkspaces = 256` distinct workspaces. Any additional workspaces beyond 256 are aggregated under `workspace="overflow"`, bounding time-series cardinality regardless of tenant count.

## Querying Usage via `GET /api/v1/usage`

`GET /api/v1/usage` flushes any buffered in-memory usage and returns durable rollup summaries alongside live `stored_rows` counts.

### Query Parameters

- `workspace` (optional): Workspace ID to inspect.
  - Callers authenticated with a workspace-bound credential (a tenant token from `WORKSPACE_TOKENS`, a database-backed scoped token carrying `stats:read`, or a non-admin role) are automatically scoped to their own workspace; passing `?workspace=<other>` returns `403 Forbidden`.
  - Unrestricted instance operators (`RoleAdmin` in the `default` workspace) may omit `workspace` (or pass `workspace=all` / `workspace=*`) to list all workspaces, or pass `?workspace=<id>` to inspect a single tenant.
- `from` (optional): Inclusive UTC start bound (`RFC3339` timestamp or `YYYY-MM-DD`).
- `to` (optional): Exclusive UTC end bound (`RFC3339` timestamp, or `YYYY-MM-DD` which includes the full UTC calendar day).
