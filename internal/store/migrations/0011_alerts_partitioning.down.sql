-- Reverse of 0011: fold the partitions back into a single plain table. Like
-- the up migration this is an in-place rewrite and takes an exclusive lock.
ALTER TABLE delivery_attempts DROP CONSTRAINT IF EXISTS delivery_attempts_alert_fkey;
ALTER TABLE delivery_attempts DROP COLUMN IF EXISTS alert_created_at;

ALTER TABLE alerts RENAME TO alerts_partitioned;

CREATE TABLE alerts (
    id           BIGINT      NOT NULL DEFAULT nextval('alerts_id_seq'),
    monitor_id   BIGINT      NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    rule_id      BIGINT      NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    event_id     TEXT        NOT NULL,
    payload      JSONB       NOT NULL DEFAULT '{}',
    ledger       BIGINT      NOT NULL DEFAULT 0,
    retracted_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id)
);

ALTER SEQUENCE alerts_id_seq OWNED BY alerts.id;

INSERT INTO alerts (id, monitor_id, rule_id, event_id, payload, ledger, retracted_at, created_at)
    SELECT id, monitor_id, rule_id, event_id, payload, ledger, retracted_at, created_at
      FROM alerts_partitioned;

SELECT setval('alerts_id_seq', GREATEST((SELECT COALESCE(max(id), 1) FROM alerts), 1));

DROP TABLE alerts_partitioned;

DROP TABLE IF EXISTS alert_dedup;

-- Restore the pre-partitioning indexes and the single-column FK.
CREATE UNIQUE INDEX alerts_rule_event_uidx ON alerts (rule_id, event_id);
CREATE INDEX alerts_monitor_created_idx ON alerts (monitor_id, created_at DESC);
CREATE INDEX alerts_monitor_rule_created_idx ON alerts (monitor_id, rule_id, created_at DESC, id DESC);
CREATE INDEX alerts_rule_created_idx ON alerts (rule_id, created_at DESC, id DESC);
CREATE INDEX alerts_contract_created_idx ON alerts ((payload->>'contract_id'), created_at DESC, id DESC);
CREATE INDEX alerts_created_at_idx ON alerts (created_at);
CREATE INDEX alerts_ledger_idx ON alerts (ledger);

ALTER TABLE delivery_attempts
    ADD CONSTRAINT delivery_attempts_alert_id_fkey
    FOREIGN KEY (alert_id) REFERENCES alerts (id) ON DELETE CASCADE;
