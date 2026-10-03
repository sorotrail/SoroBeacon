-- Scoped API tokens: credentials that carry a scope list, an expiry, a
-- revocation timestamp and a last-used stamp, so a token handed to CI can be
-- limited to creating monitors and retired without rotating the instance-wide
-- API_TOKEN.
--
-- Design decisions (recorded where the schema keeps them next to the change):
--
--   * token_hash, never the token. The column is the hex SHA-256 of the secret,
--     so a database dump cannot mint anything and the secret can be shown only
--     once, at creation. SHA-256 rather than bcrypt/argon2 because these are
--     256 bits of crypto/rand, not human passwords: an offline attack against
--     full entropy is infeasible whatever the hash, while a slow KDF would cost
--     the one thing the auth path needs — an equality lookup. The unique index
--     below is that lookup.
--
--   * prefix is display text, not a security control: the first characters of
--     the secret, kept so the dashboard can tell two tokens apart without
--     holding either. It is a prefix of the plaintext, not of the hash, so
--     showing it cannot narrow a brute-force search over the digest.
--
--   * scopes is a comma-separated list rather than a join table. A token's
--     scopes are read only together with the token, on every request that
--     authenticates; a second table would add a query to that path and permit
--     nothing this issue needs to enforce. The vocabulary is validated in Go
--     (auth.ParseScopes), so the column holds no free text.
--
--   * revoked_at instead of a boolean revoked flag, so "when" survives alongside
--     "whether", and NULL keeps the pre-revocation state unambiguous. Expired
--     and revoked are separate columns but one answer at auth time
--     (Token.Live), which is what stops a caller from telling them apart.
--
--   * No foreign key to workspaces, and workspace_id NOT NULL DEFAULT
--     'default', both for the reasons 0012 records: deleting a tenant is a
--     separate operation, and a token's workspace comes from the credential
--     that minted it, never from the request body.
--
--   * token_hash is unique instance-wide, not per workspace. Two tenants
--     holding the same secret would be one credential with two identities,
--     which is a bug the schema should refuse rather than resolve.

CREATE TABLE api_tokens (
    id           BIGSERIAL   PRIMARY KEY,
    workspace_id TEXT        NOT NULL DEFAULT 'default',
    name         TEXT        NOT NULL,
    token_hash   TEXT        NOT NULL,
    prefix       TEXT        NOT NULL,
    scopes       TEXT        NOT NULL DEFAULT '',
    expires_at   TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The authentication path: one indexed read per request.
CREATE UNIQUE INDEX api_tokens_hash_idx ON api_tokens (token_hash);
-- The dashboard listing, which is tenant-scoped like every other table's.
CREATE INDEX api_tokens_workspace_idx ON api_tokens (workspace_id, id DESC);
