-- Reverses 0014. Dropping the table takes its indexes with it, as the repo's
-- other table-creating migrations do.
--
-- Rolling back removes every scoped token, which signs out whatever CI was
-- using them. That is the correct outcome rather than a surprise: the previous
-- schema has nowhere to hold a scope list, so a token that survives a downgrade
-- could only survive as an unscoped credential.
DROP TABLE IF EXISTS api_tokens;
