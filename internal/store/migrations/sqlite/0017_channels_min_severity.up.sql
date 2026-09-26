ALTER TABLE channels ADD COLUMN min_severity TEXT;
-- SQLite doesn't support CHECK constraints on ALTER TABLE ADD COLUMN
-- The constraint is enforced at the application layer