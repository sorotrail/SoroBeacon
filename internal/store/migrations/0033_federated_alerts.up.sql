-- Federated alerts: an alert another SoroBeacon instance forwarded.
--
-- The sender's monitor and rule ids belong to the sending instance's
-- database. Writing them into the receiving instance's foreign keys either
-- fails (the row does not exist here) or attaches the alert to an unrelated
-- local monitor (it does), so the columns become nullable and a federated
-- alert carries its origin and the sender's ids in columns of its own.
--
-- The dedup guard moves with it. A federated alert has no local rule, so its
-- key is the origin plus the event id: the same event forwarded twice by the
-- same peer is one alert, and the same event id from two peers is two. The
-- rule-keyed index covers only the rows that have a rule.
--
-- The workspace column is unchanged and still answers the tenancy question:
-- a federated alert lands in the workspace of the credential that delivered
-- it, never in one inferred from the payload.

ALTER TABLE alerts ALTER COLUMN monitor_id DROP NOT NULL;
ALTER TABLE alerts ALTER COLUMN rule_id DROP NOT NULL;

ALTER TABLE alerts ADD COLUMN federated BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE alerts ADD COLUMN federation_origin TEXT;
ALTER TABLE alerts ADD COLUMN source_monitor_id BIGINT;
ALTER TABLE alerts ADD COLUMN source_rule_id BIGINT;

-- The primary key was (rule_id, event_id), and a rule id cannot be part of a
-- primary key while it is nullable, so the key becomes two partial unique
-- indexes: one for the alerts that have a local rule, one for the federated
-- ones.
ALTER TABLE alert_dedup DROP CONSTRAINT alert_dedup_pkey;
ALTER TABLE alert_dedup ALTER COLUMN rule_id DROP NOT NULL;
ALTER TABLE alert_dedup ADD COLUMN federation_origin TEXT;

CREATE UNIQUE INDEX alert_dedup_rule_event_uidx
    ON alert_dedup (rule_id, event_id) WHERE rule_id IS NOT NULL;
CREATE UNIQUE INDEX alert_dedup_origin_event_uidx
    ON alert_dedup (federation_origin, event_id) WHERE federation_origin IS NOT NULL;
