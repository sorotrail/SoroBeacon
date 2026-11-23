-- Revert monitor tags (SQLite version).
-- SQLite doesn't support DROP COLUMN before 3.35.0; modern versions do.
ALTER TABLE monitors DROP COLUMN tags;
