-- 0047_workspace_retention.down.sql: remove per-workspace alert retention policies (SQLite).

DROP TABLE IF EXISTS workspace_retention_policies;
