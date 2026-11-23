-- Reverse of 0033_federated_alerts.
--
-- The federated rows cannot be represented by the old schema: an alert with
-- no local monitor and no local rule has nowhere to point. They are deleted
-- rather than kept in a shape the old columns cannot hold, which is the one
-- thing a down migration here has to give up.

DELETE FROM alerts WHERE monitor_id IS NULL OR rule_id IS NULL;

DELETE FROM alert_dedup WHERE rule_id IS NULL;

DROP INDEX alert_dedup_origin_event_uidx;
DROP INDEX alert_dedup_rule_event_uidx;

ALTER TABLE alert_dedup DROP COLUMN federation_origin;
ALTER TABLE alert_dedup ALTER COLUMN rule_id SET NOT NULL;
ALTER TABLE alert_dedup ADD CONSTRAINT alert_dedup_pkey PRIMARY KEY (rule_id, event_id);

ALTER TABLE alerts DROP COLUMN source_rule_id;
ALTER TABLE alerts DROP COLUMN source_monitor_id;
ALTER TABLE alerts DROP COLUMN federation_origin;
ALTER TABLE alerts DROP COLUMN federated;

ALTER TABLE alerts ALTER COLUMN rule_id SET NOT NULL;
ALTER TABLE alerts ALTER COLUMN monitor_id SET NOT NULL;
