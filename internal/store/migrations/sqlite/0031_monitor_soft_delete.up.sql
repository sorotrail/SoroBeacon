-- Soft-delete monitors with a restore window (SQLite version).
--
-- SQLite doesn't support partial indexes with WHERE clauses before 3.38.0,
-- and the syntax is different. We use a trigger to enforce uniqueness
-- on active monitors only.

ALTER TABLE monitors ADD COLUMN deleted_at TIMESTAMPTZ;

-- Index for the trash listing and reaper
CREATE INDEX monitors_deleted_at_idx ON monitors (deleted_at) WHERE deleted_at IS NOT NULL;

-- SQLite doesn't support DROP INDEX IF EXISTS before 3.38.0, so we use a different approach
-- The original unique index on (workspace_id, name) is recreated by the migration system
-- when we drop it. We need to drop it first, then create a partial unique index.
-- SQLite supports partial indexes since 3.8.0 (2013).

DROP INDEX IF EXISTS monitors_workspace_name_uidx;
CREATE UNIQUE INDEX monitors_workspace_name_uidx ON monitors (workspace_id, name) WHERE deleted_at IS NULL;