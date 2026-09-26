-- Inhibition rules: while the source rule is firing, deliveries for the
-- target rule are suppressed (the alert rows are still stored). Cross-monitor
-- pairs are allowed: one incident usually trips several monitors at once and
-- the root-cause page is what the operator wants.
-- firing_window_seconds is per-pair and configurable; 300 keeps a sustained
-- incident quiet without hiding a genuinely new episode the next day.
-- inhibited_by_rule_id on alerts records which source suppressed a delivery
-- so the dashboard can show why nothing was sent.
CREATE TABLE alert_inhibitions (
    source_rule_id       BIGINT  NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    target_rule_id       BIGINT  NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    firing_window_seconds INTEGER NOT NULL DEFAULT 300,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (source_rule_id, target_rule_id)
);
CREATE INDEX alert_inhibitions_target_idx ON alert_inhibitions (target_rule_id);

ALTER TABLE alerts ADD COLUMN inhibited_by_rule_id BIGINT REFERENCES rules (id) ON DELETE SET NULL;
CREATE INDEX alerts_inhibited_by_idx ON alerts (inhibited_by_rule_id) WHERE inhibited_by_rule_id IS NOT NULL;
