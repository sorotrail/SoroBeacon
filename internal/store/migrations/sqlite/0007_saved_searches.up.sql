-- SQLite equivalent of the Postgres 0007_saved_searches. As in 0001_init,
-- JSONB becomes TEXT holding JSON (saved searches filter with json_extract)
-- and BOOLEAN becomes INTEGER 0/1.
--
-- The Postgres partial unique index is written as `WHERE is_default = TRUE`
-- and needs no such clause here: SQLite stores false as 0, which is not NULL,
-- so the index would otherwise allow only one non-default row too. The WHERE
-- clause is what keeps it "at most one default" rather than "at most one row".
CREATE TABLE saved_searches (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL,
    filter     TEXT    NOT NULL DEFAULT '{}',
    is_default INTEGER NOT NULL DEFAULT 0 CHECK (is_default IN (0, 1)),
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE UNIQUE INDEX saved_searches_default_idx ON saved_searches (is_default) WHERE is_default = 1;
