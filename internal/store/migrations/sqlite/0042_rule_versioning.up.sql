-- SQLite half of 0042_rule_versioning: track rule revisions and stamp each
-- alert with the rule version that produced it.
ALTER TABLE rules ADD COLUMN version INTEGER NOT NULL DEFAULT 1;
ALTER TABLE alerts ADD COLUMN rule_version INTEGER NOT NULL DEFAULT 1;
