-- Revert soft-delete monitors
DROP INDEX IF EXISTS monitors_workspace_name_uidx;
DROP INDEX IF EXISTS monitors_deleted_at_idx;
ALTER TABLE monitors DROP COLUMN IF EXISTS deleted_at;

-- Restore the original unique index (without the WHERE clause)
CREATE UNIQUE INDEX monitors_workspace_name_uidx ON monitors (workspace_id, name);