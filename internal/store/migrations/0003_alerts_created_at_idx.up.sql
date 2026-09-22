-- Speeds batch retention deletes that filter alerts by created_at.
CREATE INDEX alerts_created_at_idx ON alerts (created_at);
