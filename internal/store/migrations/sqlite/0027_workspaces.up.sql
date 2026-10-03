-- SQLite equivalent of the Postgres 0012_workspaces. Same tenancy model and
-- the same reasons behind it (see the Postgres file); the differences are the
-- dialect's: TEXT ids, no TIMESTAMPTZ, and expression indexes written the way
-- SQLite accepts them.
--
-- Row-level security is not merely undesirable here, it does not exist, which
-- is the sharpest argument for scoping in SQL: one mechanism, both backends.

CREATE TABLE workspaces (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- The implicit workspace every pre-existing row lands in, so an upgrade changes
-- what a single-tenant instance can see by nothing at all.
INSERT INTO workspaces (id, name) VALUES ('default', 'Default');

ALTER TABLE monitors          ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE channels          ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE alerts            ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE saved_searches    ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE monitor_templates ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';

-- "At most one default search" was a global rule, enforced by 0007's partial
-- unique index on is_default. Under tenancy it is a per-workspace rule, and the
-- old index would make the second workspace's first default search a unique
-- violation. Recreated over workspace_id, so the invariant stays an invariant
-- rather than becoming the application's best effort.
DROP INDEX saved_searches_default_idx;
CREATE UNIQUE INDEX saved_searches_default_idx ON saved_searches (workspace_id) WHERE is_default = 1;

-- Leading workspace column: every listing now filters by it, so the pre-existing
-- indexes are at best a scan of the whole table. The orderings mirror the
-- listings' ORDER BY clauses (id DESC for monitors and channels, created DESC
-- for alerts, name for the two catalogues).
CREATE INDEX monitors_workspace_idx          ON monitors (workspace_id, id DESC);
CREATE INDEX monitors_workspace_name_idx     ON monitors (workspace_id, lower(name), id);
CREATE INDEX channels_workspace_idx          ON channels (workspace_id, id DESC);
CREATE INDEX alerts_workspace_created_idx    ON alerts (workspace_id, created_at DESC, id DESC);
CREATE INDEX saved_searches_workspace_idx    ON saved_searches (workspace_id, name);
CREATE INDEX monitor_templates_workspace_idx ON monitor_templates (workspace_id, name);
