ALTER TABLE channels DROP COLUMN disabled_at;
ALTER TABLE channels DROP COLUMN last_success_at;
ALTER TABLE channels DROP COLUMN last_error_at;
ALTER TABLE channels DROP COLUMN last_error;
ALTER TABLE channels DROP COLUMN consecutive_permanent_failures;
ALTER TABLE channels DROP COLUMN consecutive_failures;
