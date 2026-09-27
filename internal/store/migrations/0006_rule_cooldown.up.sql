-- Per-rule alert cooldown state, kept on the rule row so the decision can be
-- serialised with the rule's own lock and survives a poller restart.
-- last_alert_at is null until the rule first fires (never backfill it).
-- suppressed_since_last counts matches dropped inside the current window so
-- the next alert can report how many were suppressed.
ALTER TABLE rules ADD COLUMN last_alert_at TIMESTAMPTZ;
ALTER TABLE rules ADD COLUMN suppressed_since_last BIGINT NOT NULL DEFAULT 0;
