-- Append-only log of configuration changes to monitors, rules and channels.
-- diff holds field names only, never values: a channel's config carries
-- webhook URLs, bot tokens and SMTP credentials, so what changed is recorded
-- but never what it changed to. There is no UPDATE or DELETE path in the
-- store for this table.
CREATE TABLE audit_log (
    id          BIGSERIAL PRIMARY KEY,
    actor       TEXT NOT NULL DEFAULT '',
    action      TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_id   BIGINT NOT NULL DEFAULT 0,
    diff        JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The list endpoint is newest-first, optionally narrowed by target.
CREATE INDEX audit_log_created_at_idx ON audit_log (created_at DESC);
CREATE INDEX audit_log_target_idx ON audit_log (target_type, target_id, created_at DESC);
