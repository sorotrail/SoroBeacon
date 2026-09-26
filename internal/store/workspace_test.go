package store

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// testWorkspaceIsolation is the tenancy half of the conformance suite, so it
// runs unchanged against Postgres and SQLite. It answers the question the
// whole feature turns on: can one workspace see another's rows?
//
// Every cross-tenant failure below is asserted as ErrNotFound rather than a
// permission error on purpose. A tenant probing for the existence of someone
// else's monitor id must get the answer it would get for an id that was never
// issued.
func testWorkspaceIsolation(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	bg := context.Background()
	// Two named tenants, plus the context-without-a-workspace that a
	// single-tenant deployment produces — which resolves to the default
	// workspace, the tenant every pre-existing row already belongs to.
	acme := workspace.With(bg, "acme")
	beta := workspace.With(bg, "beta")
	system := workspace.WithSystem(bg)

	// --- monitors ---

	acmeMonitor := &Monitor{Name: "payments", ContractIDs: []string{"CAACME"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(acme, acmeMonitor))

	got, err := st.GetMonitor(acme, acmeMonitor.ID)
	require.NoError(t, err)
	assert.Equal(t, "payments", got.Name)

	for name, other := range map[string]context.Context{
		"another workspace": beta,
		"the default one":   bg,
	} {
		_, err := st.GetMonitor(other, acmeMonitor.ID)
		assert.ErrorIs(t, err, ErrNotFound, "GetMonitor must not reach across from %s", name)
	}

	listAcme, err := st.ListMonitors(acme, false)
	require.NoError(t, err)
	require.Len(t, listAcme, 1)
	listBeta, err := st.ListMonitors(beta, false)
	require.NoError(t, err)
	assert.Empty(t, listBeta)
	listDefault, err := st.ListMonitors(bg, false)
	require.NoError(t, err)
	assert.Empty(t, listDefault, "an unconfigured instance must see only the default workspace")

	// The cross-tenant listings a dashboard performs are the paged ones, so
	// they are checked separately from ListMonitors.
	pageBeta, err := st.ListMonitorsPage(beta, ListFilter{Limit: 50})
	require.NoError(t, err)
	assert.Empty(t, pageBeta, "a keyset page must not leak another workspace's monitors")
	pageAcme, err := st.ListMonitorsPage(acme, ListFilter{Limit: 50})
	require.NoError(t, err)
	require.Len(t, pageAcme, 1)

	// Names are unique per workspace, not per instance: two teams may both
	// call their monitor "payments".
	betaMonitor := &Monitor{Name: "payments", ContractIDs: []string{"CBETAB"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(beta, betaMonitor), "the name collision check must be scoped")

	// Writes cannot cross either, and a cross-tenant write reports exactly what
	// a write to a missing row reports.
	acmeMonitor.Name = "renamed"
	require.ErrorIs(t, st.UpdateMonitor(beta, acmeMonitor), ErrNotFound)
	unchanged, err := st.GetMonitor(acme, acmeMonitor.ID)
	require.NoError(t, err)
	assert.Equal(t, "payments", unchanged.Name, "a rejected cross-tenant update must change nothing")

	require.ErrorIs(t, st.DeleteMonitor(beta, acmeMonitor.ID), ErrNotFound)
	_, err = st.GetMonitor(acme, acmeMonitor.ID)
	require.NoError(t, err, "the monitor must survive a delete attempted by another tenant")

	updated, unknown, err := st.SetMonitorsEnabled(beta, []int64{acmeMonitor.ID}, false)
	require.NoError(t, err)
	assert.Equal(t, 0, updated)
	assert.Equal(t, []int64{acmeMonitor.ID}, unknown, "another tenant's monitor must look like an id that does not exist")

	_, err = st.DuplicateMonitor(beta, acmeMonitor.ID)
	assert.ErrorIs(t, err, ErrNotFound)

	// --- rules ---

	rule := &Rule{MonitorID: acmeMonitor.ID, Type: "event_emitted", Params: json.RawMessage(`{"event_name":"transfer"}`), Enabled: true}
	require.ErrorIs(t, st.CreateRule(beta, rule), ErrNotFound, "a rule cannot be attached to a monitor the caller cannot see")
	require.NoError(t, st.CreateRule(acme, rule))

	rulesAcme, err := st.ListRules(acme, acmeMonitor.ID, false)
	require.NoError(t, err)
	assert.Len(t, rulesAcme, 1)
	rulesBeta, err := st.ListRules(beta, acmeMonitor.ID, false)
	require.NoError(t, err)
	assert.Empty(t, rulesBeta)
	// The poller reads rules for a monitor it found in an earlier step, under a
	// cross-tenant context: that lookup has to keep working.
	rulesSystem, err := st.ListRules(system, acmeMonitor.ID, false)
	require.NoError(t, err)
	assert.Len(t, rulesSystem, 1)

	_, err = st.GetRule(beta, rule.ID)
	assert.ErrorIs(t, err, ErrNotFound, "rules reach the workspace through their monitor, so a foreign monitor hides the rule")

	// --- channels ---

	acmeChannel := &Channel{Name: "ops-slack", Type: "slack", Config: json.RawMessage(`{"webhook_url":"https://example.invalid/hook"}`), Enabled: true}
	require.NoError(t, st.CreateChannel(acme, acmeChannel))
	betaChannel := &Channel{Name: "ops-slack", Type: "slack", Config: json.RawMessage(`{"webhook_url":"https://example.invalid/other"}`), Enabled: true}
	require.NoError(t, st.CreateChannel(beta, betaChannel), "the same channel name in two workspaces")

	_, err = st.GetChannel(beta, acmeChannel.ID)
	assert.ErrorIs(t, err, ErrNotFound)

	// Attachment is checked on both sides. Without the channel check a tenant
	// could point its own monitor at another tenant's webhook and have its
	// alerts delivered to a target it does not own; without the monitor check
	// it could rewrite someone else's monitor.
	require.NoError(t, st.SetMonitorChannels(acme, acmeMonitor.ID, []int64{acmeChannel.ID}))
	assert.ErrorIs(t, st.SetMonitorChannels(acme, acmeMonitor.ID, []int64{betaChannel.ID}), ErrNotFound)
	assert.ErrorIs(t, st.SetMonitorChannels(beta, acmeMonitor.ID, []int64{betaChannel.ID}), ErrNotFound)

	attached, err := st.ListChannelsForMonitor(acme, acmeMonitor.ID)
	require.NoError(t, err)
	require.Len(t, attached, 1)
	assert.Equal(t, acmeChannel.ID, attached[0].ID)
	attachedBeta, err := st.ListChannelsForMonitor(beta, acmeMonitor.ID)
	require.NoError(t, err)
	assert.Empty(t, attachedBeta)

	chansAcme, err := st.ListChannels(acme, false)
	require.NoError(t, err)
	assert.Len(t, chansAcme, 1)
	chansBeta, err := st.ListChannels(beta, false)
	require.NoError(t, err)
	assert.Len(t, chansBeta, 1)
	pageChansBeta, err := st.ListChannelsPage(beta, ListFilter{Limit: 50})
	require.NoError(t, err)
	require.Len(t, pageChansBeta, 1)
	assert.Equal(t, betaChannel.ID, pageChansBeta[0].ID)

	// Duplicating a monitor copies only attachments this tenant can see.
	dup, err := st.DuplicateMonitor(acme, acmeMonitor.ID)
	require.NoError(t, err)
	assert.Equal(t, []int64{acmeChannel.ID}, dup.ChannelIDs)
	dupChannels, err := st.ListChannelsForMonitor(acme, dup.ID)
	require.NoError(t, err)
	require.Len(t, dupChannels, 1)
	assert.Equal(t, acmeChannel.ID, dupChannels[0].ID)

	// --- alerts ---

	acmeAlert := &Alert{MonitorID: acmeMonitor.ID, RuleID: rule.ID, EventID: "ev-acme", Payload: json.RawMessage(`{"contract_id":"CAACME"}`)}
	outcome, err := st.CreateAlert(acme, acmeAlert)
	require.NoError(t, err)
	assert.Equal(t, AlertCreated, outcome)

	// A rule belonging to another tenant's monitor is rejected even though
	// both foreign keys exist: alerts.monitor_id and alerts.rule_id are
	// independent constraints, so only the join stops the mismatch.
	_, err = st.CreateAlert(beta, &Alert{MonitorID: betaMonitor.ID, RuleID: rule.ID, EventID: "ev-cross"})
	assert.ErrorIs(t, err, ErrNotFound, "an alert cannot claim another workspace's rule")

	alertsAcme, err := st.ListAlerts(acme, AlertFilter{Limit: 50})
	require.NoError(t, err)
	require.Len(t, alertsAcme, 1)
	assert.Equal(t, "ev-acme", alertsAcme[0].EventID)
	alertsBeta, err := st.ListAlerts(beta, AlertFilter{Limit: 50})
	require.NoError(t, err)
	assert.Empty(t, alertsBeta, "an alert's workspace comes from its monitor, not from the caller")

	_, err = st.GetAlert(beta, acmeAlert.ID)
	assert.ErrorIs(t, err, ErrNotFound)
	written, err := st.GetAlert(acme, acmeAlert.ID)
	require.NoError(t, err)
	assert.Equal(t, acmeMonitor.ID, written.MonitorID)

	// Delivery attempts are scoped through their alert.
	require.NoError(t, st.RecordDeliveryAttempt(system, &DeliveryAttempt{
		AlertID: acmeAlert.ID, ChannelID: acmeChannel.ID, Status: DeliveryStatusSuccess, ResponseSnippet: "ok",
	}))
	attemptsBeta, err := st.ListDeliveryAttempts(beta, acmeAlert.ID, "")
	require.NoError(t, err)
	assert.Empty(t, attemptsBeta)
	attemptsAcme, err := st.ListDeliveryAttempts(acme, acmeAlert.ID, "")
	require.NoError(t, err)
	assert.Len(t, attemptsAcme, 1)

	// --- aggregates ---

	statsAcme, err := st.GetStats(acme)
	require.NoError(t, err)
	assert.Equal(t, int64(2), statsAcme.Monitors, "the original plus its duplicate")
	assert.Equal(t, int64(1), statsAcme.Alerts)
	assert.Equal(t, int64(1), statsAcme.Channels)
	statsBeta, err := st.GetStats(beta)
	require.NoError(t, err)
	assert.Equal(t, int64(1), statsBeta.Monitors)
	assert.Equal(t, int64(0), statsBeta.Alerts)
	assert.Equal(t, int64(0), statsBeta.Rules, "beta's own rows only")

	seriesBeta, err := st.AlertCountsByDay(beta, AlertSeriesDays)
	require.NoError(t, err)
	var totalBeta int64
	for _, day := range seriesBeta {
		totalBeta += day.Count
	}
	assert.Zero(t, totalBeta, "acme's alert must not appear in beta's chart")

	// --- saved searches and templates ---

	// "At most one default search" used to be an instance-wide rule backed by a
	// partial unique index. Under tenancy it is a per-workspace rule, and the
	// second tenant to set a default would hit a unique violation if that index
	// had been left as 0007 wrote it.
	require.NoError(t, st.CreateSavedSearch(acme, &SavedSearch{Name: "mine", Filter: SavedSearchFilter{ContractID: "CAACME"}, IsDefault: true}))
	require.NoError(t, st.CreateSavedSearch(beta, &SavedSearch{Name: "mine", Filter: SavedSearchFilter{ContractID: "CBETAB"}, IsDefault: true}))
	searchesAcme, err := st.ListSavedSearches(acme)
	require.NoError(t, err)
	require.Len(t, searchesAcme, 1)
	assert.True(t, searchesAcme[0].IsDefault)
	searchesBeta, err := st.ListSavedSearches(beta)
	require.NoError(t, err)
	require.Len(t, searchesBeta, 1)
	assert.NotEqual(t, searchesAcme[0].ID, searchesBeta[0].ID, "the same search name in two workspaces")

	// Clearing the previous default is a per-workspace operation, so setting
	// one tenant's default cannot silently disarm the other's.
	require.NoError(t, st.CreateSavedSearch(acme, &SavedSearch{Name: "second", Filter: SavedSearchFilter{ContractID: "CBETAB"}}))
	require.NoError(t, st.SetDefaultSearch(acme, searchesAcme[0].ID))
	afterBeta, err := st.ListSavedSearches(beta)
	require.NoError(t, err)
	assert.True(t, afterBeta[0].IsDefault, "clearing the default must be scoped to the caller's workspace")
	assert.ErrorIs(t, st.DeleteSavedSearch(beta, searchesAcme[0].ID), ErrNotFound)

	tmpl := &MonitorTemplate{Name: "standard", Description: "d", Rules: []MonitorTemplateRule{{Type: "event_emitted", Params: json.RawMessage(`{}`)}}}
	require.NoError(t, st.CreateMonitorTemplate(acme, tmpl))
	templatesBeta, err := st.ListMonitorTemplates(beta)
	require.NoError(t, err)
	assert.Empty(t, templatesBeta)
	templatesAcme, err := st.ListMonitorTemplates(acme)
	require.NoError(t, err)
	assert.Len(t, templatesAcme, 1)
	assert.ErrorIs(t, st.DeleteMonitorTemplate(beta, tmpl.ID), ErrNotFound)

	// --- the cross-tenant scope ---

	// Instance work (ingest, retention, reorg) runs under WithSystem and is the
	// only context allowed to see every tenant at once.
	all, err := st.ListMonitors(system, false)
	require.NoError(t, err)
	assert.Len(t, all, 3, "two tenants plus the duplicate")
	allAlerts, err := st.ListAlerts(system, AlertFilter{Limit: 50})
	require.NoError(t, err)
	assert.Len(t, allAlerts, 1)

	// --- workspace rows ---

	// Seeding is idempotent: it runs at every startup, so a restart must
	// neither fail nor rewrite what is already there.
	for range 2 {
		require.NoError(t, st.EnsureWorkspace(bg, "acme"))
	}
	require.NoError(t, st.EnsureWorkspace(bg, workspace.Default), "the default workspace already exists from the migration")
}

// tableDeleter is the shared delete-by-id helper both backends route their
// scoped deletes through. It exists only here: the allowlist is a security
// boundary, so the test runs the real method rather than reading its SQL.
type tableDeleter interface {
	deleteByID(ctx context.Context, table string, id int64) error
}

// testDeleteByIDAllowlist pins the one place a table name reaches SQL as Go
// text instead of a bound parameter. The name comes from this package's own
// callers, but constraining it is what keeps deleteByID safe for the next one,
// so both properties are asserted: the four workspace-bearing tables are
// deletable, everything else is refused before any SQL is built.
func testDeleteByIDAllowlist(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	bg := context.Background()

	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(bg, m))

	d, ok := st.(tableDeleter)
	require.True(t, ok, "both backends delete through deleteByID")

	// An id no row owns proves the table name was accepted: the answer is
	// ErrNotFound from the missing row, which is a different failure from the
	// refusal. Using a missing id keeps the fixture intact for the assertions
	// after this one.
	missing := int64(999999)
	for table := range deletableTables {
		assert.ErrorIs(t, d.deleteByID(bg, table, missing), ErrNotFound, "%s must stay deletable", table)
	}
	for _, table := range append(append([]string{}, unallowlistedTableNames...), "monitors; DROP TABLE monitors") {
		assert.ErrorIs(t, d.deleteByID(bg, table, m.ID), errUnallowlistedTable, "%q must not be deletable", table)
	}
	_, err := st.GetMonitor(bg, m.ID)
	require.NoError(t, err, "a refused delete must leave the row alone")
}

// unallowlistedTableNames is checked against deleteByID's map rather than its
// SQL, so it lives with the test that runs the method.
var unallowlistedTableNames = []string{"workspaces", "rules", "alerts", "monitor_channels", "ingest_state"}

// TestDeleteTableAllowlist asserts the allowlist as data, so it is enforced
// without a database and cannot drift from a backend's SQL silently.
func TestDeleteTableAllowlist(t *testing.T) {
	for _, table := range []string{"monitors", "channels", "saved_searches", "monitor_templates"} {
		assert.True(t, deletableTables[table], "%s must be deletable through deleteByID", table)
	}
	for _, table := range unallowlistedTableNames {
		assert.False(t, deletableTables[table], "%s must not be deletable through deleteByID", table)
	}
}

// unscopedStoreMethods lists the methods allowed to serve a cross-tenant
// context, with why. It is the readable counterpart to the source scan below
// and to the conditional predicates in each of these methods.
var unscopedStoreMethods = map[string]string{
	"ListMonitors":            "the poller schedules every tenant's monitors",
	"ListMonitorsPage":        "serves a system context so the poller's view matches the dashboard's",
	"ListAlerts":              "the frequency rule reads one rule's history; rule_id implies the tenant",
	"ListRules":               "the poller loads rules for a monitor it already resolved",
	"ListChannelsForMonitor":  "the dispatcher delivers for a monitor it already resolved",
	"RecordDeliveryAttempt":   "the dispatcher writes for an alert it already resolved",
	"CreateAlert":             "derives the workspace from the monitor it inserts into",
	"ExpiredAlerts":           "retention sweeps every workspace",
	"GetIngestState":          "the ledger cursor is per instance, not per tenant",
	"SetIngestState":          "the ledger cursor is per instance, not per tenant",
	"RecordLedgerHashes":      "the reorg window is per network, not per tenant",
	"LedgerHashes":            "the reorg window is per network, not per tenant",
	"PruneLedgerHashes":       "the reorg window is per network, not per tenant",
	"RetractAlertsFromLedger": "a reorg orphans every tenant's alerts on that ledger",
	"DeleteExpiredAlerts":     "retention sweeps every workspace, and ExpiredAlerts selects the rows it deletes",
	"EnsureWorkspace":         "takes the workspace as an argument",
	"AssignLegacyNetwork":     "the one-time startup upgrade labels rows the migration left unlabelled in every workspace; its predicate is the empty network, not a tenant",
	"TokenByHash":             "authenticating: the token's own row is where its workspace comes from, so the read cannot be scoped to a tenant the caller does not have yet",
}

// scannedFiles are the store sources that hold queries against tenant tables.
// New files belong here the moment they start embedding SQL, or a method in
// them would pass the scan by being invisible to it.
var scannedFiles = []string{"postgres.go", "sqlite.go", "prune.go", "partition.go"}

// TestEveryScopedQueryCarriesWorkspace is the guard the design leans on: a new
// store method that touches a tenant's table without scoping to the caller's
// workspace fails here, with no database involved.
//
// The rule is deliberately blunt — any statement in either backend that names
// a workspace-bearing table must also name workspace_id — and the exceptions
// are the methods listed above. It complements testWorkspaceIsolation: that
// test proves the methods that exist today, this one covers the ones nobody
// has written yet.
//
// A table counts as named only where a statement reads or writes it, after
// FROM/INTO/UPDATE/JOIN. Every literal in the function is still concatenated
// before the match, so a statement assembled in pieces is judged as a whole;
// the position test is only what stops a log message saying "alerts" from
// obliging a predicate.
func TestEveryScopedQueryCarriesWorkspace(t *testing.T) {
	tenantTableRead := regexp.MustCompile(`(?i)\b(?:FROM|INTO|UPDATE|JOIN)\s+(?:ONLY\s+)?(monitors|channels|alerts|saved_searches|monitor_templates|api_tokens)\b`)

	for _, file := range scannedFiles {
		functions := sqlFunctions(t, file)
		require.NotEmpty(t, functions, "%s parsed into no functions", file)
		helpers := scopingHelperNames()
		for _, fn := range functions {
			if !tenantTableRead.MatchString(fn.sql) {
				continue
			}
			if strings.Contains(fn.sql, "workspace_id") {
				continue
			}
			if helpers[fn.name] {
				// Checked as any other query: it is here only so a wrapper's
				// delegation can be trusted, not so the helper escapes the rule.
				t.Errorf("%s: %s is a declared scoping helper but carries no workspace_id predicate", file, fn.name)
				continue
			}
			if delegatesToHelper(fn, helpers) {
				continue
			}
			if _, ok := unscopedStoreMethods[fn.name]; ok {
				// Deliberate, and documented in the map. Its staleness is
				// TestUnscopedStoreMethodsAreReal's job.
				continue
			}
			// Schema maintenance lives in the same files as the queries and
			// is instance-level by definition.
			if isSchemaStatement(fn.sql) {
				continue
			}
			t.Errorf("%s: %s touches a workspace-scoped table with no workspace_id predicate:\n%s\nIf this is deliberate, add the method to unscopedStoreMethods with a reason.",
				file, fn.name, fn.sql)
		}
	}
}

// delegatesToHelper reports whether a function's tenant SQL all runs inside a
// scoping helper it calls.
func delegatesToHelper(fn sqlFunc, helpers map[string]bool) bool {
	for callee := range fn.callees {
		if helpers[callee] {
			return true
		}
	}
	return false
}

// TestScopingHelpersAreReal keeps the delegation list honest: a helper that
// was renamed away stops shielding its callers, and the wrappers above would
// then fail the scan with no predicate of their own.
func TestScopingHelpersAreReal(t *testing.T) {
	defined := map[string]bool{}
	for _, file := range scannedFiles {
		for _, fn := range sqlFunctions(t, file) {
			defined[fn.name] = true
		}
	}
	for name := range scopingHelpers {
		assert.True(t, defined[name], "scopingHelpers lists %s, which no backend defines", name)
	}
}

// TestUnscopedStoreMethodsAreReal keeps the allowlist above honest: a method
// that was renamed or deleted must not stay listed, because an entry nobody
// calls is how an unscoped query becomes permanent.
func TestUnscopedStoreMethodsAreReal(t *testing.T) {
	called := map[string]bool{}
	for _, file := range scannedFiles {
		for _, fn := range sqlFunctions(t, file) {
			called[fn.name] = true
		}
	}
	for name := range unscopedStoreMethods {
		assert.True(t, called[name], "unscopedStoreMethods lists %s, which no backend queries", name)
	}
}

// sqlFunc is one function's text: every string literal in its body,
// concatenated in source order. Concatenating is what lets a statement
// assembled in pieces still be judged as a whole — `UPDATE monitors SET ...
// WHERE id IN (` + placeholders(n) + `) AND workspace_id = ?` would otherwise
// fail the scan on its first fragment, which is the half that names the table.
//
// No literal is filtered out, so a stray mention of a tenant table in an error
// message counts as a match. That is the right direction for a blunt rule to
// fall: it can ask for a predicate that is already there, and never wave one
// through.
type sqlFunc struct {
	name    string
	sql     string
	callees map[string]bool
}

// scopingHelpers are unexported methods that apply the tenant predicate for
// their caller, so a thin wrapper over one of them is scoped even though its
// own text names nothing but a table. Each of these is itself checked by the
// scan, so delegating cannot hide an unscoped query — it moves the obligation.
var scopingHelpers = map[string]string{
	"deleteByID": "DELETE ... AND workspace_id = ?, gated by deletableTables",
}

func scopingHelperNames() map[string]bool {
	names := make(map[string]bool, len(scopingHelpers))
	for name := range scopingHelpers {
		names[name] = true
	}
	return names
}

func sqlFunctions(t *testing.T, file string) []sqlFunc {
	t.Helper()
	src, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var out []sqlFunc
	for _, decl := range src.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		var b strings.Builder
		callees := map[string]bool{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				switch callee := call.Fun.(type) {
				case *ast.SelectorExpr:
					callees[callee.Sel.Name] = true
				case *ast.Ident:
					callees[callee.Name] = true
				}
			}
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			b.WriteString(s)
			b.WriteByte('\n')
			return true
		})
		out = append(out, sqlFunc{name: fn.Name.Name, sql: b.String(), callees: callees})
	}
	return out
}

// isSchemaStatement reports whether a function's text is DDL. Migrations and
// partition maintenance run over the whole instance and name tables without
// owing a tenant predicate.
func isSchemaStatement(sql string) bool {
	upper := strings.ToUpper(sql)
	return strings.Contains(upper, "CREATE TABLE") || strings.Contains(upper, "CREATE INDEX") ||
		strings.Contains(upper, "CREATE UNIQUE") || strings.Contains(upper, "ALTER TABLE") ||
		strings.Contains(upper, "DROP TABLE")
}
