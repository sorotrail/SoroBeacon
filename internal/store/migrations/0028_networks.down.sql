DROP INDEX IF EXISTS network_ledger_hashes_observed_idx;
DROP TABLE IF EXISTS network_ledger_hashes;
DROP TABLE IF EXISTS network_ingest_state;
DROP INDEX IF EXISTS alerts_network_created_idx;
DROP INDEX IF EXISTS monitors_network_idx;
ALTER TABLE alerts   DROP COLUMN IF EXISTS network;
ALTER TABLE monitors DROP COLUMN IF EXISTS network;
