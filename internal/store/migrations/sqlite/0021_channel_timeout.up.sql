-- Per-channel HTTP delivery timeout in seconds.
-- Defaults to 15s to match the previous package-level client timeout, ensuring
-- existing channels behave identically after migration.
ALTER TABLE channels ADD COLUMN timeout INTEGER NOT NULL DEFAULT 15;
