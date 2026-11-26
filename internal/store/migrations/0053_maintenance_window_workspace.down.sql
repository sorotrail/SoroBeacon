DROP INDEX IF EXISTS maintenance_windows_workspace_idx;
ALTER TABLE maintenance_windows DROP COLUMN workspace_id;
