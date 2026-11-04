# Workspace Data Export

SoroBeacon supports exporting everything belonging to a single workspace (`workspace.ID`) without including any other workspace's data or shared instance-only tables. This complements `sorobeacon backup` (which backs up the entire database instance) and fits tenant offboarding, compliance/data-portability requests, and workspace migration between instances.

## HTTP API Endpoints

| Method & Path | Description | Minimum Role / Scope |
| --- | --- | --- |
| `GET /api/v1/workspaces/{id}/export` | Export all data belonging to workspace `{id}`. | `viewer` (`monitors:read`) |
| `GET /api/v1/workspace/export` | Export all data belonging to the authenticated caller's workspace (or `?workspace=<id>` for cross-workspace operators). | `viewer` (`monitors:read`) |

### Authorization

- **Workspace-bound credentials** (static tokens in `WORKSPACE_TOKENS`, database-backed scoped tokens `sb_...`, OIDC group mappings, or dashboard sessions bound to a workspace) may export **only their own workspace**.
- A token bound to workspace `alpha` requesting `GET /api/v1/workspaces/beta/export` or `GET /api/v1/workspace/export?workspace=beta` is rejected with `403 Forbidden`.
- **Instance operators** (unrestricted `admin` credentials in the `default` workspace) may export any workspace via `GET /api/v1/workspaces/{id}/export` or `GET /api/v1/workspace/export?workspace=<id>`.

### Query Parameters & Content Negotiation

| Parameter | Values | Default | Behaviour |
| --- | --- | --- | --- |
| `include_secrets` | `true` \| `false` | `false` | When `false`, channel secret fields (`url`, `webhook_url`, `secret`, `signing_secret`, `bot_token`, `password`, `routing_key`, `api_key`, `headers.Authorization`, etc.), `channel_signing_keys.secret`, and `api_tokens.token_hash` are replaced with `"REDACTED"`, and the manifest sets `"secrets_redacted": true, "include_secrets": false`. When `true` (**requires `admin` role / `*` scope**; `viewer` or `monitors:read`-only tokens receive `403 Forbidden`), encrypted channel configs and signing key secrets are decrypted via the store's `ConfigCipher` and exported in cleartext alongside `api_tokens.token_hash`. Raw API token secrets are never stored in SoroBeacon and are never recoverable; migrated workspaces must mint or rotate API tokens on the target instance if raw bearer tokens are not already retained by the tenant. |
| `include_alerts` | `true` \| `false` | `false` | When `false`, only workspace configuration, operational state, and usage rollups are exported (`"alerts_included": false`). When `true`, high-volume alert history tables (`alerts`, `alert_dedup`, `delivery_attempts`, `dead_letters`, `alert_escalations`) are streamed in bounded keyset pages (`DefaultWorkspaceExportPageSize = 250`) without buffering the full table in memory. |
| `page_size` | `1..5000` | `250` | Keyset pagination batch size used when streaming high-volume alert history tables (`include_alerts=true`). |
| `format` | `json` \| `ndjson` | `json` | Selects streaming JSON bundle (`application/json`) or newline-delimited JSON (`application/x-ndjson`). Sending `Accept: application/x-ndjson` also selects NDJSON when `format` is omitted. |

## Export Format & Versioning

Every export is self-describing and deterministic (all rows are emitted in ascending primary-key order `ORDER BY <pk> ASC`):

- `format`: `"sorobeacon.workspace-export"`
- `version`: `1`
- `manifest`:
  - `workspace_id`: exported tenant identifier
  - `exported_at`: UTC timestamp (`RFC3339`)
  - `include_secrets` / `secrets_redacted`: explicit secret-handling flags
  - `include_alerts` / `alerts_included`: explicit alert-history flags
  - `tables`: ordered list of tables covered by this export
- `summary`:
  - `table_counts`: map of table name to exported row count
  - `total_rows`: total number of rows emitted across all tables

In **NDJSON mode** (`?format=ndjson`), the first line is `{"kind":"manifest","manifest":{...}}`, followed by one `{"kind":"row","table":"<table_name>","data":{...}}` line per row in deterministic table and primary-key order, and terminated by `{"kind":"summary","summary":{...}}`.

## Schema Coverage & Mechanical Guard

A mechanical schema completeness test (`TestWorkspaceExportCoversEverySchemaTable` in `internal/store/workspace_export_test.go`) parses every `CREATE TABLE` across all SQLite and PostgreSQL migrations and fails the build if a new table is added without being classified in either `WorkspaceExportTables()` or `NonExportedInstanceTables`.

### Exported Workspace Tables (28 tables)

1. **Configuration, Operational State & Rollups (`WorkspaceExportConfigTables` — 23 tables, always exported)**:
   - `workspaces`
   - `workspace_quotas`
   - `workspace_query_budget_state`
   - `workspace_retention_policies`
   - `channels`
   - `channel_signing_keys`
   - `channel_ack_reconciliations`
   - `pending_digests`
   - `monitors` (active and soft-deleted)
   - `monitor_channels`
   - `rules` (including cooldown state)
   - `rule_absence_state`
   - `alert_inhibitions`
   - `escalation_policies`
   - `escalation_steps`
   - `escalation_step_channels`
   - `maintenance_windows`
   - `backfills`
   - `saved_searches`
   - `monitor_templates`
   - `api_tokens` (metadata and rotation lineage; `token_hash` redacted unless `include_secrets=true`)
   - `workspace_usage_rollups`
   - `workspace_notification_usage_rollups`

2. **Alert History (`WorkspaceExportAlertTables` — 5 tables, streamed when `include_alerts=true`)**:
   - `alerts`
   - `alert_dedup`
   - `delivery_attempts`
   - `dead_letters`
   - `alert_escalations`

### Excluded Instance-Wide Tables (`NonExportedInstanceTables` — 12 tables)

The following tables are instance-wide infrastructure rather than single-tenant data and are deliberately excluded:
- `schema_migrations`: database migration bookkeeping
- `ingest_state`, `network_ingest_state`: chain-wide poller checkpoints
- `ledger_hashes`, `network_ledger_hashes`: chain-wide ledger hash ring buffer for reorg detection
- `contract_ingest_cursors`: per-network contract ingest cursors shared across workspaces
- `retention_gaps`: instance-wide poller ledger retention gap log
- `event_archive`: instance-wide raw decoded Stellar event replay corpus
- `audit_log`: instance-wide append-only operator audit log
- `persisted_operations`: instance-wide vetted GraphQL persisted operation registry
- `idempotency_keys`: ephemeral HTTP transport idempotency response cache (24h TTL)
- `workspace_deletions`: durable workspace deletion progress and completion tombstone log
