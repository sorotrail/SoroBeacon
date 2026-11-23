-- Bounded archive of every ingested contract event, so a rule can be
-- replayed against the real event corpus instead of the biased set of
-- events that already matched some rule (see internal/replay).
--
-- Design decisions:
--   * One row per (network, event_id): the poller re-reads a ledger window
--     after a crash or a confirmation-depth hold, and the unique key turns
--     that re-read into a no-op instead of duplicating the corpus.
--   * Topics/value/fields are JSONB: rule evaluators operate on decoded
--     values, so archiving the decoded shape lets replay feed the same
--     evaluators directly instead of re-decoding XDR.
--   * Retention is bounded (default 30 days, EVENT_ARCHIVE_RETENTION): the
--     pruner deletes expired rows in batches, so the table stays a replay
--     window rather than a second copy of the chain.
--
-- No workspace column: like ingest_state, the archive mirrors chain data
-- the ingest pipeline observed, not any tenant's configuration. Replay is
-- scoped by the monitor's own contract ids.
CREATE TABLE event_archive (
    id               BIGSERIAL PRIMARY KEY,
    network          TEXT        NOT NULL DEFAULT '',
    event_id         TEXT        NOT NULL,
    contract_id      TEXT        NOT NULL,
    ledger           BIGINT      NOT NULL,
    ledger_closed_at TIMESTAMPTZ,
    tx_hash          TEXT        NOT NULL DEFAULT '',
    event_name       TEXT        NOT NULL DEFAULT '',
    topics           JSONB       NOT NULL DEFAULT '[]',
    value            JSONB,
    fields           JSONB,
    ingested_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (network, event_id)
);

-- Replay asks for one contract's events in a ledger range; this is the
-- index that answers it without scanning the archive.
CREATE INDEX event_archive_contract_ledger_idx ON event_archive (contract_id, ledger);

-- The pruner cuts by age.
CREATE INDEX event_archive_ingested_idx ON event_archive (ingested_at);
