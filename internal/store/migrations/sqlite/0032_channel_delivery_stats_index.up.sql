-- SQLite version of the per-channel delivery statistics index (issue #117).
-- The same composite (channel_id, attempted_at) shape as Postgres: SQLite
-- attempts store attempted_at as fixed-width UTC TEXT, so the b-tree over the
-- pair orders chronologically exactly like the Postgres one, and the backend's
-- GetChannelDeliveryStats filters with the same predicates.
CREATE INDEX IF NOT EXISTS delivery_attempts_channel_attempted_idx
    ON delivery_attempts (channel_id, attempted_at);
