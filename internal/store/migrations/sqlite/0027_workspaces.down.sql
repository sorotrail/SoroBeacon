-- Reverses the SQLite 0012. The unique-index swap goes back first, because the
-- column it references is dropped below.
--
-- Restoring 0007's global "one default search" index fails when more than one
-- workspace has a default, and that is the correct outcome: rolling a tenanted
-- database back to a single-tenant schema has to decide which default survives,
-- which this migration cannot do without silently discarding someone's saved
-- view. Fix the rows, then roll back.

DROP INDEX IF EXISTS saved_searches_default_idx;
CREATE UNIQUE INDEX saved_searches_default_idx ON saved_searches (is_default) WHERE is_default = 1;

DROP INDEX IF EXISTS monitor_templates_workspace_idx;
DROP INDEX IF EXISTS saved_searches_workspace_idx;
DROP INDEX IF EXISTS alerts_workspace_created_idx;
DROP INDEX IF EXISTS channels_workspace_idx;
DROP INDEX IF EXISTS monitors_workspace_name_idx;
DROP INDEX IF EXISTS monitors_workspace_idx;

ALTER TABLE monitor_templates  DROP COLUMN workspace_id;
ALTER TABLE saved_searches     DROP COLUMN workspace_id;
ALTER TABLE alerts             DROP COLUMN workspace_id;
ALTER TABLE channels           DROP COLUMN workspace_id;
ALTER TABLE monitors           DROP COLUMN workspace_id;

DROP TABLE IF EXISTS workspaces;
