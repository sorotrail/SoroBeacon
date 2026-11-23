-- Track rule revisions and stamp each alert with the rule version that
-- produced it. Starting at 1 keeps existing rules and historical alerts on a
-- valid positive revision without backfilling NULLs.
ALTER TABLE rules ADD COLUMN version INTEGER NOT NULL DEFAULT 1;
ALTER TABLE alerts ADD COLUMN rule_version INTEGER NOT NULL DEFAULT 1;
