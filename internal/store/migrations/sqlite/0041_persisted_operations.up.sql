CREATE TABLE IF NOT EXISTS persisted_operations (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL DEFAULT '',
    document TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_persisted_operations_created_at ON persisted_operations (created_at DESC);
