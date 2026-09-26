-- Parity with the Postgres 0007. JSONB becomes TEXT (the Go layer marshals
-- the filter, exactly as it does for monitors.contract_ids), TIMESTAMPTZ
-- becomes TEXT in the fixed 'YYYY-MM-DDTHH:MM:SS.mmmZ' format so lexicographic
-- order equals chronological order, and BOOLEAN becomes INTEGER 0/1. The
-- partial unique index keeps at most one default row, mirroring the Postgres
-- index, so "set this one default" stays a two-statement operation.
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
