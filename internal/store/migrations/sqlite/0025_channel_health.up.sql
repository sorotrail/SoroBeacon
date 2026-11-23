-- SQLite equivalent of the Postgres 0009_channel_health. Per-channel delivery
-- health: consecutive_failures counts failed deliveries since the last success
-- and consecutive_permanent_failures counts only the failures a channel will
-- not recover from on its own (401/403/404), which is what auto-disable
-- triggers on. Timestamps are TEXT in the store's fixed UTC format so they
-- compare chronologically, exactly as the other time columns do; NULL is the
-- "never happened" case and parses to the zero time through parseSQLiteTime.
ALTER TABLE channels ADD COLUMN consecutive_failures INTEGER NOT NULL DEFAULT 0;
ALTER TABLE channels ADD COLUMN consecutive_permanent_failures INTEGER NOT NULL DEFAULT 0;
ALTER TABLE channels ADD COLUMN last_error TEXT NOT NULL DEFAULT '';
ALTER TABLE channels ADD COLUMN last_error_at TEXT;
ALTER TABLE channels ADD COLUMN last_success_at TEXT;
ALTER TABLE channels ADD COLUMN disabled_at TEXT;
