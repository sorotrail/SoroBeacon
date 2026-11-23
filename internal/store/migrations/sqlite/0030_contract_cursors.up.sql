-- Keep the Postgres cursor semantics; SQLite stores timestamps as UTC text.
CREATE TABLE contract_ingest_cursors (
	network     TEXT    NOT NULL,
	contract_id TEXT    NOT NULL,
	last_ledger INTEGER NOT NULL DEFAULT 0,
	updated_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
	PRIMARY KEY (network, contract_id)
);
