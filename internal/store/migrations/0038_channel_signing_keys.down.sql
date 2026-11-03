-- Revert channel signing keys table.
DROP INDEX IF EXISTS channel_signing_keys_workspace_idx;
DROP INDEX IF EXISTS channel_signing_keys_channel_status_idx;
DROP INDEX IF EXISTS channel_signing_keys_channel_key_id_idx;
DROP TABLE IF EXISTS channel_signing_keys;
