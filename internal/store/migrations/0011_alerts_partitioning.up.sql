-- Convert alerts to a range-partitioned table by created_at.
--
-- Design decisions (documented here because a migration is the only place the
-- schema rationale lives with the change):
--
--   * Partition span: monthly. Alert volume is bursty, so monthly gives a
--     bounded partition count (~12/year) while a month of drop granularity is
--     finer than any retention window measured in weeks. Daily would mean
--     hundreds of partitions to plan for; yearly would make the ragged edge
--     huge.
--   * default partition: created as the safety net, so a row whose monthly
--     partition does not exist yet is stored rather than rejected. Partition
--     creation moves such rows explicitly (see EnsureAlertPartitions).
--   * Dedup: a partitioned table cannot carry a global unique index that
--     omits the partition key, so the (rule_id, event_id) guard moves to a
--     small, unpartitioned `alert_dedup` table.
--   * delivery_attempts: a foreign key must reference a unique constraint,
--     and the only unique key on the partitioned table is (id, created_at),
--     so the FK becomes composite and delivery_attempts carries the parent's
--     created_at. ON DELETE CASCADE still fires on row deletes.
--
-- Cost on an existing large table: this is an in-place conversion. It reads
-- and rewrites the whole alerts table once (INSERT ... SELECT) and takes an
-- ACCESS EXCLUSIVE lock for the duration, so on a table with millions of rows
-- schedule it in a maintenance window. The alternative — an online conversion
-- with a shadow table and triggers — was rejected as far more moving parts
-- than a monitoring tool's alert history warrants.

-- 1. Detach the alert table from the FK graph and take ownership of its
--    sequence, so the replacement can reuse both.
ALTER TABLE delivery_attempts DROP CONSTRAINT delivery_attempts_alert_id_fkey;
ALTER SEQUENCE alerts_id_seq OWNED BY NONE;

-- 2. The dedup guard moves to its own table before the unique index disappears
--    with the rename. alert_created_at rides along so retention can drop old
--    dedup rows by time.
CREATE TABLE alert_dedup (
    rule_id          BIGINT      NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    event_id         TEXT        NOT NULL,
    alert_id         BIGINT,
    alert_created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (rule_id, event_id)
);

CREATE INDEX alert_dedup_created_idx ON alert_dedup (alert_created_at);

ALTER TABLE alerts RENAME TO alerts_unpartitioned;

ALTER INDEX alerts_rule_event_uidx RENAME TO alerts_unpart_rule_event_uidx;
ALTER INDEX alerts_monitor_created_idx RENAME TO alerts_unpart_monitor_created_idx;
ALTER INDEX alerts_rule_created_idx RENAME TO alerts_unpart_rule_created_idx;
ALTER INDEX alerts_contract_created_idx RENAME TO alerts_unpart_contract_created_idx;
ALTER INDEX alerts_monitor_rule_created_idx RENAME TO alerts_unpart_monitor_rule_created_idx;
ALTER INDEX alerts_created_at_idx RENAME TO alerts_unpart_created_at_idx;
ALTER INDEX alerts_ledger_idx RENAME TO alerts_unpart_ledger_idx;

INSERT INTO alert_dedup (rule_id, event_id, alert_id, alert_created_at)
    SELECT rule_id, event_id, id, created_at FROM alerts_unpartitioned;

-- 3. The partitioned replacement. The primary key must contain the partition
--    key, which is why it is (id, created_at) and not id alone.
CREATE TABLE alerts (
    id           BIGINT      NOT NULL DEFAULT nextval('alerts_id_seq'),
    monitor_id   BIGINT      NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    rule_id      BIGINT      NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    event_id     TEXT        NOT NULL,
    payload      JSONB       NOT NULL DEFAULT '{}',
    ledger       BIGINT      NOT NULL DEFAULT 0,
    retracted_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

ALTER SEQUENCE alerts_id_seq OWNED BY alerts.id;

-- The safety net: a row bound for a month with no partition is stored here
-- instead of failing. EnsureAlertPartitions moves rows out of it when it
-- creates the real partition.
CREATE TABLE alerts_default PARTITION OF alerts DEFAULT;

-- 4. Create a partition for every month that holds existing data, then copy.
--    Creating these before the copy keeps rows out of the default partition,
--    which is what lets the default stay a pure safety net.
DO $$
DECLARE
    lo timestamptz;
    hi timestamptz;
    cur date;
    stop date;
BEGIN
    SELECT min(created_at), max(created_at) INTO lo, hi FROM alerts_unpartitioned;
    IF lo IS NULL THEN
        lo := now();
        hi := now();
    END IF;
    cur := date_trunc('month', lo)::date;
    stop := date_trunc('month', hi)::date;
    WHILE cur <= stop LOOP
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS %I PARTITION OF alerts FOR VALUES FROM (%L) TO (%L)',
            'alerts_' || to_char(cur, 'YYYY_MM'), cur, (cur + interval '1 month')::date);
        cur := (cur + interval '1 month')::date;
    END LOOP;
END $$;

INSERT INTO alerts (id, monitor_id, rule_id, event_id, payload, ledger, retracted_at, created_at)
    SELECT id, monitor_id, rule_id, event_id, payload, ledger, retracted_at, created_at
      FROM alerts_unpartitioned;

SELECT setval('alerts_id_seq', GREATEST((SELECT COALESCE(max(id), 1) FROM alerts), 1));

DROP TABLE alerts_unpartitioned;

-- 5. Recreate the query indexes on the partitioned parent.
CREATE INDEX alerts_monitor_created_idx ON alerts (monitor_id, created_at DESC, id DESC);
CREATE INDEX alerts_rule_created_idx ON alerts (rule_id, created_at DESC, id DESC);
CREATE INDEX alerts_contract_created_idx ON alerts ((payload->>'contract_id'), created_at DESC, id DESC);
CREATE INDEX alerts_monitor_rule_created_idx ON alerts (monitor_id, rule_id, created_at DESC, id DESC);
CREATE INDEX alerts_created_at_idx ON alerts (created_at);
CREATE INDEX alerts_ledger_idx ON alerts (ledger);

-- 6. delivery_attempts now references (id, created_at) so the cascade survives
--    partitioning, and carries the parent's created_at to do it.
ALTER TABLE delivery_attempts ADD COLUMN alert_created_at TIMESTAMPTZ;
UPDATE delivery_attempts da
   SET alert_created_at = a.created_at
  FROM alerts a
 WHERE a.id = da.alert_id;
UPDATE delivery_attempts SET alert_created_at = now() WHERE alert_created_at IS NULL;
ALTER TABLE delivery_attempts ALTER COLUMN alert_created_at SET NOT NULL;
ALTER TABLE delivery_attempts ALTER COLUMN alert_created_at SET DEFAULT now();
ALTER TABLE delivery_attempts
    ADD CONSTRAINT delivery_attempts_alert_fkey
    FOREIGN KEY (alert_id, alert_created_at) REFERENCES alerts (id, created_at) ON DELETE CASCADE;

-- 7. A year of partitions ahead so a running instance never lands a row in the
--    default partition under normal operation. EnsureAlertPartitions extends
--    this window on every start and prune pass.
DO $$
DECLARE
    cur date := date_trunc('month', now())::date;
    stop date := (date_trunc('month', now()) + interval '12 months')::date;
BEGIN
    WHILE cur <= stop LOOP
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS %I PARTITION OF alerts FOR VALUES FROM (%L) TO (%L)',
            'alerts_' || to_char(cur, 'YYYY_MM'), cur, (cur + interval '1 month')::date);
        cur := (cur + interval '1 month')::date;
    END LOOP;
END $$;
