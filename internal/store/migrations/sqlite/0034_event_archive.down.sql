-- Revert the event archive (SQLite version).
DROP INDEX IF EXISTS event_archive_ingested_idx;
DROP INDEX IF EXISTS event_archive_contract_ledger_idx;
DROP TABLE IF EXISTS event_archive;
