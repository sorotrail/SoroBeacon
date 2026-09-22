-- Supports GET /api/v1/channels/{id}/stats: aggregate attempts by channel
-- over a time window without scanning every delivery_attempts row.
CREATE INDEX delivery_attempts_channel_attempted_idx
    ON delivery_attempts (channel_id, attempted_at DESC);
