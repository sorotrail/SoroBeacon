package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// TestReplicaUnavailable pins the one decision that separates "fall back" from
// "report the error": whether a failed replica query means the replica is gone
// or means the query itself is wrong. Both halves matter — falling back too
// eagerly hides a broken replica behind the primary, and falling back too
// rarely turns a replica blip into a failed request.
func TestReplicaUnavailable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "no error",
			err:  nil,
			want: false,
		},
		{
			name: "cancelled caller does not retry",
			err:  context.Canceled,
			want: false,
		},
		{
			name: "spent deadline does not retry",
			err:  context.DeadlineExceeded,
			want: false,
		},
		{
			name: "wrapped cancellation still does not retry",
			err:  fmt.Errorf("query alerts: %w", context.Canceled),
			want: false,
		},
		{
			// The replica answered, and the answer was an error. Replaying it
			// on the primary returns the same error and doubles the load.
			name: "server-side error is not a replica outage",
			err:  &pgconn.PgError{Code: "42P01", Message: `relation "alerts" does not exist`},
			want: false,
		},
		{
			name: "wrapped server-side error is not a replica outage",
			err:  fmt.Errorf("scan stats: %w", &pgconn.PgError{Code: "57014", Message: "statement timeout"}),
			want: false,
		},
		{
			name: "failed connection is a replica outage",
			err:  &pgconn.ConnectError{},
			want: true,
		},
		{
			name: "wrapped connection failure is a replica outage",
			err:  fmt.Errorf("acquire conn: %w", &pgconn.ConnectError{}),
			want: true,
		},
		{
			name: "closed pool is a replica outage",
			err:  errors.New("closed pool"),
			want: true,
		},
		{
			name: "dropped connection mid-query is a replica outage",
			err:  errors.New("unexpected EOF"),
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, replicaUnavailable(tc.err))
		})
	}
}

// TestConnectReplicaUnsetKeepsReadsOnPrimary is the off-by-default guarantee:
// a deployment that does not set REPLICA_DATABASE_URL ends up with no replica
// pool at all, so every query — read and write — runs on the primary exactly
// as it did before replica routing existed. It also covers the nil-metrics
// case, since PoolSettings here is the zero value.
func TestConnectReplicaUnsetKeepsReadsOnPrimary(t *testing.T) {
	p := &Postgres{}

	require.NoError(t, p.connectReplica(context.Background(), PoolSettings{}))

	assert.Nil(t, p.replica)
	assert.Equal(t, DefaultMaxReplicaLag, p.MaxReplicaLag())
}

// TestConnectReplicaRejectsUnparseableURL covers the read-replica half of the
// boot contract: a replica URL that cannot even be parsed is a startup error,
// not something to discover on the first read. The error names the replica so
// an operator does not go looking at DATABASE_URL.
func TestConnectReplicaRejectsUnparseableURL(t *testing.T) {
	p := &Postgres{}

	err := p.connectReplica(context.Background(), PoolSettings{ReplicaURL: "://not-a-url"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "postgres replica")
	assert.Nil(t, p.replica)
}

// TestOnlyReplicaRoutedMethodsUseReplicaHelpers mechanically verifies via
// go/ast that the set of functions in internal/store calling queryRows or
// queryRowFallback matches ReplicaRoutedMethods() in both directions, and that
// no method in PrimaryOnlyCriticalMethods() ever touches the replica (#572).
func TestOnlyReplicaRoutedMethodsUseReplicaHelpers(t *testing.T) {
	routed := ReplicaRoutedMethods()
	primaryOnly := PrimaryOnlyCriticalMethods()

	for fn, reason := range routed {
		assert.NotEmpty(t, strings.TrimSpace(reason), "ReplicaRoutedMethods entry %s must carry a justification", fn)
		_, conflict := primaryOnly[fn]
		assert.Falsef(t, conflict, "%s cannot appear in both ReplicaRoutedMethods and PrimaryOnlyCriticalMethods", fn)
	}

	fset := token.NewFileSet()
	actualCallers := make(map[string]bool)
	definedFuncs := make(map[string]bool)

	for _, file := range sqlSourceFiles(t) {
		fAST, err := parser.ParseFile(fset, file, nil, 0)
		require.NoError(t, err, "parse %s", file)

		for _, decl := range fAST.Decls {
			fnDecl, ok := decl.(*ast.FuncDecl)
			if !ok || fnDecl.Body == nil {
				continue
			}
			fnName := fnDecl.Name.Name
			definedFuncs[fnName] = true
			if file == "replica.go" {
				continue
			}
			ast.Inspect(fnDecl.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if sel.Sel.Name == "queryRows" || sel.Sel.Name == "queryRowFallback" {
					actualCallers[fnName] = true
				}
				return true
			})
		}
	}

	for fn := range actualCallers {
		_, allowed := routed[fn]
		assert.Truef(t, allowed,
			"store function %s calls queryRows/queryRowFallback but is not in ReplicaRoutedMethods(); default new methods to p.pool or add to ReplicaRoutedMethods with a reviewed reason",
			fn)
	}
	for fn := range routed {
		assert.Truef(t, actualCallers[fn],
			"ReplicaRoutedMethods() lists %s, which no longer calls queryRows or queryRowFallback",
			fn)
	}
	for fn, reason := range primaryOnly {
		assert.NotEmpty(t, strings.TrimSpace(reason))
		assert.Truef(t, definedFuncs[fn], "PrimaryOnlyCriticalMethods() lists %s, which is not defined in internal/store", fn)
		assert.Falsef(t, actualCallers[fn], "primary-only critical method %s must never call replica helpers", fn)
	}
}

func TestReplicaLagBoundAndMetrics(t *testing.T) {
	m := metrics.New()
	p := &Postgres{metrics: m}
	require.NoError(t, p.connectReplica(context.Background(), PoolSettings{
		MaxReplicaLag: 15 * time.Second,
	}))
	assert.Equal(t, 15*time.Second, p.MaxReplicaLag())
	assert.Equal(t, time.Duration(0), p.ObservedReplicaLag())
	assert.False(t, p.replicaLagExceeded())

	p.SetObservedReplicaLag(10 * time.Second)
	assert.Equal(t, 10*time.Second, p.ObservedReplicaLag())
	assert.False(t, p.replicaLagExceeded(), "lag within MaxReplicaLag must keep routing to the replica")

	p.SetObservedReplicaLag(20 * time.Second)
	assert.Equal(t, 20*time.Second, p.ObservedReplicaLag())
	assert.True(t, p.replicaLagExceeded(), "lag above MaxReplicaLag must trigger fallback to the primary")

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Contains(t, rec.Body.String(), "sorobeacon_store_replica_lag_seconds 20")
}

func TestPostgresReplicaRoutingFallbackLagAndReadAfterWrite(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres replica routing test")
	}
	require.NoError(t, Migrate(url))

	ctx := context.Background()
	m := metrics.New()
	st, err := NewPostgres(ctx, url, PoolSettings{
		ReplicaURL:    url,
		MaxReplicaLag: 10 * time.Second,
		Metrics:       m,
	})
	require.NoError(t, err)
	t.Cleanup(st.Close)
	require.NoError(t, st.resetConformance(ctx))

	// 1. SampleReplicaLag succeeds against Postgres and records a non-negative lag.
	lag, err := st.SampleReplicaLag(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, lag, time.Duration(0))

	// 2. Write followed immediately by its own read on primary-only methods.
	mon := &Monitor{Name: "primary-read-your-writes", ContractIDs: []string{"CCONTRACT1"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, mon))
	gotMon, err := st.GetMonitor(ctx, mon.ID)
	require.NoError(t, err)
	assert.Equal(t, mon.Name, gotMon.Name)

	rule := &Rule{MonitorID: mon.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, rule))

	a1 := &Alert{MonitorID: mon.ID, RuleID: rule.ID, EventID: "evt-1", Payload: json.RawMessage(`{}`)}
	_, err = st.CreateAlert(ctx, a1)
	require.NoError(t, err)
	a2 := &Alert{MonitorID: mon.ID, RuleID: rule.ID, EventID: "evt-2", Payload: json.RawMessage(`{}`)}
	_, err = st.CreateAlert(ctx, a2)
	require.NoError(t, err)

	gotAlert, err := st.GetAlert(ctx, a1.ID)
	require.NoError(t, err)
	assert.Equal(t, a1.EventID, gotAlert.EventID)

	// 3. Routed reads use the replica pool when healthy and within MaxReplicaLag.
	_, err = st.ListMonitorsPage(ctx, ListFilter{Limit: 10})
	require.NoError(t, err)
	page1, err := st.ListAlerts(ctx, AlertFilter{Limit: 1})
	require.NoError(t, err)
	require.Len(t, page1, 1)
	page2, err := st.ListAlerts(ctx, AlertFilter{Limit: 1, AfterID: page1[0].ID})
	require.NoError(t, err)
	require.Len(t, page2, 1)
	assert.NotEqual(t, page1[0].ID, page2[0].ID)

	// 4. Exceeding MaxReplicaLag routes subsequent reads to the primary and increments fallbacks.
	st.SetObservedReplicaLag(30 * time.Second)
	_, err = st.GetStats(ctx)
	require.NoError(t, err)

	// 5. Closing the replica pool falls back to the primary transparently, while Ping (readiness) stays green.
	st.SetObservedReplicaLag(0)
	st.replica.Close()
	alertsAfterClose, err := st.ListAlerts(ctx, AlertFilter{Limit: 10})
	require.NoError(t, err)
	assert.Len(t, alertsAfterClose, 2)
	require.NoError(t, st.Ping(ctx), "readiness Ping must succeed when only the replica is down")

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	assert.Contains(t, body, `sorobeacon_store_reads_total{pool="replica"}`)
	assert.Contains(t, body, `sorobeacon_store_reads_total{pool="primary"}`)
	assert.Contains(t, body, `sorobeacon_store_replica_fallbacks_total`)
}

// TestBuildAlertQueryIsParameterised guards the shared query builder that both
// ListAlerts and ListAlertsPrimary call. The filter values must reach Postgres
// as bind parameters in the order the placeholders appear: the values here are
// chosen to look like SQL so a builder that concatenated them would produce a
// statement that no longer matches the expected placeholders.
func TestBuildAlertQueryIsParameterised(t *testing.T) {
	f := AlertFilter{
		MonitorID:  7,
		RuleID:     9,
		ContractID: "C' OR 1=1 --",
		Limit:      25,
	}

	// System scope: the test pins the placeholder positions, and a tenant
	// predicate would prepend one and shift every assertion below.
	q, args := buildAlertQuery(workspace.WithSystem(context.Background()), f)

	require.Len(t, args, 4)
	assert.Equal(t, int64(7), args[0])
	assert.Equal(t, int64(9), args[1])
	assert.Equal(t, "C' OR 1=1 --", args[2])
	// The limit is the last placeholder, so the filter values cannot shift it.
	assert.Equal(t, 25, args[len(args)-1])
	assert.Contains(t, q, "$1")
	assert.Contains(t, q, "$4")
	assert.NotContains(t, q, "1=1", "a filter value reached the statement text")
	assert.Contains(t, q, "ORDER BY created_at DESC, id DESC")
}

// TestBuildAlertQueryAscendingCursorOrdersBothSides is the cursor property the
// builder's comment claims: the page after a cursor is read in the same
// direction as the first page, so paging cannot skip or repeat a row once two
// alerts share a timestamp.
func TestBuildAlertQueryAscendingCursorOrdersBothSides(t *testing.T) {
	q, args := buildAlertQuery(workspace.WithSystem(context.Background()), AlertFilter{Sort: "created_at_asc", AfterID: 42})

	require.Len(t, args, 2)
	assert.Equal(t, int64(42), args[0])
	assert.Contains(t, q, `(created_at, id) > (SELECT created_at, id FROM alerts WHERE id = $1)`)
	assert.Contains(t, q, "ORDER BY created_at ASC, id ASC")
}
