package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	q, args := buildAlertQuery(f)

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
	q, args := buildAlertQuery(AlertFilter{Sort: "created_at_asc", AfterID: 42})

	require.Len(t, args, 2)
	assert.Equal(t, int64(42), args[0])
	assert.Contains(t, q, `(created_at, id) > (SELECT created_at, id FROM alerts WHERE id = $1)`)
	assert.Contains(t, q, "ORDER BY created_at ASC, id ASC")
}
