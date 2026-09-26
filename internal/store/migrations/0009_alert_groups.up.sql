-- Alert group state for deduplication and grouping within a time window.
-- group_key is (monitor_id:rule_id:contract_id), window_start is the
-- fixed tumbling window start stamped from the first event. The primary
-- key (group_key, window_start) lets a new window close cleanly after
-- the summary is delivered and a fresh group is created.
CREATE TABLE alert_groups (
  group_key TEXT NOT NULL,
  window_start TIMESTAMPTZ NOT NULL,
  count INTEGER NOT NULL DEFAULT 1,
  first_alert_id BIGINT,
  PRIMARY KEY (group_key, window_start)
);
