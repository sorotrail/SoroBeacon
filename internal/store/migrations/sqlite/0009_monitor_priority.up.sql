-- SQLite equivalent of the Postgres 0009_monitor_priority. SQLite cannot add
-- a column with a CHECK constraint via ALTER TABLE, so the closed vocabulary
-- is enforced by the Go write path (store.ParsePriority / Normalized) and the
-- DEFAULT keeps existing monitors in the middle tier.
ALTER TABLE monitors ADD COLUMN priority TEXT NOT NULL DEFAULT 'normal';

CREATE INDEX monitors_priority_idx ON monitors (priority);
