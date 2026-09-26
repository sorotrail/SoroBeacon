-- SQLite equivalent of the Postgres 0014_channel_digest. JSONB becomes TEXT
-- and timestamps use the fixed strftime format; the foreign key cascades so
-- pending rows follow their channel, exactly as on Postgres.
ALTER TABLE channels ADD COLUMN digest_mode TEXT NOT NULL DEFAULT '';
ALTER TABLE channels ADD COLUMN digest_window_seconds INTEGER NOT NULL DEFAULT 0;

CREATE TABLE pending_digests (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    channel_id INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    payload    TEXT    NOT NULL,
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX pending_digests_channel_idx ON pending_digests (channel_id, id);
