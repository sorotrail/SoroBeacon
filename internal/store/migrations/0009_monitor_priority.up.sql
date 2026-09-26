-- Per-monitor poll priority. The poller orders each cycle's watch list so
-- high-priority contracts are fetched first; the column defaults to 'normal'
-- so every monitor created before this migration keeps today's behaviour.
-- The CHECK keeps the vocabulary closed: an unknown value must fail at write
-- time rather than fall through the scheduler's switch to the middle tier.
ALTER TABLE monitors ADD COLUMN priority TEXT NOT NULL DEFAULT 'normal'
    CHECK (priority IN ('low', 'normal', 'high'));

-- The poller lists enabled monitors ordered by id today; a priority index
-- lets a future partial-scheduling pass fetch the tiers it wants without a
-- sort. It is cheap and additive, so it ships with the column.
CREATE INDEX monitors_priority_idx ON monitors (priority);
