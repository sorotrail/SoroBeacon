-- Overlapping API token rotation: successor/predecessor tracking and safe expiry
ALTER TABLE api_tokens ADD COLUMN rotated_from_id BIGINT REFERENCES api_tokens(id);
ALTER TABLE api_tokens ADD COLUMN successor_id BIGINT REFERENCES api_tokens(id);
ALTER TABLE api_tokens ADD COLUMN rotated_at TIMESTAMPTZ;
ALTER TABLE api_tokens ADD COLUMN pre_rotation_expires_at TIMESTAMPTZ;

CREATE INDEX api_tokens_rotated_from_idx ON api_tokens (rotated_from_id);
CREATE INDEX api_tokens_successor_idx ON api_tokens (successor_id);
