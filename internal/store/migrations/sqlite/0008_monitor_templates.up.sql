-- SQLite equivalent of the Postgres 0008_monitor_templates. Postgres keeps
-- channel_ids in a BIGINT[]; SQLite has no array type, so the ids live in TEXT
-- as a JSON array — the same encoding monitors.contract_ids already uses.
-- rules and parameters are JSON documents for the same reason filter is TEXT
-- in 0007: SQLite has no JSONB, only text that the JSON1 functions can query.
CREATE TABLE monitor_templates (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL,
    description TEXT    NOT NULL DEFAULT '',
    rules       TEXT    NOT NULL DEFAULT '[]',
    channel_ids TEXT    NOT NULL DEFAULT '[]',
    parameters  TEXT    NOT NULL DEFAULT '[]',
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
