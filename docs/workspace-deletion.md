# Workspace Deletion

SoroBeacon supports permanent, bounded, and resumable deletion of a tenant workspace and every record belonging to it.

> **Warning — No Undo:** Workspace deletion is **permanent and irreversible**. Once confirmed and executed, all monitors, rules, notification channels (including encrypted credentials and signing keys), alerts, delivery history, dead letters, escalation policies, maintenance windows, inhibitions, saved searches, templates, scoped API tokens, usage rollups, quotas, and the workspace record itself are permanently removed. Always run a dry-run preview or export the workspace before confirming deletion.

---

## Endpoints

### 1. Dry-Run Preview (Safe, Read-Only)

Preview the exact row counts that would be deleted for a workspace without modifying any state:

```http
DELETE /api/v1/workspaces/{id}?dry_run=true
```
or:
```http
GET /api/v1/workspaces/{id}/delete?dry_run=true
```

### 2. Permanent Deletion (Requires Explicit Confirmation)

To permanently delete workspace `{id}`, pass `confirm={id}` via query parameter, `X-Confirm-Workspace` header, or JSON request body:

```http
DELETE /api/v1/workspaces/{id}?confirm={id}
```
or:
```http
POST /api/v1/workspaces/{id}/delete
Content-Type: application/json

{
  "confirm": "acme",
  "batch_size": 500
}
```

If `dry_run` is not `true` and `confirm` does not match `{id}`, the API refuses the request with `400 Bad Request` (`INVALID_ARGUMENT`).

### 3. Inspect Deletion Progress / Tombstone

```http
GET /api/v1/workspaces/{id}/delete
```

---

## Three-Phase Execution & Resumability

Deleting a workspace with a large alert history never runs as one unbounded lock-holding transaction:

1. **Phase 1 — Immediate Delivery & Evaluation Stop (`stopping` -> `deleting_alerts`)**:
   - Disables all monitors (`enabled = false`) and notification channels (`enabled = false`) in the workspace so the poller stops evaluating rules and no new notifications can be routed.
   - Revokes all active scoped API tokens (`revoked_at = now()`) in the workspace.
   - Clears `pending_digests` and cancels pending `alert_escalations` for the workspace.
   - Cancels any in-flight `notify.Dispatcher` HTTP/webhook delivery contexts, drops queued fairness submissions for the workspace, and removes per-channel circuit breakers and rate limiters.
2. **Phase 2 — Bounded, Resumable Alert Batching (`deleting_alerts`)**:
   - Deletes `delivery_attempts`, `dead_letters`, `alert_escalations`, `alert_dedup`, and `alerts` (across partitioned monthly alert tables on Postgres and SQLite) in bounded batches (`batch_size`, default `500`), checkpointing cumulative counts and `batches_completed` in `workspace_deletions` after every batch.
   - If interrupted, re-running the deletion resumes from the remaining alerts and accumulates into the existing `workspace_deletions` record.
3. **Phase 3 — Resource & Metadata Cleanup (`deleting_resources` -> `completed`)**:
   - Removes `alert_inhibitions`, `rule_absence_state`, `escalation_step_channels`, `escalation_steps`, `escalation_policies`, `maintenance_windows`, `backfills`, `monitor_channels`, `rules`, `monitors`, `channel_signing_keys`, `channel_ack_reconciliations`, `channels`, `saved_searches`, `monitor_templates`, `api_tokens` (clearing self-referential rotation pointers first), `workspace_usage_rollups`, `workspace_notification_usage_rollups`, `workspace_query_budget_state`, `workspace_quotas`, `idempotency_keys`, and the `workspaces` row itself.
   - Marks `workspace_deletions.status = 'completed'`. Re-running a completed deletion is idempotent and returns the completed summary (`idempotent_replay: true`).

---

## Security & Audit Trail

- **Authorization**: An instance operator (`RoleAdmin` on the default/system scope) may delete any workspace; a workspace-bound `RoleAdmin` credential may only delete its own workspace (`403 Forbidden` for any other workspace).
- **Audit Logging**: Every executed deletion writes an `audit_log` entry (`action = "delete"`, `target_type = "workspace"`) and emits a structured `slog` entry with `lower_snake_case` keys recording row counts and actor identity. Channel configurations and credentials are never logged or echoed.
