-- Soft-delete monitors with a restore window.
--
-- Deleting a monitor cascades to its rules and channel attachments and is
-- irreversible. A misclick or a bad script loses configuration with no
-- recovery short of a database restore — which also rolls back everything
-- else.
--
-- A restore window turns an irreversible action into a reversible one.
--
-- Design decisions:
--   * deleted_at column on monitors: a timestamp when the monitor was
--     soft-deleted. NULL means active. Rules and monitor_channels are NOT
--     soft-deleted; they are hidden by the monitor's deleted_at and come
--     back intact on restore. This avoids the complexity of cascading
--     soft-deletes across multiple tables.
--   * Every read path that drives evaluation (ListMonitors, ListRules) filters
--     on deleted_at IS NULL. The API listing endpoints also filter by default.
--   * A ?permanent=true flag on DELETE does the hard delete. A separate
--     /trash endpoint lists soft-deleted monitors. A PATCH /monitors/{id}/restore
--     restores a monitor within the window.
--   * The existing retention job (prune.go) is the natural home for reaping
--     expired soft-deletes. It runs daily and hard-deletes monitors where
--     deleted_at is older than the restore window (default 30 days).
--   * Name uniqueness: a name is reusable only when no active monitor has it.
--     A soft-deleted monitor's name is still "taken" until hard-deleted, so
--     you cannot create "Foo" if a deleted "Foo" exists in the trash. This
--     prevents ambiguity when restoring.

ALTER TABLE monitors ADD COLUMN deleted_at TIMESTAMPTZ;

-- Index for the trash listing and reaper
CREATE INDEX monitors_deleted_at_idx ON monitors (deleted_at) WHERE deleted_at IS NOT NULL;

-- The existing unique index on (workspace_id, name) would allow a new monitor
-- with the same name as a soft-deleted one. We need a partial unique index
-- that only applies to active (deleted_at IS NULL) monitors.
-- First drop the old index (created in 0001_init and recreated in 0027_workspaces).
DROP INDEX IF EXISTS monitors_workspace_name_uidx;
CREATE UNIQUE INDEX monitors_workspace_name_uidx ON monitors (workspace_id, name) WHERE deleted_at IS NULL;