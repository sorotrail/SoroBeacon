-- Reconcile delivery against acknowledgement per channel and workspace.
CREATE TABLE IF NOT EXISTS channel_ack_reconciliations (
    channel_id BIGINT PRIMARY KEY REFERENCES channels(id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL,
    delivered_total BIGINT NOT NULL DEFAULT 0,
    acknowledged_total BIGINT NOT NULL DEFAULT 0,
    unacknowledged_total BIGINT NOT NULL DEFAULT 0,
    unack_rate DOUBLE PRECISION,
    flagged BOOLEAN NOT NULL DEFAULT FALSE,
    computed_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_channel_ack_reconciliations_ws ON channel_ack_reconciliations (workspace_id);
