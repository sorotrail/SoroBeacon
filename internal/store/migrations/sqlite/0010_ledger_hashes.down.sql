DROP INDEX IF EXISTS alerts_ledger_idx;
ALTER TABLE alerts DROP COLUMN IF EXISTS retracted_at;
ALTER TABLE alerts DROP COLUMN IF EXISTS ledger;
DROP TABLE IF EXISTS ledger_hashes;
