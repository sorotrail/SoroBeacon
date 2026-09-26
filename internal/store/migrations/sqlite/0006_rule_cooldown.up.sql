-- Per-rule alert cooldown state. SQLite serialises writers, so the SQLite
-- store takes the write lock for the whole CreateAlert transaction instead of
-- Postgres' SELECT ... FOR UPDATE; the state lives on the rule row either way
-- so the decision survives a poller restart. last_alert_at is null until the
-- rule first fires (never backfill it). suppressed_since_last counts matches
-- dropped inside the current window so the next alert can report them.
ALTER TABLE rules ADD COLUMN last_alert_at TEXT;
ALTER TABLE rules ADD COLUMN suppressed_since_last INTEGER NOT NULL DEFAULT 0;
