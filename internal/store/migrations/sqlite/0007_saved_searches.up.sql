-- SQLite equivalent of the Postgres 0007_saved_searches. JSONB becomes TEXT
-- (queried with json_extract), timestamps use the fixed strftime format, and
-- BOOLEAN becomes INTEGER 0/1. The partial unique index on the default row is
-- supported by SQLite and keeps "exactly one default" true on both backends.
CREATE TABLE saved_searches (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL,
    filter     TEXT    NOT NULL DEFAULT '{}',
    is_default INTEGER NOT NULL DEFAULT 0,
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE UNIQUE INDEX saved_searches_default_idx ON saved_searches (is_default) WHERE is_default = 1;
