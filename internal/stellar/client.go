package stellar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Client is the surface SoroBeacon needs from a Stellar RPC node.
//
// Contributors: swap this out (e.g. for a streaming ingester or a mock in
// tests) by providing any implementation of these three methods.
type Client interface {
	GetEvents(ctx context.Context, req GetEventsRequest) (*GetEventsResult, error)
	GetLatestLedger(ctx context.Context) (*LatestLedger, error)
	GetHealth(ctx context.Context) (*Health, error)
	GetNetwork(ctx context.Context) (*Network, error)
}

// RPCError is a JSON-RPC 2.0 error object returned by the node.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// HTTPStatusError is a non-200 HTTP answer from an RPC endpoint. It is typed
// rather than a plain string error so the failover client can tell the
// endpoint's own failures (429, 5xx — worth trying elsewhere) from a
// legitimate 4xx result, which every endpoint would answer the same way and
// which therefore must not mark any of them unhealthy.
//
// Body is the truncated response body; RPC endpoints do not echo credentials,
// and the client already limits it to keep a proxy's HTML error page from
// filling the log line.
type HTTPStatusError struct {
	Method     string
	StatusCode int
	Body       string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("%s: unexpected status %d: %s", e.Method, e.StatusCode, e.Body)
}

// HTTPClient implements Client against a JSON-RPC 2.0 endpoint such as
// https://soroban-testnet.stellar.org.
type HTTPClient struct {
	url  string
	http *http.Client

	// jsonUnsupported flips to true the first time the node rejects
	// xdrFormat:"json", after which we stop asking and decode base64 XDR
	// ourselves (see Decoder).
	jsonUnsupported atomic.Bool
	reqID           atomic.Int64
}

// NewHTTPClient returns a Client for the given RPC URL. httpClient may be nil.
func NewHTTPClient(url string, httpClient *http.Client) *HTTPClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &HTTPClient{url: url, http: httpClient}
}

// GetEvents calls the getEvents method. It requests xdrFormat "json" so
// topics/values come back readable; if the node rejects that param it retries
// in base64 mode and remembers the downgrade for future calls.
func (c *HTTPClient) GetEvents(ctx context.Context, req GetEventsRequest) (*GetEventsResult, error) {
	if req.Pagination == nil || req.Pagination.Limit == 0 {
		limit := DefaultEventsLimit
		cursor := ""
		if req.Pagination != nil {
			cursor = req.Pagination.Cursor
		}
		req.Pagination = &Pagination{Cursor: cursor, Limit: limit}
	}
	// The RPC forbids startLedger alongside a cursor: the cursor already
	// encodes the position.
	if req.Pagination.Cursor != "" {
		req.StartLedger = 0
		req.EndLedger = 0
	}
	if !c.jsonUnsupported.Load() {
		req.XDRFormat = "json"
	} else {
		req.XDRFormat = ""
	}

	var res GetEventsResult
	err := c.call(ctx, "getEvents", req, &res)
	if err != nil && req.XDRFormat == "json" && isUnsupportedParam(err) {
		c.jsonUnsupported.Store(true)
		req.XDRFormat = ""
		err = c.call(ctx, "getEvents", req, &res)
	}
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// GetLedgers calls the getLedgers method, which returns ledger identity
// (hash, sequence) for a range. The poller uses it to track the hash of every
// recently ingested ledger so a reorg that rewrites one shows up as a changed
// hash rather than being silently ingested.
func (c *HTTPClient) GetLedgers(ctx context.Context, req GetLedgersRequest) (*GetLedgersResult, error) {
	if req.Pagination == nil || req.Pagination.Limit == 0 {
		cursor := ""
		if req.Pagination != nil {
			cursor = req.Pagination.Cursor
		}
		req.Pagination = &Pagination{Cursor: cursor, Limit: DefaultLedgersLimit}
	}
	if req.Pagination.Cursor != "" {
		req.StartLedger = 0
	}
	var res GetLedgersResult
	if err := c.call(ctx, "getLedgers", req, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (c *HTTPClient) GetLatestLedger(ctx context.Context) (*LatestLedger, error) {
	var res LatestLedger
	if err := c.call(ctx, "getLatestLedger", struct{}{}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (c *HTTPClient) GetHealth(ctx context.Context) (*Health, error) {
	var res Health
	if err := c.call(ctx, "getHealth", struct{}{}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (c *HTTPClient) GetNetwork(ctx context.Context) (*Network, error) {
	var res Network
	if err := c.call(ctx, "getNetwork", struct{}{}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// GetLedgerEntries calls getLedgerEntries with base64-encoded XDR LedgerKeys.
// It backs the spec fetcher, which reads a contract's instance and Wasm code.
// The RPC does not support xdrFormat here in the versions SoroBeacon targets,
// so entries always come back as base64 XDR.
func (c *HTTPClient) GetLedgerEntries(ctx context.Context, keys []string) ([]LedgerEntryResult, error) {
	var res struct {
		Entries      []LedgerEntryResult `json:"entries"`
		LatestLedger uint32              `json:"latestLedger"`
	}
	if err := c.call(ctx, "getLedgerEntries", struct {
		Keys []string `json:"keys"`
	}{Keys: keys}, &res); err != nil {
		return nil, err
	}
	return res.Entries, nil
}

func (c *HTTPClient) call(ctx context.Context, method string, params, result any) error {
	body, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int64  `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{"2.0", c.reqID.Add(1), method, params})
	if err != nil {
		return fmt.Errorf("marshal %s params: %w", method, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpRes, err := c.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	defer httpRes.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(httpRes.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("%s: read response: %w", method, err)
	}
	if httpRes.StatusCode != http.StatusOK {
		return &HTTPStatusError{Method: method, StatusCode: httpRes.StatusCode, Body: truncate(string(raw), 200)}
	}

	var envelope struct {
		Error  *RPCError       `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("%s: decode response: %w", method, err)
	}
	if envelope.Error != nil {
		return fmt.Errorf("%s: %w", method, envelope.Error)
	}
	if err := json.Unmarshal(envelope.Result, result); err != nil {
		return fmt.Errorf("%s: decode result: %w", method, err)
	}
	return nil
}

// isUnsupportedParam reports whether err looks like the node rejecting the
// xdrFormat param specifically (older RPC versions). -32602 alone is not
// enough: the node uses it for any invalid param (e.g. a bad contract ID),
// and those must surface instead of silently downgrading the format.
func isUnsupportedParam(err error) bool {
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		msg := strings.ToLower(rpcErr.Message)
		return strings.Contains(msg, "xdrformat") || strings.Contains(msg, "xdr_format")
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
