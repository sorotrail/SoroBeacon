-- SQLite equivalent of the Postgres 0008_monitor_templates. rules and
-- parameters are JSONB there and TEXT here; the Postgres BIGINT[] channel_ids
-- becomes a JSON array in TEXT, since SQLite has no array type. The store
-- marshals and unmarshals it the same way in both backends, so a caller never
-- sees the difference.
CREATE TABLE monitor_templates (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL,
    description TEXT    NOT NULL DEFAULT '',
    rules       TEXT    NOT NULL DEFAULT '[]',
    channel_ids TEXT    NOT NULL DEFAULT '[]',
    parameters  TEXT    NOT NULL DEFAULT '[]',
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
-- SQLite equivalent of the Postgres 0008_monitor_templates. JSONB becomes TEXT
-- and the BIGINT[] of channel ids becomes a JSON array in TEXT, since SQLite
-- has no array type; the Go layer marshals the slice in and out.
CREATE TABLE monitor_templates (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    rules       TEXT NOT NULL DEFAULT '[]',
    channel_ids TEXT NOT NULL DEFAULT '[]',
    parameters  TEXT NOT NULL DEFAULT '[]',
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
