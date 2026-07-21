CREATE TABLE monitors (
    id           BIGSERIAL PRIMARY KEY,
    name         TEXT        NOT NULL,
    contract_ids JSONB       NOT NULL DEFAULT '[]',
    enabled      BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE rules (
    id         BIGSERIAL PRIMARY KEY,
    monitor_id BIGINT  NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    type       TEXT    NOT NULL,
    params     JSONB   NOT NULL DEFAULT '{}',
    enabled    BOOLEAN NOT NULL DEFAULT TRUE
);

CREATE INDEX rules_monitor_id_idx ON rules (monitor_id);

-- config holds channel secrets (webhook URLs, tokens, SMTP credentials).
-- TODO(contributors): encrypt this column at rest.
CREATE TABLE channels (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT        NOT NULL,
    type       TEXT        NOT NULL,
    config     JSONB       NOT NULL DEFAULT '{}',
    enabled    BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE monitor_channels (
    monitor_id BIGINT NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    channel_id BIGINT NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    PRIMARY KEY (monitor_id, channel_id)
);

CREATE TABLE alerts (
    id         BIGSERIAL PRIMARY KEY,
    monitor_id BIGINT      NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    rule_id    BIGINT      NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    event_id   TEXT        NOT NULL,
    payload    JSONB       NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Dedup guard: the same rule can only ever fire once per source event.
CREATE UNIQUE INDEX alerts_rule_event_uidx ON alerts (rule_id, event_id);
CREATE INDEX alerts_monitor_created_idx ON alerts (monitor_id, created_at DESC);

CREATE TABLE delivery_attempts (
    id               BIGSERIAL PRIMARY KEY,
    alert_id         BIGINT      NOT NULL REFERENCES alerts (id) ON DELETE CASCADE,
    channel_id       BIGINT      NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    status           TEXT        NOT NULL,
    response_snippet TEXT        NOT NULL DEFAULT '',
    attempted_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX delivery_attempts_alert_idx ON delivery_attempts (alert_id);

-- Single-row poller checkpoint.
CREATE TABLE ingest_state (
    id          INTEGER     PRIMARY KEY CHECK (id = 1),
    last_ledger BIGINT      NOT NULL DEFAULT 0,
    last_cursor TEXT        NOT NULL DEFAULT '',
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO ingest_state (id) VALUES (1);
