DROP INDEX IF EXISTS idx_audit_log_outcome;
DROP INDEX IF EXISTS idx_audit_log_workspace_created;
DROP INDEX IF EXISTS idx_audit_log_workspace_id;

ALTER TABLE audit_log DROP COLUMN record_hash;
ALTER TABLE audit_log DROP COLUMN prev_hash;
ALTER TABLE audit_log DROP COLUMN reason;
ALTER TABLE audit_log DROP COLUMN status_code;
ALTER TABLE audit_log DROP COLUMN outcome;
ALTER TABLE audit_log DROP COLUMN source_ip;
ALTER TABLE audit_log DROP COLUMN surface;
ALTER TABLE audit_log DROP COLUMN resource_id;
ALTER TABLE audit_log DROP COLUMN actor_type;
ALTER TABLE audit_log DROP COLUMN workspace_id;
