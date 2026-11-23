-- Overlapping API token rotation: successor/predecessor tracking and safe expiry (SQLite)
ALTER TABLE api_tokens ADD COLUMN rotated_from_id INTEGER REFERENCES api_tokens(id);
ALTER TABLE api_tokens ADD COLUMN successor_id INTEGER REFERENCES api_tokens(id);
ALTER TABLE api_tokens ADD COLUMN rotated_at TEXT;
ALTER TABLE api_tokens ADD COLUMN pre_rotation_expires_at TEXT;

CREATE INDEX api_tokens_rotated_from_idx ON api_tokens (rotated_from_id);
CREATE INDEX api_tokens_successor_idx ON api_tokens (successor_id);
