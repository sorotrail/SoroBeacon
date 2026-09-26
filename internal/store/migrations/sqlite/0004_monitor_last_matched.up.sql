-- Ledger close time of the most recent matching event. Nullable: never
-- backfill a fake timestamp for monitors that have not matched yet. Stored in
-- the same fixed TEXT timestamp format as every other time column.
ALTER TABLE monitors ADD COLUMN last_matched_at TEXT;
