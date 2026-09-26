ALTER TABLE alerts ADD COLUMN severity TEXT NOT NULL DEFAULT 'warning';
-- SQLite doesn't support CHECK constraints on ALTER TABLE ADD COLUMN
-- The constraint is enforced at the application layer