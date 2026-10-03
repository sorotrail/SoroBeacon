-- Multi-network ingestion: one instance polls several Stellar networks at once,
-- each with its own monitors, checkpoint and reorg window.
--
-- Design decisions (recorded where the schema keeps them next to the change):
--
--   * `network` is a column on monitors and alerts, not a per-instance setting.
--     A monitor watches contracts whose ids are only meaningful on one chain,
--     so a monitor cannot span networks; an alert inherits its monitor's
--     network and carries it as a column so the dashboard filter and the reorg
--     retraction are both plain indexed comparisons rather than a join.
--
--   * Empty string means "the network this row was written before multi-network
--     existed". The default is deliberately '' rather than a network name: a
--     migration cannot know which network an operator configured, and guessing
--     'testnet' would silently mislabel a mainnet deployment's entire history.
--     cmd/sorobeacon assigns the configured primary network to those rows once,
--     at startup, before any poller runs.
--
--   * Per-network checkpoint and ledger-hash window live in NEW tables, and
--     `ingest_state`/`ledger_hashes` keep serving the '' network they already
--     hold. Relaxing a primary key in place would have meant rebuilding
--     `ingest_state` on SQLite and dropping a CHECK constraint on Postgres to
--     change one column's role; a second table costs a query and leaves the
--     existing hot path's schema untouched.
--
--   * The reorg window is per network because two chains number their ledgers
--     independently. Sharing one table would make mainnet ledger 100 overwrite
--     testnet ledger 100 and report a reorganisation on every cycle — a false
--     positive that retracts real alerts. The legacy window is not carried over
--     on purpose: a missing stored hash cannot be read as a divergence (see
--     detectReorg's `seen` check), so the first cycle after upgrading simply
--     re-establishes the window instead of retracting anything.

ALTER TABLE monitors ADD COLUMN network TEXT NOT NULL DEFAULT '';
ALTER TABLE alerts   ADD COLUMN network TEXT NOT NULL DEFAULT '';

-- Both listings filter by network, so without these the per-network views are
-- a scan of the whole table. The orderings match the listings' ORDER BY clauses.
CREATE INDEX monitors_network_idx ON monitors (network, id DESC);
CREATE INDEX alerts_network_created_idx ON alerts (network, created_at DESC, id DESC);

-- One poller checkpoint per network. network = '' is the pre-multi-network
-- row's value and stays in ingest_state; the first read for a named network
-- copies that row forward once, so an upgrade neither rewinds nor skips.
CREATE TABLE network_ingest_state (
    network     TEXT        PRIMARY KEY,
    last_ledger BIGINT      NOT NULL DEFAULT 0,
    last_cursor TEXT        NOT NULL DEFAULT '',
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE network_ledger_hashes (
    network     TEXT        NOT NULL,
    ledger      BIGINT      NOT NULL,
    hash        TEXT        NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (network, ledger)
);

CREATE INDEX network_ledger_hashes_observed_idx ON network_ledger_hashes (observed_at);
