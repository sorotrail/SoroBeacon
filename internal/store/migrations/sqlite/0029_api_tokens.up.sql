-- SQLite equivalent of the Postgres 0014_api_tokens. Same model and the same
-- reasons (see the Postgres file); the dialect differences are BIGSERIAL →
-- INTEGER PRIMARY KEY AUTOINCREMENT, TIMESTAMPTZ → the fixed strftime TEXT
-- format, and a nullable timestamp simply being nullable TEXT.

CREATE TABLE api_tokens (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    workspace_id TEXT    NOT NULL DEFAULT 'default',
    name         TEXT    NOT NULL,
    token_hash   TEXT    NOT NULL,
    prefix       TEXT    NOT NULL,
    scopes       TEXT    NOT NULL DEFAULT '',
    expires_at   TEXT,
    last_used_at TEXT,
    revoked_at   TEXT,
    created_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE UNIQUE INDEX api_tokens_hash_idx ON api_tokens (token_hash);
CREATE INDEX api_tokens_workspace_idx ON api_tokens (workspace_id, id DESC);
