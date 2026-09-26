DROP INDEX IF EXISTS monitors_priority_idx;
ALTER TABLE monitors DROP COLUMN IF EXISTS priority;
