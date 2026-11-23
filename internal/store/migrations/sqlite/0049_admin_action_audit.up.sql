-- SQLite half of 0047_admin_action_audit: extend audit_log with workspace
-- scoping, actor classification, surface and source IP context, outcome/reason
-- tracking, and tamper-evident hash chain columns (prev_hash, record_hash).
ALTER TABLE audit_log ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE audit_log ADD COLUMN actor_type TEXT NOT NULL DEFAULT 'person';
ALTER TABLE audit_log ADD COLUMN resource_id TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN surface TEXT NOT NULL DEFAULT 'rest';
ALTER TABLE audit_log ADD COLUMN source_ip TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN outcome TEXT NOT NULL DEFAULT 'succeeded';
ALTER TABLE audit_log ADD COLUMN status_code INTEGER NOT NULL DEFAULT 200;
ALTER TABLE audit_log ADD COLUMN reason TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN prev_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN record_hash TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_audit_log_workspace_id ON audit_log(workspace_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_audit_log_workspace_created ON audit_log(workspace_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_log_outcome ON audit_log(outcome, id DESC);
