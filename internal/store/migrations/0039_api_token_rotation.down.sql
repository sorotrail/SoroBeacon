-- Revert overlapping API token rotation columns.
DROP INDEX IF EXISTS api_tokens_successor_idx;
DROP INDEX IF EXISTS api_tokens_rotated_from_idx;

ALTER TABLE api_tokens DROP COLUMN IF EXISTS pre_rotation_expires_at;
ALTER TABLE api_tokens DROP COLUMN IF EXISTS rotated_at;
ALTER TABLE api_tokens DROP COLUMN IF EXISTS successor_id;
ALTER TABLE api_tokens DROP COLUMN IF EXISTS rotated_from_id;
