CREATE TABLE saved_searches (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL,
    filter     JSONB NOT NULL DEFAULT '{}',
    is_default BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX saved_searches_default_idx ON saved_searches (is_default) WHERE is_default = TRUE;
