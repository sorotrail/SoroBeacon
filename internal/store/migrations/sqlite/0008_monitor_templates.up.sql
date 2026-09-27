-- Parity with the Postgres 0008. rules, channel_ids and parameters are JSONB
-- and BIGINT[] upstream; SQLite stores all three as TEXT holding the same JSON
-- bytes, so the Go layer round-trips them through encoding/json unchanged.
CREATE TABLE monitor_templates (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL,
    description TEXT    NOT NULL DEFAULT '',
    rules       TEXT    NOT NULL DEFAULT '[]',
    channel_ids TEXT    NOT NULL DEFAULT '[]',
    parameters  TEXT    NOT NULL DEFAULT '[]',
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
