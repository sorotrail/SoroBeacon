-- Per-channel HTTP delivery timeout in seconds.
-- Defaults to 15s to match the previous package-level client timeout, ensuring
-- existing channels behave identically after migration.
-- The CHECK constraint enforces the bounded range: at least 1s and at most 60s
-- (preventing wedged delivery workers).
ALTER TABLE channels ADD COLUMN timeout INTEGER NOT NULL DEFAULT 15
    CHECK (timeout >= 1 AND timeout <= 60);
