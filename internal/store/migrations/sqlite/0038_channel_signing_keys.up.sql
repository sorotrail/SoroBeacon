-- Channel signing keys: multiple HMAC-SHA256 signing keys per webhook channel
-- (SQLite version).

CREATE TABLE channel_signing_keys (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    channel_id   INTEGER NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL DEFAULT 'default',
    key_id       TEXT NOT NULL,
    secret       TEXT NOT NULL,
    is_current   INTEGER NOT NULL DEFAULT 0,
    status       TEXT NOT NULL DEFAULT 'active',
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    expires_at   TEXT,
    last_used_at TEXT,
    revoked_at   TEXT
);

CREATE UNIQUE INDEX channel_signing_keys_channel_key_id_idx ON channel_signing_keys (channel_id, key_id);
CREATE INDEX channel_signing_keys_channel_status_idx ON channel_signing_keys (channel_id, status);
CREATE INDEX channel_signing_keys_workspace_idx ON channel_signing_keys (workspace_id, id DESC);
