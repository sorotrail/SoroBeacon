-- 0047_workspace_retention.up.sql: per-workspace alert retention policies (SQLite).

CREATE TABLE IF NOT EXISTS workspace_retention_policies (
    workspace_id      TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    retention_seconds INTEGER NOT NULL CHECK (retention_seconds > 0),
    updated_by        TEXT NOT NULL DEFAULT '',
    updated_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
