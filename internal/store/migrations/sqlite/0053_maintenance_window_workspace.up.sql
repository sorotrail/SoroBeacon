ALTER TABLE maintenance_windows ADD COLUMN workspace_id TEXT NOT NULL DEFAULT '';
UPDATE maintenance_windows SET workspace_id = 'default';
CREATE INDEX maintenance_windows_workspace_idx ON maintenance_windows (workspace_id);
