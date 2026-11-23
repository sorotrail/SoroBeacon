-- Last-seen clock for absence_of_event rules ("alert me when my contract goes
-- quiet"). One row per (rule, awaited event pattern), because the same rule
-- cannot express two patterns but an edited rule can move from one to another
-- without the old clock being read for the new pattern.
--
-- The row is what makes silence measurable at all: what these rules watch for
-- is precisely the thing that produces no event to evaluate, so the sweep
-- compares now() against last_seen_at instead. It lives in the database rather
-- than in memory so a process bounce cannot reset the clock and swallow a real
-- outage, and so the alert window id (derived from last_seen_at) is stable
-- across restarts — which is what keeps the (rule_id, event_id) dedup guard
-- working after a restart instead of re-alerting the same silence.
CREATE TABLE rule_absence_state (
    rule_id      BIGINT      NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    event_name   TEXT        NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (rule_id, event_name)
);
