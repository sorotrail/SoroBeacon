-- Workspaces: the tenancy boundary every monitor, channel, alert, saved
-- search and template belongs to.
--
-- Design decisions (recorded here because the schema is where the rationale
-- has to live next to the change):
--
--   * Scoping in SQL, not row-level security. RLS was the rejected
--     alternative: it is Postgres-only, and this codebase ships an equally
--     supported SQLite backend (store.New selects the backend from the
--     DATABASE_URL scheme), so RLS would leave half the deployments
--     unprotected. It also needs a per-transaction `set_config`, which under
--     a pooled connection is exactly the "subtly wrong" failure the scope has
--     to avoid — one code path that runs outside the transaction that set it
--     reads every tenant's rows. Explicit predicates apply to both backends
--     and are checked by the conformance suite running the whole store
--     interface once per workspace.
--
--   * Existing data. Every column is NOT NULL DEFAULT 'default', and the
--     'default' row is inserted here, so the upgrade is transparent: rows
--     written before this migration belong to 'default', and a single-tenant
--     instance keeps reading and writing exactly what it did before.
--
--   * rules and delivery_attempts carry no workspace column. Both are
--     reachable only through a row that does (rules.monitor_id,
--     delivery_attempts.alert_id), so scoping them by join keeps one copy of
--     the truth instead of a derived column that can disagree with its parent.
--
--   * No foreign key from the tenant columns to workspaces. Deleting a
--     workspace is a separate, deliberate operation (this issue's scope is
--     isolation, not lifecycle); an FK would turn a missing row into a write
--     failure on the hot ingest path, and the ids that reach SQL are already
--     constrained to a narrow grammar by workspace.Parse.

CREATE TABLE workspaces (
    id         TEXT        PRIMARY KEY,
    name       TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The implicit workspace every pre-existing row lands in. The id is a literal
-- so the column defaults above and Go-side resolution agree on it.
INSERT INTO workspaces (id, name) VALUES ('default', 'Default');

ALTER TABLE monitors          ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE channels          ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE alerts            ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE saved_searches    ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE monitor_templates ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';

-- "At most one default search" was a global rule, enforced by 0007's partial
-- unique index on is_default. Under tenancy it is a per-workspace rule, and the
-- old index would make the second workspace's first default search a unique
-- violation. Recreated over workspace_id, so the invariant is one index again
-- rather than the application's best effort.
DROP INDEX saved_searches_default_idx;
CREATE UNIQUE INDEX saved_searches_default_idx ON saved_searches (workspace_id) WHERE is_default = TRUE;

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
