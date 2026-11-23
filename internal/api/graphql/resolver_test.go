package graphql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"

	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// This file covers the resolver package against the schema it serves.
//
// The handler in resolver.go is a stub: NewHandler writes a fixed JSON body and
// never calls the resolver it was handed, so no query can be executed over HTTP
// today (see docs/reference/graphql.md, finding 1). That forces the shape of
// this file: the query and nested-field tests drive the resolver methods
// directly, because that is the only contract that exists to test, while
// testHandler* pins what the HTTP surface actually does so the executor's
// landing cannot go unnoticed. Once a real executor exists, those handler tests
// become end-to-end tests and the direct calls below become the unit layer.

const graphqlSchemaPath = "schema.graphqls"

// loadSchema parses the schema the package serves. Loading the real file —
// rather than restating its fields in Go — is what makes the coverage sweeps
// below fail when a field is added.
func loadSchema(t *testing.T) *ast.Schema {
	t.Helper()
	src, err := os.ReadFile(graphqlSchemaPath)
	require.NoError(t, err)
	schema, err := gqlparser.LoadSchema(&ast.Source{Name: graphqlSchemaPath, Input: string(src)})
	require.NoError(t, err)
	return schema
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestResolver returns a resolver over a fresh SQLite database. SQLite is
// the backend CI's default test job runs: no service container, no
// TEST_DATABASE_URL, so the tenancy assertions below always run rather than
// skipping.
func newTestResolver(t *testing.T) (*Resolver, store.Store) {
	t.Helper()
	url := "sqlite://" + filepath.Join(t.TempDir(), "graphql.db")
	require.NoError(t, store.Migrate(url))
	st, err := store.NewSQLite(context.Background(), url)
	require.NoError(t, err)
	t.Cleanup(st.Close)
	return NewResolver(st, discardLogger()), st
}

// ptr is the pointer-of-value helper the argument-heavy resolver signatures
// need.
func ptr[T any](v T) *T { return &v }

// --- fixtures ---------------------------------------------------------------

const (
	acmeWorkspace = workspace.ID("acme")
	betaWorkspace = workspace.ID("beta")

	// Distinctive strings seeded into workspace beta. A query scoped to acme
	// must not return any of them at any depth.
	betaMonitorName = "beta-treasury"
	betaEventID     = "ev-beta-1"
	betaChannelName = "beta-pager"
	betaWebhook     = "https://hooks.example.invalid/beta-super-secret"
	betaRuleMarker  = "beta_transfer"

	acmeWebhook = "https://hooks.example.invalid/acme-super-secret"
)

// tenants is the two-workspace fixture. Both tenants get one monitor, one
// rule, one enabled channel and one alert, so a leak is visible as the other
// tenant's marker rather than as an empty collection that could be caused by
// anything.
type tenants struct {
	acmeMonitor *store.Monitor
	betaMonitor *store.Monitor
	acmeRule    *store.Rule
	betaRule    *store.Rule
	acmeChannel *store.Channel
	betaChannel *store.Channel
	acmeAlert   *store.Alert
	betaAlert   *store.Alert
}

// acmeCtx is the principal every tenancy test uses: a client scoped to
// workspace acme, which is what the API's auth middleware puts in the request
// context.
func acmeCtx() context.Context { return workspace.With(context.Background(), acmeWorkspace) }

func betaCtx() context.Context { return workspace.With(context.Background(), betaWorkspace) }

func seedTenants(t *testing.T, st store.Store) tenants {
	t.Helper()
	bg := context.Background()
	for _, id := range []workspace.ID{acmeWorkspace, betaWorkspace} {
		require.NoError(t, st.EnsureWorkspace(bg, id))
	}

	acme, beta := acmeCtx(), betaCtx()
	tt := tenants{}

	tt.acmeMonitor = &store.Monitor{
		Name: "acme-payments", ContractIDs: []string{"CAACME"}, Enabled: true,
		Priority: store.PriorityHigh,
	}
	require.NoError(t, st.CreateMonitor(acme, tt.acmeMonitor))
	tt.betaMonitor = &store.Monitor{
		Name: betaMonitorName, ContractIDs: []string{"CBETA"}, Enabled: false,
		Priority: store.PriorityLow,
	}
	require.NoError(t, st.CreateMonitor(beta, tt.betaMonitor))

	tt.acmeRule = &store.Rule{
		MonitorID: tt.acmeMonitor.ID, Type: "event_emitted",
		Params: json.RawMessage(`{"event_name":"transfer"}`), Enabled: true,
		Severity: store.SeverityCritical,
	}
	require.NoError(t, st.CreateRule(acme, tt.acmeRule))
	tt.betaRule = &store.Rule{
		MonitorID: tt.betaMonitor.ID, Type: "event_emitted",
		Params: json.RawMessage(`{"event_name":"` + betaRuleMarker + `"}`), Enabled: true,
		Severity: store.SeverityWarning,
	}
	require.NoError(t, st.CreateRule(beta, tt.betaRule))

	tt.acmeChannel = &store.Channel{
		Name: "acme-slack", Type: "slack", Enabled: true,
		Config:      json.RawMessage(`{"webhook_url":"` + acmeWebhook + `"}`),
		MinSeverity: store.SeverityInfo,
	}
	require.NoError(t, st.CreateChannel(acme, tt.acmeChannel))
	tt.betaChannel = &store.Channel{
		Name: betaChannelName, Type: "pagerduty", Enabled: true,
		Config:      json.RawMessage(`{"webhook_url":"` + betaWebhook + `"}`),
		MinSeverity: store.SeverityCritical,
	}
	require.NoError(t, st.CreateChannel(beta, tt.betaChannel))

	require.NoError(t, st.SetMonitorChannels(acme, tt.acmeMonitor.ID, []int64{tt.acmeChannel.ID}))
	require.NoError(t, st.SetMonitorChannels(beta, tt.betaMonitor.ID, []int64{tt.betaChannel.ID}))

	tt.acmeAlert = &store.Alert{
		MonitorID: tt.acmeMonitor.ID, RuleID: tt.acmeRule.ID, EventID: "ev-acme-1",
		Payload: json.RawMessage(`{"contract_id":"CAACME"}`), Ledger: 100,
	}
	outcome, err := st.CreateAlert(acme, tt.acmeAlert)
	require.NoError(t, err)
	require.Equal(t, store.AlertCreated, outcome)

	tt.betaAlert = &store.Alert{
		MonitorID: tt.betaMonitor.ID, RuleID: tt.betaRule.ID, EventID: betaEventID,
		Payload: json.RawMessage(`{"contract_id":"CBETA"}`), Ledger: 200,
	}
	outcome, err = st.CreateAlert(beta, tt.betaAlert)
	require.NoError(t, err)
	require.Equal(t, store.AlertCreated, outcome)

	// Record which rule suppressed beta's alert, so Alert.inhibitedByRule has a
	// foreign rule id to refuse rather than only an absent one.
	require.NoError(t, st.MarkAlertInhibited(beta, tt.betaAlert.ID, tt.betaRule.ID))
	stored, err := st.GetAlert(beta, tt.betaAlert.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.InhibitedByRuleID)

	return tt
}

// seedMonitors adds n monitors named prefix-00, prefix-01, ... to ctx and
// returns them in creation order.
func seedMonitors(t *testing.T, st store.Store, ctx context.Context, prefix string, n int) []store.Monitor {
	t.Helper()
	out := make([]store.Monitor, n)
	for i := range out {
		out[i] = store.Monitor{
			Name:        fmt.Sprintf("%s-%02d", prefix, i),
			ContractIDs: []string{fmt.Sprintf("C%s%02d", strings.ToUpper(prefix), i)},
			Enabled:     i%2 == 0,
			Priority:    store.PriorityNormal,
		}
		require.NoError(t, st.CreateMonitor(ctx, &out[i]))
	}
	return out
}

func seedAlerts(t *testing.T, st store.Store, ctx context.Context, monitorID, ruleID int64, prefix string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		a := &store.Alert{
			MonitorID: monitorID,
			RuleID:    ruleID,
			EventID:   fmt.Sprintf("%s-%03d", prefix, i),
			Payload:   json.RawMessage(`{"contract_id":"CAACME"}`),
			Ledger:    uint32(100 + i),
		}
		outcome, err := st.CreateAlert(ctx, a)
		require.NoError(t, err)
		require.Equal(t, store.AlertCreated, outcome)
	}
}

// --- schema-driven coverage -------------------------------------------------

// queryFieldTests maps every Query field in schema.graphqls to the test that
// covers it. TestEveryQueryFieldHasATest walks the parsed schema and fails on a
// field with no entry here, so a query added without a test is a red build
// rather than a silent gap.
var queryFieldTests = map[string]func(t *testing.T){
	"monitor":     TestQueryMonitor,
	"monitors":    TestQueryMonitors,
	"alert":       TestQueryAlert,
	"alerts":      TestQueryAlerts,
	"stats":       TestQueryStats,
	"alertSeries": TestQueryAlertSeries,
}

// nestedFieldTests covers the fields that reach the store on their own behalf,
// as opposed to being copied out of the parent row.
var nestedFieldTests = map[string]func(t *testing.T){
	"Monitor.rules":           TestMonitorRules,
	"Monitor.channels":        TestMonitorChannels,
	"Alert.inhibitedByRuleId": TestAlertInhibitedByRule,
}

// plainModelFields is the allowlist of schema fields satisfied by a value the
// parent resolver already returned. Everything else on the four model types
// must appear in nestedFieldTests.
var plainModelFields = map[string]bool{
	"Monitor.id": true, "Monitor.name": true, "Monitor.contractIds": true,
	"Monitor.enabled": true, "Monitor.priority": true, "Monitor.createdAt": true,
	"Monitor.lastMatchedAt": true,
	"Rule.id":               true, "Rule.monitorId": true, "Rule.type": true,
	"Rule.params": true, "Rule.enabled": true, "Rule.severity": true,
	"Channel.id": true, "Channel.name": true, "Channel.type": true,
	"Channel.enabled": true, "Channel.digestMode": true,
	"Channel.digestWindowSeconds": true, "Channel.minSeverity": true,
	"Channel.createdAt": true,
	"Alert.id":          true, "Alert.monitorId": true, "Alert.ruleId": true,
	"Alert.eventId": true, "Alert.payload": true, "Alert.createdAt": true,
	"Alert.severity": true, "Alert.ledger": true, "Alert.retractedAt": true,
	"Alert.backfilled": true,
}

// TestEveryQueryFieldHasATest is the coverage sweep the issue asks for: it is
// driven from schema.graphqls, so a field added to the schema without a test
// fails here.
func TestEveryQueryFieldHasATest(t *testing.T) {
	schema := loadSchema(t)
	require.NotNil(t, schema.Query, "schema.graphqls must declare a Query type")

	// The schema is documented as read-only. A Mutation or Subscription would
	// need its own coverage before it could ship.
	assert.Nil(t, schema.Mutation, "schema declares a Mutation type; it is documented as read-only, so add tests and update the docs")
	assert.Nil(t, schema.Subscription, "schema declares a Subscription type; it is documented as read-only, so add tests and update the docs")

	covered := make(map[string]bool, len(schema.Query.Fields))
	for _, field := range schema.Query.Fields {
		if strings.HasPrefix(field.Name, "__") {
			// gqlparser injects the introspection meta-fields (__schema,
			// __type) into Query. They are served by the executor, not by a
			// resolver in this package, so they are not part of this sweep.
			continue
		}
		covered[field.Name] = true
		test, ok := queryFieldTests[field.Name]
		if !ok {
			t.Errorf("schema.graphqls declares Query.%s but queryFieldTests has no entry for it", field.Name)
			continue
		}
		t.Run(field.Name, test)
	}
	for name := range queryFieldTests {
		assert.Truef(t, covered[name], "queryFieldTests covers %q, which schema.graphqls no longer declares", name)
	}
}

// TestEveryNestedResolverFieldHasATest does for the model types what the query
// sweep does for the root: it classifies every field as either a copied value
// or a resolver with a test. A new list or lookup field cannot slip in
// uncovered.
func TestEveryNestedResolverFieldHasATest(t *testing.T) {
	schema := loadSchema(t)

	accounted := make(map[string]bool, len(plainModelFields)+len(nestedFieldTests))
	for _, typeName := range []string{"Monitor", "Rule", "Channel", "Alert"} {
		def := schema.Types[typeName]
		require.NotNil(t, def, "schema.graphqls must declare type %s", typeName)
		for _, field := range def.Fields {
			key := typeName + "." + field.Name
			accounted[key] = true
			if test, ok := nestedFieldTests[key]; ok {
				t.Run(key, test)
				continue
			}
			if !plainModelFields[key] {
				t.Errorf("schema field %s is neither a plain model field nor covered by nestedFieldTests", key)
			}
		}
	}
	for key := range plainModelFields {
		assert.Truef(t, accounted[key], "plainModelFields lists %q, which the schema no longer declares", key)
	}
	for key := range nestedFieldTests {
		assert.Truef(t, accounted[key], "nestedFieldTests covers %q, which the schema no longer declares", key)
	}
}

// TestChannelConfigIsNotResolvable is the disclosure guard. Channel config
// holds webhook URLs, bot tokens and SMTP credentials; no query, at any depth,
// may return it.
func TestChannelConfigIsNotResolvable(t *testing.T) {
	schema := loadSchema(t)

	channel := schema.Types["Channel"]
	require.NotNil(t, channel)
	var fields []string
	for _, f := range channel.Fields {
		fields = append(fields, f.Name)
	}
	assert.ElementsMatch(t,
		[]string{"id", "name", "type", "enabled", "digestMode", "digestWindowSeconds", "minSeverity", "createdAt"},
		fields,
		"adding a field to Channel must be a deliberate act: config must never be one of them")

	// A credential-shaped field anywhere in the schema is the same disclosure
	// under a different name, so the guard is not limited to Channel.
	credentialName := regexp.MustCompile(`(?i)config|secret|password|credential|webhook|bot_token|api_?key`)
	for typeName, def := range schema.Types {
		for _, f := range def.Fields {
			assert.Falsef(t, credentialName.MatchString(f.Name),
				"%s.%s looks like a credential field; channel config must never be resolvable", typeName, f.Name)
		}
	}

	// ChannelResolver has no methods, which is what makes an unlisted field
	// unresolvable rather than merely absent from the schema.
	assert.Zero(t, reflect.TypeOf((*ChannelResolver)(nil)).Elem().NumMethod(),
		"ChannelResolver must stay empty: a method here can resolve a field the schema does not name")

	r, st := newTestResolver(t)
	tt := seedTenants(t, st)
	ctx := acmeCtx()

	// The fixture really does hold a secret, so the assertions below are not
	// vacuously true.
	secrets := []string{acmeWebhook, betaWebhook}
	storedAcme, err := st.GetChannel(ctx, tt.acmeChannel.ID)
	require.NoError(t, err)
	require.Contains(t, string(storedAcme.Config), acmeWebhook)

	conn, err := (&monitorQuery{}).run(ctx, r)
	require.NoError(t, err)
	require.NotEmpty(t, conn.Edges)

	var returned []Channel
	for _, edge := range conn.Edges {
		channels, err := r.Monitor().Channels(ctx, edge.Node)
		require.NoError(t, err)
		returned = append(returned, channels...)
	}
	require.NotEmpty(t, returned, "the fixture attaches a channel, so the sweep has something to leak")

	payload, err := json.Marshal(returned)
	require.NoError(t, err)
	for _, secret := range secrets {
		assert.NotContains(t, string(payload), secret, "channel config leaked through Monitor.channels")
	}
	assert.NotContains(t, string(payload), `"Config"`, "the GraphQL Channel model must not carry the store's config field")
}

// --- query coverage ---------------------------------------------------------

func TestQueryMonitor(t *testing.T) {
	r, st := newTestResolver(t)
	tt := seedTenants(t, st)
	ctx := acmeCtx()

	got, err := r.Query().Monitor(ctx, strconv.FormatInt(tt.acmeMonitor.ID, 10))
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, strconv.FormatInt(tt.acmeMonitor.ID, 10), got.ID)
	assert.Equal(t, "acme-payments", got.Name)
	assert.Equal(t, []string{"CAACME"}, got.ContractIDs)
	assert.True(t, got.Enabled)
	assert.Equal(t, PriorityHigh, got.Priority)
	assert.False(t, got.CreatedAt.Time.IsZero())
	assert.Nil(t, got.LastMatchedAt, "a monitor that never fired must not be given a timestamp")

	t.Run("unknown id is null, not an error", func(t *testing.T) {
		got, err := r.Query().Monitor(ctx, "999999")
		assert.NoError(t, err)
		assert.Nil(t, got)
	})
}

// monitorQuery mirrors the argument list of Query.monitors, for the same
// reason alertQuery exists.
type monitorQuery struct {
	enabled *bool
	limit   *int
	cursor  *string
	query   *string
	sort    *MonitorSort
}

func (q monitorQuery) run(ctx context.Context, r *Resolver) (*MonitorConnection, error) {
	return r.Query().Monitors(ctx, q.enabled, q.limit, q.cursor, q.query, q.sort)
}

func TestQueryMonitors(t *testing.T) {
	r, st := newTestResolver(t)
	seedTenants(t, st)
	ctx := acmeCtx()
	seedMonitors(t, st, ctx, "page", 3)

	// The fixture plus the three seeded monitors, all in acme.
	all, err := (&monitorQuery{}).run(ctx, r)
	require.NoError(t, err)
	assert.Len(t, all.Edges, 4)
	assert.False(t, all.PageInfo.HasNextPage)
	assert.False(t, all.PageInfo.HasPreviousPage)

	cases := []struct {
		name  string
		query monitorQuery
		want  []string // monitor names, in the order the resolver returns them
	}{
		{
			name:  "enabled only",
			query: monitorQuery{enabled: ptr(true)},
			want:  []string{"acme-payments", "page-00", "page-02"},
		},
		{
			name:  "disabled only",
			query: monitorQuery{enabled: ptr(false)},
			want:  []string{"page-01"},
		},
		{
			name:  "name substring is case insensitive",
			query: monitorQuery{query: ptr("PAGE-0")},
			want:  []string{"page-00", "page-01", "page-02"},
		},
		{
			name:  "sort by name",
			query: monitorQuery{sort: ptr(MonitorSortName)},
			want:  []string{"acme-payments", "page-00", "page-01", "page-02"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := tc.query.run(ctx, r)
			require.NoError(t, err)
			var got []string
			for _, edge := range conn.Edges {
				got = append(got, edge.Node.Name)
			}
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("cursor pages forward and is echoed on the page", func(t *testing.T) {
		first, err := (monitorQuery{limit: ptr(2), sort: ptr(MonitorSortName)}).run(ctx, r)
		require.NoError(t, err)
		require.Len(t, first.Edges, 2)
		assert.True(t, first.PageInfo.HasNextPage, "a full page reports more, even when the next page may be empty")
		require.NotNil(t, first.PageInfo.StartCursor)
		require.NotNil(t, first.PageInfo.EndCursor)
		assert.Equal(t, first.Edges[0].Cursor, *first.PageInfo.StartCursor)
		assert.Equal(t, first.Edges[len(first.Edges)-1].Cursor, *first.PageInfo.EndCursor)

		second, err := (monitorQuery{limit: ptr(2), sort: ptr(MonitorSortName), cursor: first.PageInfo.EndCursor}).run(ctx, r)
		require.NoError(t, err)
		require.Len(t, second.Edges, 2)
		assert.Equal(t, "page-01", second.Edges[0].Node.Name)
		assert.Equal(t, "page-02", second.Edges[1].Node.Name)
	})

	t.Run("a non-numeric cursor is ignored rather than rejected", func(t *testing.T) {
		conn, err := (monitorQuery{cursor: ptr("not-a-cursor")}).run(ctx, r)
		require.NoError(t, err)
		assert.Len(t, conn.Edges, 4, "the first page is returned again")
	})

	t.Run("an empty page still populates its cursors as empty strings", func(t *testing.T) {
		conn, err := (&monitorQuery{query: ptr("no-such-monitor")}).run(ctx, r)
		require.NoError(t, err)
		assert.Empty(t, conn.Edges)
		require.NotNil(t, conn.PageInfo.StartCursor)
		require.NotNil(t, conn.PageInfo.EndCursor)
		assert.Equal(t, "", *conn.PageInfo.StartCursor)
		assert.Equal(t, "", *conn.PageInfo.EndCursor)
	})

	t.Run("limit is clamped to 50", func(t *testing.T) {
		seedMonitors(t, st, ctx, "clamp", 60)
		conn, err := (monitorQuery{limit: ptr(1000), sort: ptr(MonitorSortID)}).run(ctx, r)
		require.NoError(t, err)
		assert.Len(t, conn.Edges, 50, "the resolver clamps anything above 500 to 50")
		assert.True(t, conn.PageInfo.HasNextPage)
	})
}

func TestQueryAlert(t *testing.T) {
	r, st := newTestResolver(t)
	tt := seedTenants(t, st)
	ctx := acmeCtx()

	got, err := r.Query().Alert(ctx, strconv.FormatInt(tt.acmeAlert.ID, 10))
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "ev-acme-1", got.EventID)
	assert.Equal(t, strconv.FormatInt(tt.acmeRule.ID, 10), got.RuleID)
	assert.Equal(t, 100, got.Ledger)
	assert.Nil(t, got.RetractedAt)
	assert.Nil(t, got.InhibitedByRuleID, "acme's alert was not inhibited")

	t.Run("unknown id is null, not an error", func(t *testing.T) {
		got, err := r.Query().Alert(ctx, "999999")
		assert.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("a foreign id is null, not an error", func(t *testing.T) {
		got, err := r.Query().Alert(ctx, strconv.FormatInt(tt.betaAlert.ID, 10))
		assert.NoError(t, err)
		assert.Nil(t, got)
	})
}

// alertQuery mirrors the argument list of Query.alerts so a table-driven case
// does not repeat nine nils.
type alertQuery struct {
	monitorID  *string
	ruleID     *string
	contractID *string
	from       *DateTime
	to         *DateTime
	severity   *Severity
	limit      *int
	cursor     *string
	sort       *AlertSort
}

func (q alertQuery) run(ctx context.Context, r *Resolver) (*AlertConnection, error) {
	return r.Query().Alerts(ctx, q.monitorID, q.ruleID, q.contractID, q.from, q.to, q.severity, q.limit, q.cursor, q.sort)
}

func TestQueryAlerts(t *testing.T) {
	r, st := newTestResolver(t)
	tt := seedTenants(t, st)
	ctx := acmeCtx()

	// The stored severity is what the store's minimum-severity filter compares
	// against, so the filter case reads it rather than assuming the alert
	// inherited its rule's severity (CreateAlert does not write that column).
	stored, err := st.GetAlert(ctx, tt.acmeAlert.ID)
	require.NoError(t, err)
	storedSeverity := toSeverity(stored.Severity)

	cases := []struct {
		name  string
		query alertQuery
		want  []string // event ids
	}{
		{
			name:  "no filter",
			query: alertQuery{},
			want:  []string{"ev-acme-1"},
		},
		{
			name:  "by monitor",
			query: alertQuery{monitorID: ptr(strconv.FormatInt(tt.acmeMonitor.ID, 10))},
			want:  []string{"ev-acme-1"},
		},
		{
			name:  "by an unknown monitor returns nothing",
			query: alertQuery{monitorID: ptr("999999")},
			want:  nil,
		},
		{
			name:  "by rule",
			query: alertQuery{ruleID: ptr(strconv.FormatInt(tt.acmeRule.ID, 10))},
			want:  []string{"ev-acme-1"},
		},
		{
			name:  "by contract id",
			query: alertQuery{contractID: ptr("CAACME")},
			want:  []string{"ev-acme-1"},
		},
		{
			name:  "by a contract id the alert does not carry",
			query: alertQuery{contractID: ptr("CNOPE")},
			want:  nil,
		},
		{
			name:  "from in the future excludes everything",
			query: alertQuery{from: &DateTime{Time: time.Now().Add(time.Minute)}},
			want:  nil,
		},
		{
			name:  "to in the past excludes everything",
			query: alertQuery{to: &DateTime{Time: time.Now().Add(-time.Minute)}},
			want:  nil,
		},
		{
			name:  "a window that contains the alert returns it",
			query: alertQuery{from: &DateTime{Time: time.Now().Add(-time.Hour)}, to: &DateTime{Time: time.Now().Add(time.Hour)}},
			want:  []string{"ev-acme-1"},
		},
		{
			name:  "minimum severity includes the stored one",
			query: alertQuery{severity: &storedSeverity},
			want:  []string{"ev-acme-1"},
		},
		{
			name:  "a non-numeric cursor is ignored rather than rejected",
			query: alertQuery{cursor: ptr("not-a-cursor")},
			want:  []string{"ev-acme-1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := tc.query.run(ctx, r)
			require.NoError(t, err)
			var got []string
			for _, edge := range conn.Edges {
				got = append(got, edge.Node.EventID)
			}
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("sort ascending reverses the page order", func(t *testing.T) {
		seedAlerts(t, st, ctx, tt.acmeMonitor.ID, tt.acmeRule.ID, "asc", 2)

		desc, err := (alertQuery{sort: ptr(AlertSortCreatedAtDesc)}).run(ctx, r)
		require.NoError(t, err)
		asc, err := (alertQuery{sort: ptr(AlertSortCreatedAtAsc)}).run(ctx, r)
		require.NoError(t, err)

		require.Len(t, desc.Edges, 3)
		require.Len(t, asc.Edges, 3)
		assert.NotEqual(t, desc.Edges[0].Node.EventID, asc.Edges[0].Node.EventID)
		assert.Equal(t, desc.Edges[len(desc.Edges)-1].Node.EventID, asc.Edges[0].Node.EventID)
	})

	t.Run("cursors page forward", func(t *testing.T) {
		first, err := (alertQuery{limit: ptr(2), sort: ptr(AlertSortCreatedAtAsc)}).run(ctx, r)
		require.NoError(t, err)
		require.Len(t, first.Edges, 2)
		assert.NotNil(t, first.PageInfo.EndCursor)

		second, err := (alertQuery{limit: ptr(2), sort: ptr(AlertSortCreatedAtAsc), cursor: first.PageInfo.EndCursor}).run(ctx, r)
		require.NoError(t, err)
		require.Len(t, second.Edges, 1)
		assert.NotEqual(t, first.Edges[0].Node.ID, second.Edges[0].Node.ID)
	})

	t.Run("limit is clamped to 100 rather than the monitor cap", func(t *testing.T) {
		seedAlerts(t, st, ctx, tt.acmeMonitor.ID, tt.acmeRule.ID, "bulk", 120)
		conn, err := (alertQuery{limit: ptr(5000)}).run(ctx, r)
		require.NoError(t, err)
		assert.Len(t, conn.Edges, 100)
		assert.True(t, conn.PageInfo.HasNextPage)
	})
}

func TestQueryStats(t *testing.T) {
	r, st := newTestResolver(t)
	seedTenants(t, st)
	ctx := acmeCtx()

	// A second acme monitor, rule and alert make acme's totals differ from
	// beta's, so a cross-workspace aggregate would be visible.
	extra := seedMonitors(t, st, ctx, "stats", 1)[0]
	extraRule := &store.Rule{MonitorID: extra.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, extraRule))
	seedAlerts(t, st, ctx, extra.ID, extraRule.ID, "stats", 1)

	got, err := r.Query().Stats(ctx)
	require.NoError(t, err)
	require.NotNil(t, got)

	want, err := st.GetStats(ctx)
	require.NoError(t, err)
	assert.Equal(t, int(want.Monitors), got.Monitors)
	assert.Equal(t, int(want.Rules), got.Rules)
	assert.Equal(t, int(want.Channels), got.Channels)
	assert.Equal(t, int(want.AlertsLast24), got.AlertsLast24h)

	assert.Equal(t, 2, got.Monitors)
	assert.Equal(t, 2, got.Rules)
	assert.Equal(t, 1, got.Channels, "beta's channel must not be counted")
	assert.Equal(t, 2, got.Alerts, "beta's alert must not be counted")
	assert.Zero(t, got.LastLedger, "no poll has advanced the ingest checkpoint in this test")

	beta, err := r.Query().Stats(betaCtx())
	require.NoError(t, err)
	assert.Equal(t, 1, beta.Monitors)
	assert.Equal(t, 1, beta.Alerts)
}

func TestQueryAlertSeries(t *testing.T) {
	r, st := newTestResolver(t)
	tt := seedTenants(t, st)
	ctx := acmeCtx()

	// acme deliberately has more alerts than beta, so a leak changes the sum.
	seedAlerts(t, st, ctx, tt.acmeMonitor.ID, tt.acmeRule.ID, "series", 2)

	cases := []struct {
		name string
		days *int
		want int // number of buckets
	}{
		{name: "default is 30 days", days: nil, want: 30},
		{name: "a requested window is honoured", days: ptr(7), want: 7},
		{name: "non-positive falls back to 30", days: ptr(0), want: 30},
		{name: "above 90 is clamped", days: ptr(200), want: 90},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			series, err := r.Query().AlertSeries(ctx, tc.days)
			require.NoError(t, err)
			require.Len(t, series, tc.want)
		})
	}

	series, err := r.Query().AlertSeries(ctx, nil)
	require.NoError(t, err)
	var total int
	for _, day := range series {
		total += day.Count
	}
	assert.Equal(t, 3, total, "today's acme alerts only; beta's is not in the chart")

	for _, day := range series {
		_, err := time.Parse("2006-01-02", day.Day)
		assert.NoErrorf(t, err, "day %q must be a UTC calendar date", day.Day)
	}
}

// --- nested field coverage --------------------------------------------------

func TestMonitorRules(t *testing.T) {
	r, st := newTestResolver(t)
	tt := seedTenants(t, st)
	ctx := acmeCtx()

	rules, err := r.Monitor().Rules(ctx, toMonitor(tt.acmeMonitor))
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, strconv.FormatInt(tt.acmeRule.ID, 10), rules[0].ID)
	assert.Equal(t, strconv.FormatInt(tt.acmeMonitor.ID, 10), rules[0].MonitorID)
	assert.Equal(t, "event_emitted", rules[0].Type)
	assert.JSONEq(t, `{"event_name":"transfer"}`, string(rules[0].Params))
	assert.True(t, rules[0].Enabled)

	// Compare against the stored row rather than the value the fixture asked
	// for: the store's create path does not write rules.severity, so the column
	// keeps its default (see the findings on the PR). The mapping itself is
	// covered by TestSeverityAndPriorityMapping.
	stored, err := st.GetRule(ctx, tt.acmeRule.ID)
	require.NoError(t, err)
	assert.Equal(t, toSeverity(stored.Severity), rules[0].Severity)

	t.Run("a foreign monitor has no rules", func(t *testing.T) {
		rules, err := r.Monitor().Rules(ctx, toMonitor(tt.betaMonitor))
		require.NoError(t, err)
		assert.Empty(t, rules)
	})
}

func TestMonitorChannels(t *testing.T) {
	r, st := newTestResolver(t)
	tt := seedTenants(t, st)
	ctx := acmeCtx()

	channels, err := r.Monitor().Channels(ctx, toMonitor(tt.acmeMonitor))
	require.NoError(t, err)
	require.Len(t, channels, 1)
	assert.Equal(t, "acme-slack", channels[0].Name)
	assert.Equal(t, "slack", channels[0].Type)

	// As with rules.severity, the store's create path does not write
	// channels.min_severity; the mapping is asserted against what was stored.
	stored, err := st.GetChannel(ctx, tt.acmeChannel.ID)
	require.NoError(t, err)
	assert.Equal(t, toSeverity(stored.MinSeverity), channels[0].MinSeverity)

	t.Run("a foreign monitor has no channels", func(t *testing.T) {
		channels, err := r.Monitor().Channels(ctx, toMonitor(tt.betaMonitor))
		require.NoError(t, err)
		assert.Empty(t, channels)
	})

	t.Run("a disabled channel is not delivered to and is not returned", func(t *testing.T) {
		disabled := &store.Channel{Name: "paused", Type: "slack", Config: json.RawMessage(`{}`), Enabled: false}
		require.NoError(t, st.CreateChannel(ctx, disabled))
		require.NoError(t, st.SetMonitorChannels(ctx, tt.acmeMonitor.ID, []int64{tt.acmeChannel.ID, disabled.ID}))

		channels, err := r.Monitor().Channels(ctx, toMonitor(tt.acmeMonitor))
		require.NoError(t, err)
		require.Len(t, channels, 1)
		assert.Equal(t, "acme-slack", channels[0].Name)
	})
}

func TestAlertInhibitedByRule(t *testing.T) {
	r, st := newTestResolver(t)
	tt := seedTenants(t, st)
	ctx := acmeCtx()

	t.Run("nil when the alert was not inhibited", func(t *testing.T) {
		rule, err := r.Alert().InhibitedByRule(ctx, toAlert(tt.acmeAlert))
		require.NoError(t, err)
		assert.Nil(t, rule)
	})

	t.Run("the rule is resolved within the caller's workspace", func(t *testing.T) {
		alert, err := st.GetAlert(ctx, tt.acmeAlert.ID)
		require.NoError(t, err)
		alert.InhibitedByRuleID = &tt.acmeRule.ID
		rule, err := r.Alert().InhibitedByRule(ctx, toAlert(alert))
		require.NoError(t, err)
		require.NotNil(t, rule)
		assert.Equal(t, strconv.FormatInt(tt.acmeRule.ID, 10), rule.ID)
	})

	t.Run("a foreign rule id resolves to nil", func(t *testing.T) {
		alert, err := st.GetAlert(ctx, tt.acmeAlert.ID)
		require.NoError(t, err)
		alert.InhibitedByRuleID = &tt.betaRule.ID
		rule, err := r.Alert().InhibitedByRule(ctx, toAlert(alert))
		require.NoError(t, err)
		assert.Nil(t, rule, "a rule id from another workspace must not resolve")
	})
}

// --- tenancy ----------------------------------------------------------------

// nestedView is a response assembled the way a nested GraphQL query would
// assemble it: every field resolved under one principal's context.
type nestedView struct {
	Monitor   *Monitor        `json:"monitor"`
	Rules     []Rule          `json:"rules"`
	Channels  []Channel       `json:"channels"`
	Alerts    []Alert         `json:"alerts"`
	Inhibited *Rule           `json:"inhibited_by_rule"`
	Stats     *Stats          `json:"stats"`
	Series    []AlertDayCount `json:"series"`
}

// TestPerFieldTenancy is the requirement the issue calls most important: a
// principal scoped to workspace acme, issuing a deeply nested query, must
// receive nothing belonging to beta at any level.
func TestPerFieldTenancy(t *testing.T) {
	r, st := newTestResolver(t)
	tt := seedTenants(t, st)
	ctx := acmeCtx()

	view := nestedView{}

	// monitor(id): a foreign id is indistinguishable from a missing one.
	got, err := r.Query().Monitor(ctx, strconv.FormatInt(tt.betaMonitor.ID, 10))
	require.NoError(t, err)
	assert.Nil(t, got)

	own, err := r.Query().Monitor(ctx, strconv.FormatInt(tt.acmeMonitor.ID, 10))
	require.NoError(t, err)
	require.NotNil(t, own)
	view.Monitor = own

	// Monitor.rules and Monitor.channels: resolved per field, not per request.
	rules, err := r.Monitor().Rules(ctx, own)
	require.NoError(t, err)
	view.Rules = rules
	channels, err := r.Monitor().Channels(ctx, own)
	require.NoError(t, err)
	view.Channels = channels

	// alerts: the collection is scoped by the store, and each alert's
	// inhibited rule is resolved again on its own.
	alerts, err := (&alertQuery{}).run(ctx, r)
	require.NoError(t, err)
	for _, edge := range alerts.Edges {
		view.Alerts = append(view.Alerts, *edge.Node)
	}

	// The foreign alert is not in acme's list, but ask the resolver to resolve
	// its inhibited rule anyway: this is the field-level leak a request-level
	// scope would miss.
	foreignAlert := &Alert{
		ID:                strconv.FormatInt(tt.betaAlert.ID, 10),
		InhibitedByRuleID: ptr(strconv.FormatInt(tt.betaRule.ID, 10)),
	}
	inhibited, err := r.Alert().InhibitedByRule(ctx, foreignAlert)
	require.NoError(t, err)
	assert.Nil(t, inhibited)
	view.Inhibited = inhibited

	stats, err := r.Query().Stats(ctx)
	require.NoError(t, err)
	view.Stats = stats

	series, err := r.Query().AlertSeries(ctx, nil)
	require.NoError(t, err)
	view.Series = series

	// Everything the query returned, serialised once: no beta marker may
	// appear anywhere in it.
	payload, err := json.Marshal(view)
	require.NoError(t, err)
	for _, marker := range []string{
		betaMonitorName,
		betaEventID,
		betaChannelName,
		betaWebhook,
	} {
		assert.NotContainsf(t, string(payload), marker, "workspace beta data leaked into an acme-scoped query")
	}

	// Per-field assertions on top of the serialised sweep, so a marker rename
	// cannot make that check vacuous.
	assert.NotEqual(t, strconv.FormatInt(tt.betaMonitor.ID, 10), view.Monitor.ID)
	page, err := (&monitorQuery{}).run(ctx, r)
	require.NoError(t, err)
	require.NotEmpty(t, page.Edges)
	for _, edge := range page.Edges {
		assert.NotEqual(t, strconv.FormatInt(tt.betaMonitor.ID, 10), edge.Node.ID,
			"another workspace's monitor must not appear in the page")
	}
	assert.Nil(t, inhibited, "a foreign rule id must not resolve")

	// The fixture is live, so the absence above is a scoping result rather
	// than an empty database.
	assert.Equal(t, "acme-payments", view.Monitor.Name)
	require.Len(t, view.Rules, 1)
	require.Len(t, view.Channels, 1)
	require.Len(t, view.Alerts, 1)
	assert.Equal(t, 1, view.Stats.Monitors)
	assert.NotZero(t, view.Stats.Alerts)

	t.Run("the same query scoped to beta returns beta's data", func(t *testing.T) {
		beta := betaCtx()
		monitor, err := r.Query().Monitor(beta, strconv.FormatInt(tt.betaMonitor.ID, 10))
		require.NoError(t, err)
		require.NotNil(t, monitor)
		assert.Equal(t, betaMonitorName, monitor.Name)

		betaRules, err := r.Monitor().Rules(beta, monitor)
		require.NoError(t, err)
		require.Len(t, betaRules, 1)
		assert.Contains(t, string(betaRules[0].Params), betaRuleMarker)

		betaAlerts, err := (&alertQuery{}).run(beta, r)
		require.NoError(t, err)
		require.Len(t, betaAlerts.Edges, 1)
		assert.Equal(t, betaEventID, betaAlerts.Edges[0].Node.EventID)

		// And the foreign rule id resolves there.
		foreign := &Alert{InhibitedByRuleID: ptr(strconv.FormatInt(tt.betaRule.ID, 10))}
		rule, err := r.Alert().InhibitedByRule(beta, foreign)
		require.NoError(t, err)
		require.NotNil(t, rule)
		assert.Equal(t, strconv.FormatInt(tt.betaRule.ID, 10), rule.ID)
	})

	t.Run("a context with no workspace sees the default workspace, never beta", func(t *testing.T) {
		bg := context.Background()
		conn, err := (&monitorQuery{}).run(bg, r)
		require.NoError(t, err)
		assert.Empty(t, conn.Edges)
		monitor, err := r.Query().Monitor(bg, strconv.FormatInt(tt.betaMonitor.ID, 10))
		assert.NoError(t, err)
		assert.Nil(t, monitor)
	})
}

// --- error shape ------------------------------------------------------------

// requireGraphQLError asserts the executor would emit an errors entry rather
// than a transport-level failure. gqlgen serialises *gqlerror.Error into the
// response's errors array; a plain error from a resolver still produces an
// entry, but the typed assertion is what pins the package's own contract.
func requireGraphQLError(t *testing.T, err error) *gqlerror.Error {
	t.Helper()
	require.Error(t, err)
	var ge *gqlerror.Error
	require.Truef(t, errors.As(err, &ge), "resolver error must be a *gqlerror.Error, got %T: %v", err, err)
	return ge
}

// errStoreClosed is the sentinel every failing-store message carries, so the
// error-shape tests also assert that the underlying cause survives into the
// GraphQL error a client sees.
var errStoreClosed = errors.New("store closed")

// failingStore is a store whose resolver-visible reads all fail. Embedding the
// interface means it satisfies store.Store without a full fake.
type failingStore struct{ store.Store }

func (failingStore) GetMonitor(context.Context, int64) (*store.Monitor, error) {
	return nil, errStoreClosed
}

func (failingStore) ListMonitorsPage(context.Context, store.ListFilter) ([]store.Monitor, error) {
	return nil, errStoreClosed
}

func (failingStore) GetAlert(context.Context, int64) (*store.Alert, error) {
	return nil, errStoreClosed
}

func (failingStore) ListAlerts(context.Context, store.AlertFilter) ([]store.Alert, error) {
	return nil, errStoreClosed
}

func (failingStore) GetStats(context.Context) (store.Stats, error) {
	return store.Stats{}, errStoreClosed
}

func (failingStore) AlertCountsByDay(context.Context, int) ([]store.AlertDayCount, error) {
	return nil, errStoreClosed
}

func (failingStore) ListRules(context.Context, int64, bool) ([]store.Rule, error) {
	return nil, errStoreClosed
}

func (failingStore) GetRule(context.Context, int64) (*store.Rule, error) {
	return nil, errStoreClosed
}

func (failingStore) ListChannelsForMonitor(context.Context, int64) ([]store.Channel, error) {
	return nil, errStoreClosed
}

func TestQueryErrorsAreGraphQLErrors(t *testing.T) {
	r := NewResolver(failingStore{}, discardLogger())
	ctx := context.Background()

	cases := []struct {
		name string
		call func() error
		want string
	}{
		{
			name: "monitor",
			call: func() error {
				_, err := r.Query().Monitor(ctx, "1")
				return err
			},
			want: "monitor not found",
		},
		{
			name: "monitors",
			call: func() error {
				_, err := (&monitorQuery{}).run(ctx, r)
				return err
			},
			want: "failed to list monitors",
		},
		{
			name: "alert",
			call: func() error {
				_, err := r.Query().Alert(ctx, "1")
				return err
			},
			want: "alert not found",
		},
		{
			name: "alerts",
			call: func() error {
				_, err := (&alertQuery{}).run(ctx, r)
				return err
			},
			want: "failed to list alerts",
		},
		{
			name: "stats",
			call: func() error {
				_, err := r.Query().Stats(ctx)
				return err
			},
			want: "failed to get stats",
		},
		{
			name: "alertSeries",
			call: func() error {
				_, err := r.Query().AlertSeries(ctx, nil)
				return err
			},
			want: "failed to get alert series",
		},
		{
			name: "rules",
			call: func() error {
				_, err := r.Monitor().Rules(ctx, &Monitor{ID: "1"})
				return err
			},
			want: "failed to list rules",
		},
		{
			name: "channels",
			call: func() error {
				_, err := r.Monitor().Channels(ctx, &Monitor{ID: "1"})
				return err
			},
			want: "failed to list channels",
		},
		{
			name: "inhibitedByRule",
			call: func() error {
				_, err := r.Alert().InhibitedByRule(ctx, &Alert{InhibitedByRuleID: ptr("1")})
				return err
			},
			want: "failed to get inhibited rule",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ge := requireGraphQLError(t, tc.call())
			assert.Contains(t, ge.Message, tc.want)
			assert.Contains(t, ge.Message, errStoreClosed.Error(), "the underlying cause must survive into the GraphQL error")
			assert.NotEmpty(t, ge.Extensions["code"], "error must carry a machine-readable code")
		})
	}
}

func TestInvalidIDsAreGraphQLErrors(t *testing.T) {
	r := NewResolver(failingStore{}, discardLogger())
	ctx := context.Background()

	cases := []struct {
		name string
		call func() error
		want string
	}{
		{
			name: "monitor id is not numeric",
			call: func() error { _, err := r.Query().Monitor(ctx, "abc"); return err },
			want: "invalid monitor ID",
		},
		{
			name: "alert id is not numeric",
			call: func() error { _, err := r.Query().Alert(ctx, "abc"); return err },
			want: "invalid alert ID",
		},
		{
			name: "monitor id on a nested resolver is not numeric",
			call: func() error { _, err := r.Monitor().Rules(ctx, &Monitor{ID: "abc"}); return err },
			want: "invalid monitor ID",
		},
		{
			name: "channels monitor id is not numeric",
			call: func() error { _, err := r.Monitor().Channels(ctx, &Monitor{ID: "abc"}); return err },
			want: "invalid monitor ID",
		},
		{
			name: "inhibited rule id is not numeric",
			call: func() error {
				_, err := r.Alert().InhibitedByRule(ctx, &Alert{InhibitedByRuleID: ptr("abc")})
				return err
			},
			want: "invalid inhibited rule ID",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ge := requireGraphQLError(t, tc.call())
			assert.Contains(t, ge.Message, tc.want)
			assert.Equal(t, "INVALID_ARGUMENT", ge.Extensions["code"])
		})
	}
}

// TestMissingRowMatchesSchemaNullability pins the interaction between the
// resolver's nil return and the schema's nullability: a nullable root field
// returns null with no error, which is what tells a client "not found" rather
// than "something broke".
func TestMissingRowMatchesSchemaNullability(t *testing.T) {
	schema := loadSchema(t)
	require.False(t, schema.Query.Fields.ForName("monitor").Type.NonNull,
		"monitor is declared nullable so a missing row can be null rather than an error")
	require.False(t, schema.Query.Fields.ForName("alert").Type.NonNull,
		"alert is declared nullable so a missing row can be null rather than an error")
	require.True(t, schema.Query.Fields.ForName("monitors").Type.NonNull)
	require.True(t, schema.Query.Fields.ForName("stats").Type.NonNull)
	require.True(t, schema.Query.Fields.ForName("alertSeries").Type.NonNull)

	r, _ := newTestResolver(t)
	ctx := acmeCtx()

	monitor, err := r.Query().Monitor(ctx, "999999")
	assert.NoError(t, err, "a nullable field resolves null, not an error")
	assert.Nil(t, monitor)

	alert, err := r.Query().Alert(ctx, "999999")
	assert.NoError(t, err)
	assert.Nil(t, alert)
}

// --- channel config ---------------------------------------------------------

func TestChannelConfigNeverAppearsInAResponse(t *testing.T) {
	r, st := newTestResolver(t)
	tt := seedTenants(t, st)
	ctx := acmeCtx()

	// Every path that can reach a channel: the monitor's channel list and the
	// raw conversion. A future query field would still have to go through one
	// of them.
	channels, err := r.Monitor().Channels(ctx, toMonitor(tt.acmeMonitor))
	require.NoError(t, err)
	require.Len(t, channels, 1)

	converted := toChannel(tt.acmeChannel)
	require.NotNil(t, converted)

	for name, value := range map[string]any{"Monitor.channels": channels, "toChannel": converted} {
		payload, err := json.Marshal(value)
		require.NoError(t, err)
		assert.NotContainsf(t, string(payload), acmeWebhook, "%s returned channel config", name)
		assert.NotContainsf(t, string(payload), "webhook_url", "%s returned a config key", name)
	}
}

// --- N+1 measurement --------------------------------------------------------

// countingStore counts the store calls each resolver field makes. Embedding the
// interface means a method added to store.Store does not break this fake; the
// ones the resolver calls are the ones overridden.
type countingStore struct {
	store.Store
	mu    sync.Mutex
	calls map[string]int
}

func newCountingStore(inner store.Store) *countingStore {
	return &countingStore{Store: inner, calls: make(map[string]int)}
}

func (c *countingStore) record(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[name]++
}

func (c *countingStore) count(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[name]
}

func (c *countingStore) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.calls {
		n += v
	}
	return n
}

func (c *countingStore) GetMonitor(ctx context.Context, id int64) (*store.Monitor, error) {
	c.record("GetMonitor")
	return c.Store.GetMonitor(ctx, id)
}

func (c *countingStore) ListMonitorsPage(ctx context.Context, f store.ListFilter) ([]store.Monitor, error) {
	c.record("ListMonitorsPage")
	return c.Store.ListMonitorsPage(ctx, f)
}

func (c *countingStore) GetAlert(ctx context.Context, id int64) (*store.Alert, error) {
	c.record("GetAlert")
	return c.Store.GetAlert(ctx, id)
}

func (c *countingStore) ListAlerts(ctx context.Context, f store.AlertFilter) ([]store.Alert, error) {
	c.record("ListAlerts")
	return c.Store.ListAlerts(ctx, f)
}

func (c *countingStore) GetStats(ctx context.Context) (store.Stats, error) {
	c.record("GetStats")
	return c.Store.GetStats(ctx)
}

func (c *countingStore) AlertCountsByDay(ctx context.Context, days int) ([]store.AlertDayCount, error) {
	c.record("AlertCountsByDay")
	return c.Store.AlertCountsByDay(ctx, days)
}

func (c *countingStore) ListRules(ctx context.Context, monitorID int64, enabledOnly bool) ([]store.Rule, error) {
	c.record("ListRules")
	return c.Store.ListRules(ctx, monitorID, enabledOnly)
}

func (c *countingStore) GetRule(ctx context.Context, id int64) (*store.Rule, error) {
	c.record("GetRule")
	return c.Store.GetRule(ctx, id)
}

func (c *countingStore) ListChannelsForMonitor(ctx context.Context, monitorID int64) ([]store.Channel, error) {
	c.record("ListChannelsForMonitor")
	return c.Store.ListChannelsForMonitor(ctx, monitorID)
}

// TestNestedListsAreNPlusOne measures the cost documented in
// docs/reference/graphql.md: the resolver holds no DataLoader and no
// per-request cache, so each nested list is one store query per parent row.
// The assertion records today's count so a future batching change is a
// deliberate edit here, not a silent improvement or regression.
func TestNestedListsAreNPlusOne(t *testing.T) {
	_, st := newTestResolver(t)
	tt := seedTenants(t, st)
	ctx := acmeCtx()

	const monitorCount = 3
	monitors := seedMonitors(t, st, ctx, "nplusone", monitorCount)
	for i := range monitors {
		rule := &store.Rule{
			MonitorID: monitors[i].ID, Type: "event_emitted",
			Params: json.RawMessage(`{"event_name":"transfer"}`), Enabled: true,
			Severity: store.SeverityWarning,
		}
		require.NoError(t, st.CreateRule(ctx, rule))
		channel := &store.Channel{Name: fmt.Sprintf("chan-%d", i), Type: "slack", Config: json.RawMessage(`{}`), Enabled: true}
		require.NoError(t, st.CreateChannel(ctx, channel))
		require.NoError(t, st.SetMonitorChannels(ctx, monitors[i].ID, []int64{channel.ID}))
	}

	// The fixture's own monitor has neither a rule nor a channel, so ask for
	// exactly the page of seeded ones.
	cs := newCountingStore(st)
	r := NewResolver(cs, discardLogger())

	conn, err := (&monitorQuery{limit: ptr(monitorCount), query: ptr("nplusone-")}).run(ctx, r)
	require.NoError(t, err)
	require.Len(t, conn.Edges, monitorCount)

	assert.Equal(t, 1, cs.count("ListMonitorsPage"), "one query resolves the whole page")
	assert.Equal(t, 0, cs.count("ListRules"))

	// This is what an executor does for `monitors { edges { node { rules channels } } }`:
	// resolve the nested field once per parent node.
	for _, edge := range conn.Edges {
		_, err := r.Monitor().Rules(ctx, edge.Node)
		require.NoError(t, err)
		_, err = r.Monitor().Channels(ctx, edge.Node)
		require.NoError(t, err)
	}

	assert.Equal(t, monitorCount, cs.count("ListRules"),
		"one ListRules per monitor: the rules field is N+1, not batched")
	assert.Equal(t, monitorCount, cs.count("ListChannelsForMonitor"),
		"one ListChannelsForMonitor per monitor: channels are N+1 too")
	assert.Equal(t, 1+2*monitorCount, cs.total(),
		"a %d-monitor page asking for rules and channels costs %d queries", monitorCount, 1+2*monitorCount)

	t.Run("inhibitedByRule is one rule lookup per alert", func(t *testing.T) {
		// Every alert on acme's monitor points at the same inhibiting rule. A
		// batching resolver would fetch it once; today it is fetched per row.
		seedAlerts(t, st, ctx, tt.acmeMonitor.ID, tt.acmeRule.ID, "inhibit", 3)
		alerts, err := st.ListAlerts(ctx, store.AlertFilter{MonitorID: tt.acmeMonitor.ID, Limit: 50})
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(alerts), 4)
		for i := range alerts {
			require.NoError(t, st.MarkAlertInhibited(ctx, alerts[i].ID, tt.acmeRule.ID))
		}

		before := cs.count("GetRule")
		conn, err := (&alertQuery{monitorID: ptr(strconv.FormatInt(tt.acmeMonitor.ID, 10))}).run(ctx, r)
		require.NoError(t, err)
		require.NotEmpty(t, conn.Edges)
		for _, edge := range conn.Edges {
			require.NotNil(t, edge.Node.InhibitedByRuleID, "the fixture marks every alert as inhibited")
			rule, err := r.Alert().InhibitedByRule(ctx, edge.Node)
			require.NoError(t, err)
			require.NotNil(t, rule)
		}
		assert.Equal(t, len(conn.Edges), cs.count("GetRule")-before,
			"one GetRule per alert with an inhibiting rule: N+1")
	})
}

// --- scalars and enums ------------------------------------------------------

func TestDateTimeScalar(t *testing.T) {
	at := time.Date(2026, 10, 19, 9, 35, 4, 0, time.UTC)

	var buf strings.Builder
	DateTime{Time: at}.MarshalGQL(&buf)
	assert.Equal(t, `"2026-10-19T09:35:04Z"`, buf.String())

	var d DateTime
	require.NoError(t, d.UnmarshalGQL("2026-10-19T09:35:04Z"))
	assert.True(t, at.Equal(d.Time))

	require.NoError(t, d.UnmarshalGQL("2026-10-19T09:35:04.500Z"), "fractional seconds are RFC 3339")

	cases := []struct {
		name  string
		value any
	}{
		{name: "not a string", value: 42},
		{name: "not RFC 3339", value: "2026-10-19"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var d DateTime
			assert.Error(t, d.UnmarshalGQL(tc.value))
		})
	}
}

func TestJSONScalar(t *testing.T) {
	var buf strings.Builder
	JSON(`{"event_name":"transfer"}`).MarshalGQL(&buf)
	assert.Equal(t, `{"event_name":"transfer"}`, buf.String())

	buf.Reset()
	JSON(nil).MarshalGQL(&buf)
	assert.Equal(t, "null", buf.String())

	var j JSON
	require.NoError(t, j.UnmarshalGQL(map[string]any{"event_name": "transfer"}))
	assert.JSONEq(t, `{"event_name":"transfer"}`, string(j))
}

// TestEnumConstantsMatchSchema keeps the hand-written enum constants in step
// with the schema the executor will marshal them against. A value added to the
// schema without a constant — or a constant that is not in the schema — fails
// here rather than at serialisation time in production.
func TestEnumConstantsMatchSchema(t *testing.T) {
	schema := loadSchema(t)

	cases := map[string][]string{
		"Priority": {
			string(PriorityLow), string(PriorityNormal), string(PriorityHigh),
		},
		"Severity": {
			string(SeverityInfo), string(SeverityWarning), string(SeverityCritical),
		},
		"MonitorSort": {
			string(MonitorSortName), string(MonitorSortID), string(MonitorSortCreatedAt),
		},
		"AlertSort": {
			string(AlertSortCreatedAtDesc), string(AlertSortCreatedAtAsc),
		},
	}
	for name, constants := range cases {
		t.Run(name, func(t *testing.T) {
			def := schema.Types[name]
			require.NotNilf(t, def, "schema.graphqls must declare enum %s", name)
			var declared []string
			for _, v := range def.EnumValues {
				declared = append(declared, v.Name)
			}
			assert.ElementsMatch(t, declared, constants)
		})
	}
}

func TestSeverityAndPriorityMapping(t *testing.T) {
	severities := []struct {
		stored store.Severity
		want   Severity
	}{
		{store.SeverityInfo, SeverityInfo},
		{store.SeverityWarning, SeverityWarning},
		{store.SeverityCritical, SeverityCritical},
		{"", SeverityWarning},
	}
	for _, tc := range severities {
		assert.Equalf(t, tc.want, toSeverity(tc.stored), "store severity %q", tc.stored)
	}

	priorities := []struct {
		stored store.Priority
		want   Priority
	}{
		{store.PriorityLow, PriorityLow},
		{store.PriorityNormal, PriorityNormal},
		{store.PriorityHigh, PriorityHigh},
		{"", PriorityNormal},
	}
	for _, tc := range priorities {
		assert.Equalf(t, tc.want, toPriority(tc.stored), "store priority %q", tc.stored)
	}
}

// --- handler contract -------------------------------------------------------

// The tests below characterise what /graphql actually does today. They exist
// because the HTTP surface is what a client uses, and because a test that
// asserts executed data would fail: NewHandler never calls the resolver (see
// docs/reference/graphql.md, findings 1, 3 and 4). When an executor lands,
// these tests must be rewritten to assert data and errors; until then they are
// the change detector that stops a half-wired executor shipping.

// fixedBody is the literal NewHandler writes for every request.
const fixedBody = `{"data":{"__schema":{"queryType":{"name":"Query"}}}}`
const fixedBodyJSON = fixedBody + "\n"

// panicStore fails loudly if the handler touches the resolver, which is the
// contract these tests pin.
type panicStore struct{ store.Store }

func (panicStore) GetMonitor(context.Context, int64) (*store.Monitor, error) {
	panic("graphql handler reached the store")
}

func (panicStore) ListMonitorsPage(context.Context, store.ListFilter) ([]store.Monitor, error) {
	panic("graphql handler reached the store")
}

func postGraphQL(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHandlerDoesNotExecuteQueries(t *testing.T) {
	r := NewResolver(panicStore{}, discardLogger())
	h := NewHandler(r, ServerConfig{MaxDepth: 1, MaxComplexity: 1})

	cases := []struct {
		name string
		body string
	}{
		{name: "a shallow query", body: `{"query":"{ stats { monitors } }"}`},
		{name: "a nested query beyond MaxDepth", body: `{"query":"{ monitors { edges { node { rules { id } channels { id } } } } }"}`},
		{name: "an introspection query", body: `{"query":"{ __schema { types { name } } }"}`},
		{name: "a mutation", body: `{"query":"mutation { nope }"}`},
		{name: "a malformed body", body: `not json at all`},
		{name: "an empty body", body: ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postGraphQL(t, h, tc.body)
			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			assert.Equal(t, fixedBodyJSON, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "types",
				"the body is a literal, not a schema dump: introspection is not served")
		})
	}

	t.Run("any method is accepted", func(t *testing.T) {
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			req := httptest.NewRequest(method, "/graphql", nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusOK, rec.Code, "%s must behave like POST today", method)
			assert.Equal(t, fixedBodyJSON, rec.Body.String())
		}
	})
}

// TestHandlerIgnoresQueryLimits records the third finding: the limits are
// parsed, validated and logged, but the handler never consults them. A nested
// query well past a MaxDepth of one is accepted. This is the test the issue
// asks for in place of a guard that does not exist.
func TestHandlerIgnoresQueryLimits(t *testing.T) {
	r := NewResolver(panicStore{}, discardLogger())
	h := NewHandler(r, ServerConfig{MaxDepth: 1, MaxComplexity: 1})

	deep := `{"query":"{ a { b { c { d { e { f { g { h { i { j { k } } } } } } } } } } }"}`
	rec := postGraphQL(t, h, deep)
	assert.Equal(t, http.StatusOK, rec.Code,
		"no depth limit is enforced today; if this fails, the executor landed and a real limit test is needed")
	assert.Equal(t, fixedBodyJSON, rec.Body.String())
}

func TestDefaultServerConfig(t *testing.T) {
	cfg := DefaultServerConfig()
	assert.False(t, cfg.EnablePlayground)
	assert.Equal(t, 10, cfg.MaxDepth)
	assert.Equal(t, 1000, cfg.MaxComplexity)
}

func TestPlaygroundRoute(t *testing.T) {
	r := NewResolver(panicStore{}, discardLogger())

	t.Run("disabled by default", func(t *testing.T) {
		mux := NewHandler(r, ServerConfig{})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/playground", nil))
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("served when enabled", func(t *testing.T) {
		mux := NewHandler(r, ServerConfig{EnablePlayground: true})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/playground", nil))
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Body.String(), "/graphql")
	})

	t.Run("PlaygroundHandler serves the same page", func(t *testing.T) {
		rec := httptest.NewRecorder()
		PlaygroundHandler("/graphql").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		assert.Equal(t, http.StatusOK, rec.Code)
	})
}
