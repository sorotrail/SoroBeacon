-- Push device registry (mobile push channel) and provider incident
-- correlation (paging acknowledgement sync).
--
-- push_devices holds only what delivery needs: an opaque caller-chosen device
-- id and the platform token. The token is a credential and is never returned
-- by the API. A retired token stays as a tombstone row until the device
-- re-registers or is deleted, but it is never sent to again.
--
-- alert_external_refs records which provider incident an alert opened, so
-- correlation is a lookup on a column rather than text matching.
CREATE TABLE push_devices (
    id           BIGSERIAL PRIMARY KEY,
    channel_id   BIGINT NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL DEFAULT 'default',
    device_id    TEXT NOT NULL,
    platform     TEXT NOT NULL DEFAULT 'fcm',
    token        TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    retired_at   TIMESTAMPTZ
);
CREATE UNIQUE INDEX push_devices_channel_device_idx ON push_devices (channel_id, device_id);

-- alerts is partitioned, so the reference carries the parent's created_at and
-- points at (id, created_at), as delivery_attempts and dead_letters do.
CREATE TABLE alert_external_refs (
    alert_id         BIGINT NOT NULL,
    alert_created_at TIMESTAMPTZ NOT NULL,
    channel_id   BIGINT NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL DEFAULT 'default',
    provider     TEXT NOT NULL,
    dedup_key    TEXT NOT NULL,
    incident_id  TEXT NOT NULL DEFAULT '',
    state        TEXT NOT NULL DEFAULT '',
    sync_error   TEXT NOT NULL DEFAULT '',
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (alert_id, channel_id),
    FOREIGN KEY (alert_id, alert_created_at) REFERENCES alerts (id, created_at) ON DELETE CASCADE
);
CREATE INDEX alert_external_refs_dedup_idx ON alert_external_refs (channel_id, dedup_key);
