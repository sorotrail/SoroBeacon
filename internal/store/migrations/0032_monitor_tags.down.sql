-- Revert monitor tags.
DROP INDEX IF EXISTS monitors_tags_gin_idx;
ALTER TABLE monitors DROP COLUMN IF EXISTS tags;
