-- Persistent record of a checkpoint that fell outside the source's
-- event retention window.
--
-- When SoroBeacon is down longer than the RPC node's retention window
-- (roughly 1-7 days), the checkpoint in ingest_state points at ledgers the
-- RPC has forgotten: getEvents errors or silently returns from
-- oldestLedger, and the events in between are unrecoverably missed. The
-- poller detects this on resume, jumps to the oldest retained ledger, and
-- writes the hole here so the data loss stays visible after a restart
-- instead of living only in a log line that has since rotated away.
--
-- No workspace column: like ingest_state and network_ingest_state, a gap is
-- an instance-level fact about one chain's ingest position, not any
-- tenant's data.
CREATE TABLE retention_gaps (
    id          BIGSERIAL PRIMARY KEY,
    network     TEXT        NOT NULL DEFAULT '',
    from_ledger BIGINT      NOT NULL,
    to_ledger   BIGINT      NOT NULL,
    detected_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (to_ledger >= from_ledger)
);

-- The dashboard and GET /stats ask for the most recent gap, per network or
-- across the instance; the index answers both in index order.
CREATE INDEX retention_gaps_network_detected_idx ON retention_gaps (network, detected_at DESC);
