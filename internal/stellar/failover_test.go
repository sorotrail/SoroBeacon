package stellar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rpcStub is one fake RPC endpoint. It records the JSON-RPC methods it was
// asked for, so a test can assert which endpoint answered a call and how often
// each was touched, and it can be told to answer with a specific HTTP status.
type rpcStub struct {
	server *httptest.Server

	mu           sync.Mutex
	methods      []string
	status       int
	rpcErr       *RPCError
	passphrase   string
	latestLedger uint32
	closed       bool
}

func newRPCStub(t *testing.T) *rpcStub {
	t.Helper()
	s := &rpcStub{
		passphrase:   PassphraseTestnet,
		latestLedger: 7,
	}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

func (s *rpcStub) URL() string { return s.server.URL }

// Close shuts the endpoint down, so a request to it fails at the transport
// layer the way an unreachable node does. It is idempotent, because the test
// cleanup closes any stub a test did not close itself.
func (s *rpcStub) Close() {
	s.mu.Lock()
	closed := s.closed
	s.closed = true
	s.mu.Unlock()
	if !closed {
		s.server.Close()
	}
}

func (s *rpcStub) setStatus(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

func (s *rpcStub) setRPCError(err *RPCError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rpcErr = err
}

// Calls returns how many times method was requested.
func (s *rpcStub) Calls(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, m := range s.methods {
		if m == method {
			n++
		}
	}
	return n
}

func (s *rpcStub) Requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.methods)
}

func (s *rpcStub) handle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string `json:"method"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.methods = append(s.methods, req.Method)
	status, rpcErr, passphrase, ledger := s.status, s.rpcErr, s.passphrase, s.latestLedger
	s.mu.Unlock()

	if status != 0 {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"stub failure"}`))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if rpcErr != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": rpcErr})
		return
	}

	var result any
	switch req.Method {
	case "getLatestLedger":
		result = map[string]any{"sequence": ledger}
	case "getNetwork":
		result = map[string]any{"passphrase": passphrase}
	case "getHealth":
		result = map[string]any{"status": "healthy", "latestLedger": ledger}
	case "getLedgerEntries":
		result = map[string]any{"entries": []any{}, "latestLedger": ledger}
	default: // getEvents
		result = map[string]any{"events": []any{}, "latestLedger": ledger}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
}

// fakeClock drives the quarantine backoff without the tests sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newTestFailover returns a failover client over urls whose clock the test
// controls, so a quarantine can be aged out deterministically.
func newTestFailover(t *testing.T, urls ...string) (*FailoverClient, *fakeClock) {
	t.Helper()
	client := NewFailoverClient(urls, nil, slog.New(slog.DiscardHandler))
	clk := newFakeClock()
	client.now = clk.Now
	return client, clk
}

func TestFailoverClientUsesFirstHealthyEndpoint(t *testing.T) {
	primary := newRPCStub(t)
	fallback := newRPCStub(t)

	client, _ := newTestFailover(t, primary.URL(), fallback.URL())

	got, err := client.GetLatestLedger(context.Background())
	require.NoError(t, err)
	assert.Equal(t, uint32(7), got.Sequence)
	assert.Equal(t, 1, primary.Calls("getLatestLedger"))
	assert.Equal(t, 0, fallback.Requests(), "a healthy primary must not be bypassed")

	for _, s := range client.Stats() {
		assert.Zero(t, s.Failures)
		assert.False(t, s.Quarantined)
	}
}

func TestFailoverClientFailsOverOnRetryableStatus(t *testing.T) {
	for _, status := range []int{
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			primary := newRPCStub(t)
			primary.setStatus(status)
			fallback := newRPCStub(t)

			client, _ := newTestFailover(t, primary.URL(), fallback.URL())

			got, err := client.GetLatestLedger(context.Background())
			require.NoError(t, err, "a %d from the primary must fail over, not fail the call", status)
			assert.Equal(t, uint32(7), got.Sequence)
			assert.Equal(t, 1, primary.Calls("getLatestLedger"))
			assert.Equal(t, 1, fallback.Calls("getLatestLedger"))

			primaryStat := client.Stats()[0]
			assert.Equal(t, 1, primaryStat.Failures)
			assert.True(t, primaryStat.Quarantined)
			assert.False(t, client.Stats()[1].Quarantined)
		})
	}
}

func TestFailoverClientFailsOverOnTransportError(t *testing.T) {
	unreachable := newRPCStub(t)
	url := unreachable.URL()
	unreachable.Close() // every request now fails at the transport layer
	fallback := newRPCStub(t)

	client, _ := newTestFailover(t, url, fallback.URL())

	got, err := client.GetLatestLedger(context.Background())
	require.NoError(t, err)
	assert.Equal(t, uint32(7), got.Sequence)
	assert.True(t, client.Stats()[0].Quarantined)
	assert.Equal(t, 1, fallback.Calls("getLatestLedger"))
}

// A 4xx is the node rejecting the request, not the node failing. Every other
// endpoint would answer the same way, so the call must come back with the
// error instead of being replayed — and the endpoint must stay in rotation.
func TestFailoverClientDoesNotFailOverOnClientError(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			primary := newRPCStub(t)
			primary.setStatus(status)
			fallback := newRPCStub(t)

			client, _ := newTestFailover(t, primary.URL(), fallback.URL())

			_, err := client.GetLatestLedger(context.Background())
			require.Error(t, err)
			assert.Contains(t, err.Error(), fmt.Sprintf("unexpected status %d", status))
			assert.Equal(t, 1, primary.Calls("getLatestLedger"))
			assert.Equal(t, 0, fallback.Requests(), "a 4xx must not be replayed on another endpoint")

			stat := client.Stats()[0]
			assert.Zero(t, stat.Failures, "a legitimate 4xx must not count against the endpoint")
			assert.False(t, stat.Quarantined)
		})
	}
}

// A JSON-RPC error object arrives with HTTP 200: the node worked and told us
// the request was bad. Same reasoning as a 4xx.
func TestFailoverClientDoesNotFailOverOnRPCError(t *testing.T) {
	primary := newRPCStub(t)
	primary.setRPCError(&RPCError{Code: -32602, Message: "invalid params"})
	fallback := newRPCStub(t)

	client, _ := newTestFailover(t, primary.URL(), fallback.URL())

	_, err := client.GetLatestLedger(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rpc error -32602")
	assert.Equal(t, 0, fallback.Requests())

	stat := client.Stats()[0]
	assert.Zero(t, stat.Failures)
	assert.False(t, stat.Quarantined)
}

// A failed endpoint is parked, not retried on every call and not written off:
// once the backoff expires it is probed again and, if it answers, preferred
// again with a clean failure count.
func TestFailoverClientQuarantinesThenProbesForRecovery(t *testing.T) {
	flaky := newRPCStub(t)
	flaky.setStatus(http.StatusServiceUnavailable)
	steady := newRPCStub(t)

	client, clk := newTestFailover(t, flaky.URL(), steady.URL())
	ctx := context.Background()

	_, err := client.GetLatestLedger(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, flaky.Calls("getLatestLedger"), "the primary is tried first")
	require.True(t, client.Stats()[0].Quarantined)

	// While quarantined, calls go straight to the fallback.
	for i := 0; i < 3; i++ {
		_, err := client.GetLatestLedger(ctx)
		require.NoError(t, err)
	}
	assert.Equal(t, 1, flaky.Calls("getLatestLedger"),
		"a quarantined endpoint must not be retried on every call")
	assert.Equal(t, 4, steady.Calls("getLatestLedger"))

	// The backoff expires and the recovered primary wins the rotation again.
	flaky.setStatus(0)
	clk.Advance(failoverQuarantineBase)

	_, err = client.GetLatestLedger(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, flaky.Calls("getLatestLedger"), "the parked endpoint is probed again")
	assert.Equal(t, 4, steady.Calls("getLatestLedger"))

	stat := client.Stats()[0]
	assert.Zero(t, stat.Failures)
	assert.False(t, stat.Quarantined)
}

func TestFailoverClientBackoffGrowsWithConsecutiveFailures(t *testing.T) {
	failing := newRPCStub(t)
	failing.setStatus(http.StatusInternalServerError)
	steady := newRPCStub(t)

	client, clk := newTestFailover(t, failing.URL(), steady.URL())
	ctx := context.Background()

	for failures := 1; failures <= 3; failures++ {
		_, err := client.GetLatestLedger(ctx)
		require.NoError(t, err, "the steady endpoint keeps serving while the primary is parked")

		stat := client.Stats()[0]
		require.Equal(t, failures, stat.Failures)

		want := failoverQuarantineBase << (failures - 1)
		require.Equal(t, want, stat.RetryAt.Sub(clk.Now()),
			"each consecutive failure must double the quarantine")

		clk.Advance(want) // age the backoff out so the next call probes again
	}
	assert.Equal(t, 3, failing.Calls("getLatestLedger"))
}

func TestFailoverClientQuarantineIsCapped(t *testing.T) {
	failing := newRPCStub(t)
	failing.setStatus(http.StatusServiceUnavailable)
	steady := newRPCStub(t)

	client, clk := newTestFailover(t, failing.URL(), steady.URL())
	ctx := context.Background()

	for i := 0; i < 12; i++ {
		_, err := client.GetLatestLedger(ctx)
		require.NoError(t, err)
		clk.Advance(failoverQuarantineMax)
	}

	stat := client.Stats()[0]
	require.Equal(t, 12, stat.Failures)
	assert.LessOrEqual(t, stat.RetryAt.Sub(clk.Now()), failoverQuarantineMax,
		"the quarantine must stop doubling once it reaches the cap")
}

func TestFailoverClientReportsQuarantinedEndpoints(t *testing.T) {
	first := newRPCStub(t)
	first.setStatus(http.StatusInternalServerError)
	second := newRPCStub(t)
	second.setStatus(http.StatusTooManyRequests)

	client, _ := newTestFailover(t, first.URL(), second.URL())
	ctx := context.Background()

	_, err := client.GetLatestLedger(ctx)
	require.Error(t, err, "with every endpoint failing there is nothing left to fail over to")
	assert.Equal(t, 1, first.Calls("getLatestLedger"))
	assert.Equal(t, 1, second.Calls("getLatestLedger"))

	// Both are parked, so the next call is refused outright rather than
	// hammering them.
	_, err = client.GetLatestLedger(ctx)
	require.ErrorContains(t, err, "all 2 RPC endpoints are quarantined")
	assert.Equal(t, 1, first.Calls("getLatestLedger"))
	assert.Equal(t, 1, second.Calls("getLatestLedger"))
}

func TestFailoverClientDelegatesToHealthyEndpoint(t *testing.T) {
	primary := newRPCStub(t)
	client, _ := newTestFailover(t, primary.URL())
	ctx := context.Background()

	events, err := client.GetEvents(ctx, GetEventsRequest{})
	require.NoError(t, err)
	assert.Empty(t, events.Events)

	health, err := client.GetHealth(ctx)
	require.NoError(t, err)
	assert.Equal(t, "healthy", health.Status)

	network, err := client.GetNetwork(ctx)
	require.NoError(t, err)
	assert.Equal(t, PassphraseTestnet, network.Passphrase)

	entries, err := client.GetLedgerEntries(ctx, []string{"key"})
	require.NoError(t, err)
	assert.Empty(t, entries)

	assert.Equal(t, 4, primary.Requests())
}

func TestFailoverClientRespectsCancelledContext(t *testing.T) {
	primary := newRPCStub(t)
	client, _ := newTestFailover(t, primary.URL())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.GetLatestLedger(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 0, primary.Requests())
	assert.Zero(t, client.Stats()[0].Failures)
}

func TestFailoverClientEndpointsPreservePriorityOrder(t *testing.T) {
	first := newRPCStub(t)
	second := newRPCStub(t)
	client, _ := newTestFailover(t, first.URL(), second.URL())

	endpoints := client.Endpoints()
	require.Len(t, endpoints, 2)
	assert.Equal(t, first.URL(), endpoints[0].URL)
	assert.Equal(t, second.URL(), endpoints[1].URL)
}

func TestIsFailoverError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "transport error",
			err:  errors.New("getEvents: dial tcp 127.0.0.1:1: connect: connection refused"),
			want: true,
		},
		{
			name: "rate limited",
			err:  &HTTPStatusError{Method: "getEvents", StatusCode: http.StatusTooManyRequests},
			want: true,
		},
		{
			name: "server error",
			err:  &HTTPStatusError{Method: "getEvents", StatusCode: http.StatusInternalServerError},
			want: true,
		},
		{
			name: "wrapped server error",
			err:  fmt.Errorf("rpc endpoint https://a: %w", &HTTPStatusError{Method: "getEvents", StatusCode: http.StatusBadGateway}),
			want: true,
		},
		{
			name: "bad request",
			err:  &HTTPStatusError{Method: "getEvents", StatusCode: http.StatusBadRequest},
			want: false,
		},
		{
			name: "forbidden",
			err:  &HTTPStatusError{Method: "getEvents", StatusCode: http.StatusForbidden},
			want: false,
		},
		{
			name: "json-rpc error",
			err:  &RPCError{Code: -32602, Message: "invalid params"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isFailoverError(tt.err))
		})
	}
}
