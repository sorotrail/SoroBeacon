-- One inclusive, fully processed ledger per watched contract and network.
-- Event ordering within a ledger is deliberately not stored: replaying the
-- whole ledger after a crash is safe because alert writes are idempotent.
CREATE TABLE contract_ingest_cursors (
	network     TEXT        NOT NULL,
	contract_id TEXT        NOT NULL,
	last_ledger BIGINT      NOT NULL DEFAULT 0,
	updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (network, contract_id)
);
