-- Rollback for 0009_alert_inhibitions.
DROP INDEX IF EXISTS alerts_inhibited_by_idx;
ALTER TABLE alerts DROP COLUMN IF EXISTS inhibited_by_rule_id;
DROP INDEX IF EXISTS alert_inhibitions_target_idx;
DROP TABLE IF EXISTS alert_inhibitions;
