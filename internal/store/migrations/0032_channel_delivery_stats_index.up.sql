-- Per-channel delivery statistics (issue #117):
-- GET /api/v1/channels/{id}/stats aggregates delivery_attempts by channel_id
-- over an attempted_at window. The existing indexes address attempts by alert
-- (delivery_attempts_alert_idx from 0001) or by the partitioned parent, so a
-- per-channel window query had no index to use. The composite (channel_id,
-- attempted_at) index lets PostgreSQL scan exactly one channel's attempts
-- inside the requested window, newest rows included in the scan order.
CREATE INDEX IF NOT EXISTS delivery_attempts_channel_attempted_idx
    ON delivery_attempts (channel_id, attempted_at);
