-- Rollback for 0009_alert_inhibitions.
ALTER TABLE alerts DROP COLUMN inhibited_by_rule_id;
DROP TABLE IF EXISTS alert_inhibitions;
