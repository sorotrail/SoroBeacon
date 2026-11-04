-- 0047_workspace_retention.down.sql: remove per-workspace alert retention policies.

DROP TABLE IF EXISTS workspace_retention_policies;
