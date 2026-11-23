-- Revert soft-delete monitors (SQLite version)
DROP INDEX IF EXISTS monitors_workspace_name_uidx;
DROP INDEX IF EXISTS monitors_deleted_at_idx;
-- SQLite doesn't support DROP COLUMN before 3.35.0 (2021)
-- The migration system will handle this if the version supports it
-- For older SQLite, the column remains but is ignored
ALTER TABLE monitors DROP COLUMN deleted_at;

-- Restore the original unique index (without the WHERE clause)
CREATE UNIQUE INDEX monitors_workspace_name_uidx ON monitors (workspace_id, name);