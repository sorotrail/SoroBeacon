-- 0047_workspace_retention.up.sql: per-workspace alert retention policies.
--
-- A workspace with no row in workspace_retention_policies inherits the
-- instance-wide ALERT_RETENTION configuration ("instance" or "unset" policy
-- source). When an explicit policy is stored, retention_seconds > 0 governs
-- both per-workspace alert pruning and read-time visibility for that tenant,
-- while monthly partition dropping waits until the longest retention horizon
-- across all workspaces and the instance default has elapsed.

CREATE TABLE IF NOT EXISTS workspace_retention_policies (
    workspace_id      TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    retention_seconds BIGINT NOT NULL CHECK (retention_seconds > 0),
    updated_by        TEXT NOT NULL DEFAULT '',
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
