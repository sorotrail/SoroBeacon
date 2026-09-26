package stellar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fixtures below are inline constants shaped exactly like the JSON the
// RPC returns (see types.go), so a decoder change that breaks the real
// contract breaks these tests too.

const fixtureEventJSON = `{
	"id": "0000000123456789-0000000000",
	"type": "contract",
	"ledger": 123456,
	"ledgerClosedAt": "2026-09-24T12:00:00Z",
	"contractId": "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA",
	"pagingToken": "0000000123456789-0000000000",
	"inSuccessfulContractCall": true,
	"txHash": "abc123",
	"topicJson": [{"symbol": "transfer"}, {"address": "GDW6AU3IL4V3R55QPGWQ7CQY6A2CUQAH5QNUU3GM6CUEUWM6KDOIJBH6"}],
	"valueJson": {"i128": "1000000000"}
}`

const fixtureGetEventsResult = `{
	"events": [` + fixtureEventJSON + `],
	"latestLedger": 123457,
	"oldestLedger": 123000,
	"cursor": "0000000123456789-0000000000"
}`

const fixtureGetLedgersResult = `{
	"ledgers": [
		{
			"hash": "aa",
			"sequence": 123456,
			"ledgerCloseTime": "2026-09-24T12:00:00Z"
		}
	],
	"latestLedger": 123457,
	"cursor": "next-page"
}`

const fixtureGetLatestLedgerResult = `{
	"id": "aa",
	"sequence": 123457,
	"protocolVersion": 22
}`

const fixtureGetHealthResult = `{
	"status": "healthy",
	"latestLedger": 123457,
	"oldestLedger": 123000,
	"ledgerRetentionWindow": 172800
}`

const fixtureGetNetworkResult = `{
	"passphrase": "Test SDF Network ; September 2015",
	"friendlierUrl": "https://stellar.org",
	"protocolVersion": 22
}`

const fixtureGetLedgerEntriesResult = `{
	"entries": [
		{
			"key": "key-xdr",
			"xdr": "data-xdr",
			"lastModifiedLedgerSeq": 123456,
			"liveUntilLedgerSeq": 130000
		}
	],
	"latestLedger": 123457
}`

// envelope builds a JSON-RPC 2.0 success envelope around a result fixture.
func envelope(result string) string {
	return `{"jsonrpc": "2.0", "id": 1, "result": ` + result + `}`
}

// errorEnvelope builds a JSON-RPC error body — the case most likely to be
// mishandled, because it arrives with HTTP 200 and a non-empty body that is
// not a result.
func errorEnvelope(code int, message string) string {
	return fmt.Sprintf(`{"jsonrpc": "2.0", "id": 1, "error": {"code": %d, "message": %q}}`, code, message)
}

// stubCall configures what the fake node answers for one JSON-RPC method.
// A nil body means the HTTP status is answered instead (when status is set).
type stubCall struct {
	mu     sync.Mutex
	status int
	body   string
}

func (s *stubCall) set(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.body = status, body
}

func (s *stubCall) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	status, body := s.status, s.body
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if status != 0 {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
		return
	}
	_, _ = w.Write([]byte(body))
}

// serveResult starts a fake RPC node that answers every method with the
// JSON-RPC envelope of result, and a client pointed at it.
func serveResult(t *testing.T, result string) (*HTTPClient, *stubCall) {
	t.Helper()
	return serveRaw(t, http.StatusOK, envelope(result))
}

// serveRaw serves body with the given status for every request.
func serveRaw(t *testing.T, status int, body string) (*HTTPClient, *stubCall) {
	t.Helper()
	stub := &stubCall{}
	stub.set(status, body)
	server := httptest.NewServer(http.HandlerFunc(stub.handle))
	t.Cleanup(server.Close)
	return NewHTTPClient(server.URL, server.Client()), stub
}

// TestHTTPClientSuccess decodes each method's fixture and asserts the fields
// the rest of SoroBeacon actually reads, so the RPC contract is pinned here.
func TestHTTPClientSuccess(t *testing.T) {
	t.Run("getEvents", func(t *testing.T) {
		client, _ := serveResult(t, fixtureGetEventsResult)

		got, err := client.GetEvents(context.Background(), GetEventsRequest{
			StartLedger: 123000,
			Filters:     []EventFilter{{Type: "contract", ContractIDs: []string{"CABC"}}},
		})
		require.NoError(t, err)
		require.Len(t, got.Events, 1)

		ev := got.Events[0]
		assert.Equal(t, "0000000123456789-0000000000", ev.ID)
		assert.Equal(t, "contract", ev.Type)
		assert.Equal(t, uint32(123456), ev.Ledger)
		assert.Equal(t, "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA", ev.ContractID)
		assert.Equal(t, "abc123", ev.TxHash)
		assert.True(t, ev.InSuccessfulContractCall)
		require.Len(t, ev.TopicJSON, 2)
		assert.JSONEq(t, `{"symbol": "transfer"}`, string(ev.TopicJSON[0]))
		assert.JSONEq(t, `{"i128": "1000000000"}`, string(ev.ValueJSON))
		assert.Equal(t, uint32(123457), got.LatestLedger)
		assert.Equal(t, uint32(123000), got.OldestLedger)
		assert.Equal(t, "0000000123456789-0000000000", got.Cursor)
	})

	t.Run("getLedgers", func(t *testing.T) {
		client, _ := serveResult(t, fixtureGetLedgersResult)

		got, err := client.GetLedgers(context.Background(), GetLedgersRequest{StartLedger: 123000})
		require.NoError(t, err)
		require.Len(t, got.Ledgers, 1)
		assert.Equal(t, "aa", got.Ledgers[0].Hash)
		assert.Equal(t, uint32(123456), got.Ledgers[0].Sequence)
		assert.Equal(t, uint32(123457), got.LatestLedger)
		assert.Equal(t, "next-page", got.Cursor)
	})

	t.Run("getLatestLedger", func(t *testing.T) {
		client, _ := serveResult(t, fixtureGetLatestLedgerResult)

		got, err := client.GetLatestLedger(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "aa", got.ID)
		assert.Equal(t, uint32(123457), got.Sequence)
		assert.Equal(t, 22, got.ProtocolVersion)
	})

	t.Run("getHealth", func(t *testing.T) {
		client, _ := serveResult(t, fixtureGetHealthResult)

		got, err := client.GetHealth(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "healthy", got.Status)
		assert.Equal(t, uint32(123457), got.LatestLedger)
		assert.Equal(t, uint32(123000), got.OldestLedger)
		assert.Equal(t, uint32(172800), got.LedgerRetentionWindow)
	})

	t.Run("getNetwork", func(t *testing.T) {
		client, _ := serveResult(t, fixtureGetNetworkResult)

		got, err := client.GetNetwork(context.Background())
		require.NoError(t, err)
		assert.Equal(t, PassphraseTestnet, got.Passphrase)
		assert.Equal(t, "https://stellar.org", got.FriendlierURL)
		assert.Equal(t, 22, got.ProtocolVersion)
	})

	t.Run("getLedgerEntries", func(t *testing.T) {
		client, _ := serveResult(t, fixtureGetLedgerEntriesResult)

		got, err := client.GetLedgerEntries(context.Background(), []string{"key-xdr"})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "key-xdr", got[0].KeyXDR)
		assert.Equal(t, "data-xdr", got[0].DataXDR)
		assert.Equal(t, uint32(123456), got[0].LastModifiedLedger)
		require.NotNil(t, got[0].LiveUntilLedgerSeq)
		assert.Equal(t, uint32(130000), *got[0].LiveUntilLedgerSeq)
	})
}

// A JSON-RPC error arrives with HTTP 200: the node answered, the request was
// bad. It must surface as an *RPCError, not be mistaken for a decode failure.
func TestHTTPClientRPCError(t *testing.T) {
	tests := []struct {
		name    string
		code    int
		message string
	}{
		{"invalid params", -32602, "invalid params"},
		{"method not found", -32601, "method not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, _ := serveRaw(t, http.StatusOK, errorEnvelope(tt.code, tt.message))

			_, err := client.GetLatestLedger(context.Background())
			require.Error(t, err)

			var rpcErr *RPCError
			require.True(t, errors.As(err, &rpcErr), "expected an *RPCError, got %v", err)
			assert.Equal(t, tt.code, rpcErr.Code)
			assert.Equal(t, tt.message, rpcErr.Message)
			assert.Contains(t, err.Error(), fmt.Sprintf("rpc error %d", tt.code))
		})
	}
}

// Malformed JSON and a valid-JSON-but-wrong-shape body are distinct failure
// modes: the first fails envelope decoding, the second decodes the envelope
// but not the result into the response type. Both must be reported as decode
// errors naming the method, never as empty results.
func TestHTTPClientBadBody(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"malformed json", `{"jsonrpc": "2.0", "id": 1, "result": `},
		{"wrong shape", `{"jsonrpc": "2.0", "id": 1, "result": {"sequence": "not a number"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, _ := serveRaw(t, http.StatusOK, tt.body)

			_, err := client.GetLatestLedger(context.Background())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "getLatestLedger")
			assert.Contains(t, err.Error(), "decode")
		})
	}
}

// A non-200 answer must come back typed as *HTTPStatusError with the status
// and a truncated body, so the failover client can tell retryable statuses
// (429, 5xx) from a legitimate 4xx that every endpoint would answer alike.
func TestHTTPClientNon200(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantSub string
	}{
		{"rate limited", http.StatusTooManyRequests, "slow down", "unexpected status 429"},
		{"server error", http.StatusInternalServerError, "boom", "unexpected status 500"},
		{"not found", http.StatusNotFound, "<html>proxy error page</html>", "unexpected status 404"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, _ := serveRaw(t, tt.status, tt.body)

			_, err := client.GetLatestLedger(context.Background())
			require.Error(t, err)

			var statusErr *HTTPStatusError
			require.True(t, errors.As(err, &statusErr), "expected an *HTTPStatusError, got %v", err)
			assert.Equal(t, tt.status, statusErr.StatusCode)
			assert.Equal(t, "getLatestLedger", statusErr.Method)
			assert.Equal(t, tt.body, statusErr.Body, "a short body is echoed whole for the log line")
			assert.Contains(t, err.Error(), tt.wantSub)
		})
	}
}

// A long error body is truncated before it reaches the error, so a proxy's
// HTML error page cannot fill the log line.
func TestHTTPClientTruncatesLongStatusBody(t *testing.T) {
	client, _ := serveRaw(t, http.StatusInternalServerError, strings.Repeat("x", 500))

	_, err := client.GetLatestLedger(context.Background())
	require.Error(t, err)

	var statusErr *HTTPStatusError
	require.True(t, errors.As(err, &statusErr))
	assert.Len(t, statusErr.Body, 203) // 200 chars + "..."
}

// A cancelled context must bring the call back promptly with
// context.Canceled — an ingestion loop cancelled at shutdown can never be
// allowed to block on a dead endpoint.
func TestHTTPClientCancelledContext(t *testing.T) {
	// A node that never answers: the only way the call can return is the
	// context cancelling it.
	block := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	t.Cleanup(func() { close(block); server.Close() })
	client := NewHTTPClient(server.URL, server.Client())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.GetLatestLedger(ctx)
		done <- err
	}()

	// Give the request time to reach the server, then cancel mid-flight.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("GetLatestLedger did not return after the context was cancelled")
	}
}

// A response slower than the client's timeout must surface as a deadline
// error rather than hanging the poller behind a stalled node.
func TestHTTPClientSlowResponseTimesOut(t *testing.T) {
	block := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	t.Cleanup(func() { close(block); server.Close() })

	client := NewHTTPClient(server.URL, &http.Client{Timeout: 50 * time.Millisecond})

	start := time.Now()
	_, err := client.GetLatestLedger(context.Background())
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, elapsed, 5*time.Second, "the timeout must be enforced, not waited out")
}

// The client asks for xdrFormat "json" so topics/values arrive readable.
func TestHTTPClientRequestsJSONXDRFormat(t *testing.T) {
	var xdrFormat string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params GetEventsRequest `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		xdrFormat = req.Params.XDRFormat
		_, _ = w.Write([]byte(envelope(`{"events": [], "latestLedger": 1}`)))
	}))
	t.Cleanup(server.Close)

	client := NewHTTPClient(server.URL, nil)
	_, err := client.GetEvents(context.Background(), GetEventsRequest{})
	require.NoError(t, err)
	assert.Equal(t, "json", xdrFormat)
}

// Older RPC versions reject the xdrFormat param outright. The client must
// retry once without it and remember the downgrade, so a poller does not
// burn an extra round trip on every call.
func TestHTTPClientDowngradesWhenXDRFormatUnsupported(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	var xdrFormats []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests++
		var req struct {
			Params GetEventsRequest `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		xdrFormats = append(xdrFormats, req.Params.XDRFormat)

		if req.Params.XDRFormat == "json" {
			_, _ = w.Write([]byte(errorEnvelope(-32602, "xdrFormat not supported")))
			return
		}
		_, _ = w.Write([]byte(envelope(`{"events": [], "latestLedger": 1}`)))
	}))
	t.Cleanup(server.Close)

	client := NewHTTPClient(server.URL, nil)
	ctx := context.Background()

	// First call: rejected, retried without the param, and remembered.
	got, err := client.GetEvents(ctx, GetEventsRequest{})
	require.NoError(t, err)
	assert.Empty(t, got.Events)
	assert.Equal(t, []string{"json", ""}, xdrFormats)

	// Later calls skip straight to base64 mode.
	xdrFormats = nil
	_, err = client.GetEvents(ctx, GetEventsRequest{})
	require.NoError(t, err)
	assert.Equal(t, []string{""}, xdrFormats)
	assert.Equal(t, 3, requests)
}

// A -32602 about some other parameter must not trigger the downgrade: that
// error means the request itself is bad and retrying identically would only
// hide it.
func TestIsUnsupportedParam(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"xdrformat spelled camel case", &RPCError{Code: -32602, Message: "xdrFormat not supported"}, true},
		{"xdr_format spelled snake case", &RPCError{Code: -32602, Message: "unknown field xdr_format"}, true},
		{"message case is irrelevant", &RPCError{Code: -32602, Message: "XDRFORMAT is invalid"}, true},
		{"other invalid param", &RPCError{Code: -32602, Message: "invalid contract id"}, false},
		{"wrapped rpc error", fmt.Errorf("getEvents: %w", &RPCError{Code: -32602, Message: "xdrFormat unknown"}), true},
		{"http status error", &HTTPStatusError{Method: "getEvents", StatusCode: 500}, false},
		{"plain error", errors.New("xdrFormat rejected"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isUnsupportedParam(tt.err))
		})
	}
}

// The request the client actually sends must satisfy the RPC's constraints:
// a default page size when none was asked for, and no startLedger alongside a
// cursor (the cursor already encodes the position, and the RPC rejects both).
func TestHTTPClientGetEventsRequestShape(t *testing.T) {
	var mu sync.Mutex
	var params []GetEventsRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params GetEventsRequest `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		params = append(params, req.Params)
		mu.Unlock()
		_, _ = w.Write([]byte(envelope(`{"events": [], "latestLedger": 1}`)))
	}))
	t.Cleanup(server.Close)

	client := NewHTTPClient(server.URL, nil)
	ctx := context.Background()

	// No pagination: the default limit fills in, and xdrFormat rides along.
	_, err := client.GetEvents(ctx, GetEventsRequest{StartLedger: 100})
	require.NoError(t, err)

	// A cursor clears the ledger range.
	_, err = client.GetEvents(ctx, GetEventsRequest{
		Pagination:  &Pagination{Cursor: "tok", Limit: 7},
		StartLedger: 100,
		EndLedger:   200,
	})
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, params, 2)
	assert.Equal(t, DefaultEventsLimit, params[0].Pagination.Limit)
	assert.Equal(t, "json", params[0].XDRFormat)
	assert.Equal(t, "tok", params[1].Pagination.Cursor)
	assert.Equal(t, 7, params[1].Pagination.Limit)
	assert.Zero(t, params[1].StartLedger, "startLedger must be dropped when a cursor is set")
	assert.Zero(t, params[1].EndLedger)
}

// getLedgers gets the same cursor/ledger-range treatment as getEvents.
func TestHTTPClientGetLedgersRequestShape(t *testing.T) {
	var mu sync.Mutex
	var params []GetLedgersRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params GetLedgersRequest `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		params = append(params, req.Params)
		mu.Unlock()
		_, _ = w.Write([]byte(envelope(fixtureGetLedgersResult)))
	}))
	t.Cleanup(server.Close)

	client := NewHTTPClient(server.URL, nil)
	ctx := context.Background()

	_, err := client.GetLedgers(ctx, GetLedgersRequest{})
	require.NoError(t, err)
	_, err = client.GetLedgers(ctx, GetLedgersRequest{
		Pagination:  &Pagination{Cursor: "tok", Limit: 5},
		StartLedger: 100,
	})
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, params, 2)
	assert.Equal(t, DefaultLedgersLimit, params[0].Pagination.Limit)
	assert.Equal(t, "tok", params[1].Pagination.Cursor)
	assert.Equal(t, 5, params[1].Pagination.Limit)
	assert.Zero(t, params[1].StartLedger, "startLedger must be dropped when a cursor is set")
}

// The client issues JSON-RPC 2.0 POSTs with the method it was asked for; a
// wrong URL makes that fail at the transport layer with the method named.
func TestHTTPClientReportsMethodOnTransportError(t *testing.T) {
	client := NewHTTPClient("http://127.0.0.1:1", &http.Client{Timeout: 2 * time.Second})

	_, err := client.GetLatestLedger(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "getLatestLedger")
}
