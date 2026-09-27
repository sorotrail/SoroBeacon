-- Retire the 0001 "encrypt this column at rest" TODO. Encryption is a
-- property of the application (internal/store applies it when
-- CONFIG_ENCRYPTION_KEY is set), not of the column type, so the column stays
-- JSONB. New and updated rows hold a JSON envelope; rows written before a key
-- was configured stay valid plaintext until they are rewritten (lazy
-- re-encryption on write).
COMMENT ON COLUMN channels.config IS 'Channel config. Encrypted at rest by the application when CONFIG_ENCRYPTION_KEY is set; rows written before a key was configured are legacy plaintext until rewritten.';
