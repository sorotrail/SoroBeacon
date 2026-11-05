DROP TRIGGER IF EXISTS trg_alerts_rollup ON alerts;
DROP FUNCTION IF EXISTS maintain_alert_stat_rollup();
DROP INDEX IF EXISTS alert_stat_rollups_monitor_idx;
DROP INDEX IF EXISTS alert_stat_rollups_workspace_idx;
DROP TABLE IF EXISTS alert_stat_rollups;
