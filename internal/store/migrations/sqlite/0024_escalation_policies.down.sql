DROP INDEX IF EXISTS alert_escalations_due_idx;
DROP TABLE IF EXISTS alert_escalations;
DROP TABLE IF EXISTS escalation_step_channels;
DROP TABLE IF EXISTS escalation_steps;
DROP TABLE IF EXISTS escalation_policies;
ALTER TABLE alerts DROP COLUMN acknowledged_at;
