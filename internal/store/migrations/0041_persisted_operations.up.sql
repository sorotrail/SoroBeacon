CREATE TABLE IF NOT EXISTS persisted_operations (
    id VARCHAR(64) PRIMARY KEY,
    name TEXT NOT NULL DEFAULT '',
    document TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_persisted_operations_created_at ON persisted_operations (created_at DESC);
