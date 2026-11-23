# Per-Workspace Quotas and Query Cost Budgets

SoroBeacon enforces per-workspace resource quotas and unified query cost budgets so a single tenant cannot exhaust database storage, rule evaluation capacity, SSE stream connections, or read-path query budgets in multi-tenant deployments.

## Resource Quotas

Each workspace has configurable upper bounds stored in the `workspace_quotas` table (`0` means unlimited):

| Quota | Field | Resource Name | Enforced On |
| --- | --- | --- | --- |
| Monitors | `max_monitors` | `monitors` | `POST /api/v1/monitors`, `POST /api/v1/monitors/import`, `POST /api/v1/monitors/{id}/duplicate`, `POST /api/v1/monitors/{id}/restore`, `POST /api/v1/templates/{id}/instantiate`, `POST /api/v1/templates/{id}/instantiate/bulk` |
| Rules | `max_rules` | `rules` | `POST /api/v1/monitors/{id}/rules`, `POST /api/v1/monitors/{id}/rules/bulk`, `POST /api/v1/monitors/{id}/duplicate`, `POST /api/v1/templates/{id}/instantiate`, `POST /api/v1/templates/{id}/instantiate/bulk` |
| Channels | `max_channels` | `channels` | `POST /api/v1/channels`, `POST /api/v1/channels/{id}/duplicate` |
| Active SSE Subscriptions | `max_subscriptions` | `subscriptions` | `GET /api/v1/alerts/stream` |

### Atomic Boundary Enforcement

Database-backed quotas (`max_monitors`, `max_rules`, `max_channels`) are checked inside the creation transaction before inserting rows. Concurrent creation requests at the quota boundary cannot overshoot the configured limit. Deleting a monitor, rule, or channel immediately frees capacity for subsequent creations.

Active SSE subscription limits (`max_subscriptions`) are tracked per workspace and released immediately when the client disconnects.

## Unified Query Cost Budgets (REST + GraphQL)

Read-heavy API queries share a per-workspace token-bucket query cost budget (`query_cost_budget` capacity with continuous refill at `query_cost_refill_per_sec` cost units per second):

* **REST routes**:
  * `GET /api/v1/alerts`, `/monitors`, `/channels`, `/dead-letters`, `/audit`: base cost `2 + limit/25` (plus `+2` when full-text `q` search is used, `+1` when `from`/`to` time bounds are used), reconciled after execution against actual response size.
  * `GET /api/v1/alerts.csv` and `GET /api/v1/alerts/export`: estimated cost `20 + limit/25`, reconciled after streaming against actual bytes exported.
  * `GET /api/v1/stats`, `GET /api/v1/stats/alerts-daily`, `GET /api/v1/usage`, and `POST /api/v1/monitors/{id}/rules/dry-run`: cost `5` (plus `+2` with `from`/`to` bounds).
* **GraphQL (`POST /graphql`)**:
  * Static query complexity is computed by `graphql.CalculateQueryComplexity` and deducted from the **same** per-workspace token bucket before executing resolvers.
* **Exempt operational endpoints**:
  * `/health`, `/livez`, `/readyz`, and `/quotas` never deduct from the query cost budget so health probes and quota inspection remain available while a workspace is throttled.

## Inspecting and Updating Quotas (`GET /quotas`, `PUT /quotas`)

* `GET /api/v1/quotas` (`stats:read` scope / `viewer` role): Returns configured limits (`max_monitors`, `max_rules`, `max_channels`, `max_subscriptions`, `query_cost_budget`, `query_cost_refill_per_sec`), live usage (`usage.monitors`, `usage.rules`, `usage.channels`, `usage.subscriptions`), and `query_budget_remaining`.
* `PUT /api/v1/quotas` (`monitors:write` scope / `admin` role): Updates any subset of `max_monitors`, `max_rules`, `max_channels`, `max_subscriptions`, `query_cost_budget`, or `query_cost_refill_per_sec`. Operators bound to the `default` workspace can target any tenant via `?workspace=<id>` or `"workspace_id"` in the request body.

## Deterministic `429 Too Many Requests` Responses

When a resource quota or query cost budget is exceeded, SoroBeacon returns HTTP `429 Too Many Requests` with a `Retry-After` header and a structured problem payload:

```json
{
  "error": "workspace \"acme\" monitors quota exceeded (current=1, requested=1, limit=1)",
  "code": "QUOTA_EXCEEDED",
  "request_id": "req-123",
  "quota": "monitors",
  "resource": "monitors",
  "workspace_id": "acme",
  "limit": 1,
  "current": 1,
  "requested": 1,
  "retry_after_seconds": 1
}
```

## Prometheus Metrics

All quota and query budget metrics use opaque `ws_<16-hex-sha256>` workspace labels (`metrics.HashWorkspaceLabel`) to prevent raw tenant names from leaking to `/metrics` scrapers and to bound label cardinality:

* `sorobeacon_workspace_quota_limit{workspace, resource}` — configured limit for `monitors`, `rules`, `channels`, and `subscriptions`.
* `sorobeacon_workspace_quota_usage{workspace, resource}` — current usage for `monitors`, `rules`, `channels`, and `subscriptions`.
* `sorobeacon_workspace_quota_rejections_total{workspace, resource}` — total requests rejected due to exceeding a resource quota.
* `sorobeacon_workspace_query_cost_charged_total{workspace, surface}` — cumulative reconciled query cost units charged (`surface="rest"|"graphql"`).
* `sorobeacon_workspace_query_budget_remaining{workspace}` — remaining query cost budget tokens for a workspace.
* `sorobeacon_workspace_query_budget_throttled_total{workspace, surface}` — total read queries throttled when the workspace query cost budget was exhausted.
