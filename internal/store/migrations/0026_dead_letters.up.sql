-- A delivery that exhausted its retries, kept so an operator can see what was
-- lost and redrive it once the destination is fixed. Without this the only
-- record of a permanently failed delivery was a delivery_attempts row that
-- nothing read.
--
-- alerts is partitioned by created_at (0011), so its primary key is
-- (id, created_at) and a foreign key on id alone has no unique constraint to
-- point at. alert_created_at rides along and the reference is composite, the
-- same shape delivery_attempts took in 0011, which is what keeps the cascade.
CREATE TABLE dead_letters (
    id               BIGSERIAL   PRIMARY KEY,
    alert_id         BIGINT      NOT NULL,
    alert_created_at TIMESTAMPTZ NOT NULL,
    channel_id       BIGINT      NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    last_error       TEXT        NOT NULL DEFAULT '',
    attempt_count    INT         NOT NULL,
    last_status      INT         NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (alert_id, alert_created_at) REFERENCES alerts (id, created_at) ON DELETE CASCADE
);

CREATE INDEX dead_letters_channel_idx ON dead_letters (channel_id);
CREATE INDEX dead_letters_alert_idx ON dead_letters (alert_id);
