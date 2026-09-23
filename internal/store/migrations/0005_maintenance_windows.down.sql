ALTER TABLE alerts DROP COLUMN suppression_reason;
ALTER TABLE alerts DROP COLUMN suppressed;
DROP INDEX maintenance_windows_contract_idx;
DROP INDEX maintenance_windows_monitor_idx;
DROP INDEX maintenance_windows_window_idx;
DROP TABLE maintenance_windows;
