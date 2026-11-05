DROP TRIGGER IF EXISTS trg_alerts_rollup_delete;
DROP TRIGGER IF EXISTS trg_alerts_rollup_update;
DROP TRIGGER IF EXISTS trg_alerts_rollup_insert;
DROP INDEX IF EXISTS alert_stat_rollups_monitor_idx;
DROP INDEX IF EXISTS alert_stat_rollups_workspace_idx;
DROP TABLE IF EXISTS alert_stat_rollups;
