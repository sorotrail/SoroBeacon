-- Ledger close time of the most recent matching event. Nullable: never
-- backfill a fake timestamp for monitors that have not matched yet.
ALTER TABLE monitors ADD COLUMN last_matched_at TIMESTAMPTZ;
