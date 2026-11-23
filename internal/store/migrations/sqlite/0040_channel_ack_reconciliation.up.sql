-- SQLite equivalent of Postgres 0040_channel_ack_reconciliation.
CREATE TABLE IF NOT EXISTS channel_ack_reconciliations (
    channel_id INTEGER PRIMARY KEY REFERENCES channels(id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL,
    delivered_total INTEGER NOT NULL DEFAULT 0,
    acknowledged_total INTEGER NOT NULL DEFAULT 0,
    unacknowledged_total INTEGER NOT NULL DEFAULT 0,
    unack_rate REAL,
    flagged BOOLEAN NOT NULL DEFAULT 0,
    computed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_channel_ack_reconciliations_ws ON channel_ack_reconciliations (workspace_id);
