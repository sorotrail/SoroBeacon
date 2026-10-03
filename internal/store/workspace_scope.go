package store

import (
	"context"
	"errors"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// Every method in this package that reads or writes a tenant-owned table
// (monitors, channels, alerts, rules, saved searches, monitor templates)
// scopes itself to the workspace carried by ctx. Two helpers below define that
// contract; the rest of this file is the rationale for it.
//
// Scoping is a predicate in the query, applied by hand, rather than Postgres
// row-level security. RLS is less code and was rejected for two reasons:
//
//   - The SQLite backend (store.New picks a backend from the DATABASE_URL
//     scheme) has no RLS at all, so the mechanism would protect one deployment
//     and silently not protect the other. One predicate dialect covers both.
//   - RLS takes its subject from a per-transaction `set_config`. Under a pooled
//     connection, a statement that runs in a transaction which did not set it
//     sees no policy filter, so the failure mode of a plumbing mistake is
//     "read every tenant's rows" rather than "read nothing". That is the wrong
//     direction for a bug to fall.
//
// The cost of hand-scoping is that a new method can forget its predicate.
// That is what the two tests over this package are for:
//
//   - The WorkspaceIsolation case in the conformance suite
//     (testWorkspaceIsolation, run against both backends) asserts that no
//     listing, lookup or id-addressed mutation reaches another tenant's row.
//   - TestEveryScopedQueryCarriesWorkspace scans the SQL literals in
//     postgres.go and sqlite.go and fails if one touches a tenant table
//     without scoping it, which catches a method the conformance suite has
//     not been taught about yet.
//
// Resolution defaults rather than errors. A context carrying no workspace
// resolves to workspace.Default, so an instance that never configured
// tenancy — every deployment predating this package, and every test that does
// not care — keeps behaving exactly as it did. The alternative, failing on a
// missing scope, would turn a forgotten With() in a new handler into a 500;
// defaulting turns it into "sees the default workspace", which is a bug but
// not a leak.

// tenantWorkspace returns the workspace a per-tenant query must scope itself
// to, and whether scoping applies to this call at all. ok is false only for a
// context marked with workspace.WithSystem, which is the cross-tenant scope
// instance-level work uses: the retention pruner, the ingest loop and reorg
// handling. Those run outside any request and must see every workspace's rows.
//
// Queries that ignore the second return value are not wrong, they are
// tenant-only: a system-scoped context there reads the default workspace, and
// nothing calls them that way.
func tenantWorkspace(ctx context.Context) (workspace.ID, bool) {
	if workspace.System(ctx) {
		return "", false
	}
	return workspaceID(ctx), true
}

// workspaceID returns the workspace a row is written into. Writes always land
// somewhere — the column is NOT NULL — so a system-scoped context falls back to
// the default workspace rather than producing a row no tenant can read.
func workspaceID(ctx context.Context) workspace.ID {
	if id, ok := workspace.From(ctx); ok {
		return id
	}
	return workspace.Default
}

// What stays shared, since a tenancy boundary is defined as much by what it
// leaves outside as by what it puts inside:
//
//   - Rule types and channel types are code registered in this process, so
//     every tenant evaluates the same rule vocabulary and delivers through the
//     same notifier set. There is no per-workspace plugin path.
//   - Ingest state: the ledger cursor and the recent ledger-hash window
//     describe the chain, not a tenant. One poller walks one chain and feeds
//     every workspace's monitors, so scoping a cursor would mean re-reading
//     the same ledgers once per tenant.
//   - The (rule_id, event_id) dedup key is global, and stays correct because
//     rule ids are unique across the instance — a duplicate can only be
//     produced by the rule that already owns the event.
//   - Alerts retracted by a reorg are retracted for everyone: a orphaned
//     ledger was never canonical for any tenant.
//   - Credentials are instance configuration. WORKSPACE_TOKENS maps a token
//     to a workspace; the mapping itself is not per-workspace data.

// deletableTables is the allowlist for the shared delete-by-id helper in both
// backends. A table name cannot be a bound parameter, so it reaches SQL as Go
// text; constraining it here is what makes that safe for callers inside this
// package, and it is deliberately limited to the tables whose rows a tenant
// owns one-by-one.
//
// rules and delivery_attempts are absent because they cascade from a monitor
// and an alert; workspaces is absent because deleting a tenant is out of this
// issue's scope.
var deletableTables = map[string]bool{
	"monitors":          true,
	"channels":          true,
	"saved_searches":    true,
	"monitor_templates": true,
}

// errUnallowlistedTable reports a delete attempt against a table deleteByID
// does not own. It is a distinct value rather than only a message so the test
// over the allowlist can assert the refusal, and it never quotes the rejected
// name back: a value that arrives here is by definition caller-shaped text.
var errUnallowlistedTable = errors.New("store: refusing to delete from an unallowlisted table")
