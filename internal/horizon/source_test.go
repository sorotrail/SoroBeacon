// Package horizon reads contract events from a Horizon server's REST API.
package horizon

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/sorotrail/sorobeacon/internal/poller"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// watch builds the poller's watch list from bare contract IDs.
func watch(ids ...string) []poller.Watch {
	out := make([]poller.Watch, 0, len(ids))
	for _, id := range ids {
		out = append(out, poller.Watch{ContractID: id})
	}
	return out
}

// makeSimpleEventMeta creates a minimal ContractEventFromMeta for testing.
// We construct the event fields directly to avoid XDR encoding issues in tests.
func makeSimpleEventMeta(contractID string, ledger uint32) ContractEventFromMeta {
	return ContractEventFromMeta{
		Event: &xdr.ContractEvent{
			ContractId: contractIDPtr(contractID),
			Type:       xdr.ContractEventTypeContract,
			Body: xdr.ContractEventBody{
				V: 0,
				V0: &xdr.ContractEventV0{
					Topics: []xdr.ScVal{{Type: xdr.ScValTypeScvSymbol, Sym: strPtr("transfer")}},
					Data:   xdr.ScVal{Type: xdr.ScValTypeScvVoid},
				},
			},
		},
		ContractID:     contractID,
		Ledger:         ledger,
		LedgerClosedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		TxHash:         "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
		TxIndex:        0,
		OpIndex:        0,
		EventIndex:     0,
	}
}

// fakeClient is a test implementation of the client interface for Source tests.
type fakeClient struct {
	transactionsFn func(ctx context.Context, contractID string, fromLedger uint32, cursor string, limit int) ([]ContractEventFromMeta, string, uint32, error)
	latestLedgerFn func(ctx context.Context) (uint32, error)
	healthFn       func(ctx context.Context) (*stellar.Health, error)
}

func (f *fakeClient) FetchTransactions(ctx context.Context, contractID string, fromLedger uint32, cursor string, limit int) ([]ContractEventFromMeta, string, uint32, error) {
	if f.transactionsFn != nil {
		return f.transactionsFn(ctx, contractID, fromLedger, cursor, limit)
	}
	return nil, "", 0, nil
}

func (f *fakeClient) GetLatestLedger(ctx context.Context) (uint32, error) {
	if f.latestLedgerFn != nil {
		return f.latestLedgerFn(ctx)
	}
	return 0, nil
}

func (f *fakeClient) GetHealth(ctx context.Context) (*stellar.Health, error) {
	if f.healthFn != nil {
		return f.healthFn(ctx)
	}
	return &stellar.Health{Status: "healthy"}, nil
}

func (f *fakeClient) OldestLedger(ctx context.Context) (uint32, error) {
	return 0, nil
}

// TestSourceFetchEvents tests basic event fetching and decoding via fake client.
func TestSourceFetchEvents(t *testing.T) {
	contractID := "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"

	callCount := 0
	fake := &fakeClient{
		transactionsFn: func(ctx context.Context, contractID string, fromLedger uint32, cursor string, limit int) ([]ContractEventFromMeta, string, uint32, error) {
			callCount++
			if callCount == 1 {
				return []ContractEventFromMeta{makeSimpleEventMeta(contractID, 150)}, "cursor-123", 150, nil
			}
			return []ContractEventFromMeta{}, "", 150, nil
		},
		latestLedgerFn: func(ctx context.Context) (uint32, error) { return 5000, nil },
	}

	src := NewSource(fake, stellar.DefaultDecoder{})

	page, err := src.FetchEvents(context.Background(), 100, watch(contractID), "", 50)
	require.NoError(t, err)
	require.Len(t, page.Events, 1)

	evDecoded := page.Events[0]
	assert.Equal(t, contractID, evDecoded.ContractID)
	assert.Equal(t, uint32(150), evDecoded.Ledger)
	assert.Equal(t, "transfer", evDecoded.EventName())

	// Cursor should be encoded (base64 of "0|cursor-123")
	assert.Equal(t, "MHxjdXJzb3ItMTIz", page.NextCursor)

	// Second call with cursor
	_, err = src.FetchEvents(context.Background(), 100, watch(contractID), page.NextCursor, 50)
	require.NoError(t, err)
	assert.Equal(t, 2, callCount)
}

// TestSourceLatestLedger tests the LatestLedger method.
func TestSourceLatestLedger(t *testing.T) {
	fake := &fakeClient{
		latestLedgerFn: func(ctx context.Context) (uint32, error) { return 200, nil },
	}
	src := NewSource(fake, stellar.DefaultDecoder{})
	latest, err := src.LatestLedger(context.Background())
	require.NoError(t, err)
	assert.Equal(t, uint32(200), latest)
}

// TestSourceCursorResumption tests that the cursor correctly encodes
// both the contract index and the Horizon cursor.
func TestSourceCursorResumption(t *testing.T) {
	contractA := "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"
	contractB := "CB7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDB"

	fake := &fakeClient{
		transactionsFn: func(ctx context.Context, contractID string, fromLedger uint32, cursor string, limit int) ([]ContractEventFromMeta, string, uint32, error) {
			return []ContractEventFromMeta{}, "horizon-cursor-5000", 5000, nil
		},
		latestLedgerFn: func(ctx context.Context) (uint32, error) { return 5000, nil },
	}

	src := NewSource(fake, stellar.DefaultDecoder{})

	// First page: contract A, no events, but has cursor
	page, err := src.FetchEvents(context.Background(), 100, watch(contractA, contractB), "", 50)
	require.NoError(t, err)

	// Cursor should encode contract index 0 and the horizon cursor
	decoded, _ := base64.RawURLEncoding.DecodeString(page.NextCursor)
	parts := strings.Split(string(decoded), "|")
	assert.Equal(t, "0", parts[0]) // contract index
	assert.Equal(t, "horizon-cursor-5000", parts[1]) // horizon cursor

	// Second call with cursor should continue on contract A
	_, err = src.FetchEvents(context.Background(), 100, watch(contractA, contractB), page.NextCursor, 50)
	require.NoError(t, err)
}

// TestSourceContractTransition tests moving to the next contract after one is drained.
func TestSourceContractTransition(t *testing.T) {
	contractA := "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"
	contractB := "CB7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDB"

	callCount := 0
	fake := &fakeClient{
		transactionsFn: func(ctx context.Context, contractID string, fromLedger uint32, cursor string, limit int) ([]ContractEventFromMeta, string, uint32, error) {
			callCount++
			if callCount == 1 && contractID == contractA {
				return []ContractEventFromMeta{makeSimpleEventMeta(contractA, 150)}, "", 150, nil
			}
			if callCount == 2 && contractID == contractB {
				return []ContractEventFromMeta{}, "", 150, nil
			}
			return nil, "", 0, fmt.Errorf("unexpected call: %d %s", callCount, contractID)
		},
		latestLedgerFn: func(ctx context.Context) (uint32, error) { return 150, nil },
	}

	src := NewSource(fake, stellar.DefaultDecoder{})

	// First call - contract A
	page1, err := src.FetchEvents(context.Background(), 100, watch(contractA, contractB), "", 50)
	require.NoError(t, err)
	require.Len(t, page1.Events, 1)
	assert.Equal(t, contractA, page1.Events[0].ContractID)

	// Cursor should point to contract B (index 1)
	decoded, _ := base64.RawURLEncoding.DecodeString(page1.NextCursor)
	parts := strings.Split(string(decoded), "|")
	assert.Equal(t, "1", parts[0])

	// Second call - contract B (drained immediately)
	page2, err := src.FetchEvents(context.Background(), 100, watch(contractA, contractB), page1.NextCursor, 50)
	require.NoError(t, err)
	assert.Empty(t, page2.Events)
	assert.Equal(t, "", page2.NextCursor)
}

// TestClient429Backoff tests that the client returns a proper error on 429.
func TestClient429Backoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Header().Set("Retry-After", "1")
		_, _ = w.Write([]byte(`{"error": "rate limited"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, &http.Client{Timeout: 5 * time.Second})
	_, _, _, err := c.FetchTransactions(context.Background(), "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA", 100, "", 50)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 429")
}

// TestClientSkipsFailedTransactions tests that failed transactions are skipped.
func TestClientSkipsFailedTransactions(t *testing.T) {
	contractID := "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"

	srv := &stubHorizon{
		rootJSON: `{"core_latest_ledger": 5000}`,
		transactionsJSON: `{
			"_embedded": {
				"records": [
					{
						"id": "tx-fail",
						"paging_token": "1000",
						"hash": "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
						"ledger": 150,
						"created_at": "2026-09-01T00:00:00Z",
						"source_account": "GABC...",
						"result_meta_xdr": "",
						"successful": false,
						"operation_count": 1
					}
				]
			},
			"_links": {}
		}`,
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	c := NewClient(ts.URL, nil)
	events, _, _, err := c.FetchTransactions(context.Background(), contractID, 100, "", 50)
	require.NoError(t, err)
	assert.Empty(t, events, "failed transactions should be skipped")
}

// TestClientSkipsTransactionsWithoutMeta tests that transactions without result_meta_xdr are skipped.
func TestClientSkipsTransactionsWithoutMeta(t *testing.T) {
	contractID := "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"

	srv := &stubHorizon{
		rootJSON: `{"core_latest_ledger": 5000}`,
		transactionsJSON: `{
			"_embedded": {
				"records": [
					{
						"id": "tx-no-meta",
						"paging_token": "1000",
						"hash": "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
						"ledger": 150,
						"created_at": "2026-09-01T00:00:00Z",
						"source_account": "GABC...",
						"result_meta_xdr": "",
						"successful": true,
"operation_count": 1
				}
			]
		},
		"_links": {}
		}`,
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	c := NewClient(ts.URL, nil)
	events, _, _, err := c.FetchTransactions(context.Background(), contractID, 100, "", 50)
	require.NoError(t, err)
	assert.Empty(t, events, "transactions without meta should be skipped")
}

// TestDecodeEvent tests the DecodeEvent function with a simple event.
func TestDecodeEvent(t *testing.T) {
	contractID := "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"

	eventFromMeta := makeSimpleEventMeta(contractID, 150)

	decoded, err := DecodeEvent(context.Background(), stellar.DefaultDecoder{}, eventFromMeta)
	require.NoError(t, err)
	assert.Equal(t, contractID, decoded.ContractID)
	assert.Equal(t, uint32(150), decoded.Ledger)
	assert.Equal(t, "transfer", decoded.EventName())
}

// TestSourceEmptyContractList tests behavior with empty watch list.
func TestSourceEmptyContractList(t *testing.T) {
	fake := &fakeClient{
		latestLedgerFn: func(ctx context.Context) (uint32, error) { return 5000, nil },
	}
	src := NewSource(fake, stellar.DefaultDecoder{})
	page, err := src.FetchEvents(context.Background(), 100, []poller.Watch{}, "", 50)
	require.NoError(t, err)
	assert.Empty(t, page.Events)
	assert.Equal(t, uint32(0), page.LatestLedger)
	assert.Equal(t, "", page.NextCursor)
}

// TestClientHealth tests the health endpoint.
func TestClientHealth(t *testing.T) {
	srv := &stubHorizon{rootJSON: `{"core_latest_ledger": 4200}`}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	c := NewClient(ts.URL, nil)
	h, err := c.GetHealth(context.Background())
	require.NoError(t, err)
	assert.Equal(t, uint32(4200), h.LatestLedger)
	assert.Equal(t, "healthy", h.Status)
}

// TestClientLatestLedger tests the GetLatestLedger method.
func TestClientLatestLedger(t *testing.T) {
	srv := &stubHorizon{rootJSON: `{"core_latest_ledger": 4200}`}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	c := NewClient(ts.URL, nil)
	latest, err := c.GetLatestLedger(context.Background())
	require.NoError(t, err)
	assert.Equal(t, uint32(4200), latest)
}

// stubHorizon serves scripted /accounts/{id}/transactions and / responses.
type stubHorizon struct {
	transactionsJSON string
	rootJSON         string
	lastQuery        string
	lastPath         string
}

func (s *stubHorizon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.lastPath = r.URL.Path
	s.lastQuery = r.URL.RawQuery
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/":
		_, _ = w.Write([]byte(s.rootJSON))
	case strings.HasPrefix(r.URL.Path, "/accounts/") && strings.HasSuffix(r.URL.Path, "/transactions"):
		_, _ = w.Write([]byte(s.transactionsJSON))
	default:
		http.NotFound(w, r)
	}
}

// Helper functions

func contractIDPtr(s string) *xdr.ContractId {
	// Use a simple contract ID from a known seed
	var raw [32]byte
	for i := range raw {
		raw[i] = 0xA1
	}
	copy(raw[:], []byte(s)[:32])
	var h xdr.Hash
	copy(h[:], raw[:])
	cid := xdr.ContractId(h)
	return &cid
}

func strPtr(s string) *xdr.ScSymbol {
	sym := xdr.ScSymbol(s)
	return &sym
}

// Compile-time interface checks.
var _ poller.EventSource = (*Source)(nil)
var _ poller.RetentionReporter = (*Source)(nil)