// Package sorotrail reads contract events from a SoroTrail indexer's HTTP
// API, for SoroBeacon's upstream mode.
//
// SoroTrail indexes events durably, past the RPC's ~1-7 day retention
// window. An upstream-mode SoroBeacon monitors through it instead of
// polling the RPC itself, which also means several SoroBeacon instances
// can share one indexer rather than each hitting the chain.
//
// The events arrive already decoded (SoroTrail stores decoded topics and
// values), so this source performs no XDR work.
package sorotrail

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/sorotrail/sorobeacon/internal/poller"
	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// Client is a minimal SoroTrail API client: just the calls the source
// needs.
type Client struct {
	base string
	http *http.Client
}

// NewClient returns a client for a SoroTrail base URL, e.g.
// http://sorotrail:8080. The base URL carries no path; /api/v1/... is
// appended here.
func NewClient(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{base: baseURL, http: httpClient}
}

// stats is the slice of GET /api/v1/stats the source needs.
type stats struct {
	FirstLedger int64 `json:"first_ledger"`
	LastLedger  int64 `json:"last_ledger"`
}

// eventsResponse is GET /api/v1/events.
type eventsResponse struct {
	Events     []event `json:"events"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

// event is one SoroTrail event row. Topics and value arrive decoded, in
// the same single-key wrapper vocabulary the local decoder produces
// ({"symbol": "..."}, {"i128": "..."}), so they pass straight through to
// the rules engine.
type event struct {
	ID         string          `json:"id"`
	ContractID string          `json:"contract_id"`
	Ledger     int64           `json:"ledger"`
	TxHash     string          `json:"tx_hash"`
	Topics     json.RawMessage `json:"topics"`
	Value      json.RawMessage `json:"value"`
	ClosedAt   time.Time       `json:"ledger_closed_at"`
}

func (c *Client) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("sorotrail %s: %w", path, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("sorotrail %s: read response: %w", path, err)
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("sorotrail %s: status %d: %.200s", path, res.StatusCode, string(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("sorotrail %s: decode response: %w", path, err)
	}
	return nil
}

// Stats fetches the indexer's ledger coverage.
func (c *Client) Stats(ctx context.Context) (stats, error) {
	var s stats
	err := c.getJSON(ctx, "/api/v1/stats", nil, &s)
	return s, err
}

// Events fetches one page of events. contractID may be a comma-separated
// union (SoroTrail's multi-value contract_id filter). cursor continues a
// previous page; fromLedger applies only on the first page.
func (c *Client) Events(ctx context.Context, contractID string, fromLedger int64, cursor string, limit int) (eventsResponse, error) {
	q := url.Values{}
	if contractID != "" {
		q.Set("contract_id", contractID)
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	} else if fromLedger > 0 {
		q.Set("from_ledger", strconv.FormatInt(fromLedger, 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var res eventsResponse
	err := c.getJSON(ctx, "/api/v1/events", q, &res)
	return res, err
}

// Health adapts the indexer to the api.HealthChecker interface, so the
// /health and /readyz probes work identically in upstream mode.
func (c *Client) Health(ctx context.Context) (*stellar.Health, error) {
	s, err := c.Stats(ctx)
	if err != nil {
		return nil, err
	}
	return &stellar.Health{
		Status:       "healthy",
		LatestLedger: uint32(s.LastLedger),
	}, nil
}

// GetHealth satisfies api.HealthChecker.
func (c *Client) GetHealth(ctx context.Context) (*stellar.Health, error) {
	return c.Health(ctx)
}

// Source implements poller.EventSource against a SoroTrail indexer.
type Source struct {
	client *Client
}

// NewSource wires an upstream-mode event source.
func NewSource(c *Client) *Source {
	return &Source{client: c}
}

// LatestLedger reports the indexer's newest ingested ledger. A cold start
// against an upstream source therefore begins at the indexer's tip — not
// because history is unavailable (it is not; SoroTrail keeps it all), but
// because a fresh monitor has no reason to replay the past.
func (s *Source) LatestLedger(ctx context.Context) (uint32, error) {
	st, err := s.client.Stats(ctx)
	if err != nil {
		return 0, err
	}
	if st.LastLedger <= 0 {
		return 0, fmt.Errorf("sorotrail: indexer reports no ingested ledgers")
	}
	return uint32(st.LastLedger), nil
}

// FetchEvents pages the indexer's /events. SoroTrail paginates ascending by
// event ID with an opaque cursor, which passes through untouched: one
// request covers the whole contract union, so there is no batching state
// to encode.
func (s *Source) FetchEvents(ctx context.Context, startLedger uint32, contracts []string, cursor string, limit int) (poller.FetchPage, error) {
	if limit <= 0 {
		limit = 50
	}
	res, err := s.client.Events(ctx, joinComma(contracts), int64(startLedger), cursor, limit)
	if err != nil {
		return poller.FetchPage{}, err
	}

	page := poller.FetchPage{LatestLedger: s.tipFrom(res)}
	for _, e := range res.Events {
		de := stellar.DecodedEvent{
			ID:         e.ID,
			ContractID: e.ContractID,
			Ledger:     uint32(e.Ledger),
			TxHash:     e.TxHash,
			Topics:     []any{},
		}
		// Topics and value are already decoded JSON; unmarshal into the
		// generic shape the rules engine expects. A topic that fails to
		// unmarshal skips that event rather than failing the page — one
		// malformed row must not stall monitoring.
		if len(e.Topics) > 0 {
			if err := json.Unmarshal(e.Topics, &de.Topics); err != nil {
				continue
			}
		}
		if len(e.Value) > 0 {
			if err := json.Unmarshal(e.Value, &de.Value); err != nil {
				continue
			}
		}
		page.Events = append(page.Events, &de)
	}
	page.NextCursor = res.NextCursor
	return page, nil
}

// tipFrom derives a page's latest ledger from the newest event in it;
// SoroTrail's events response does not carry the indexer's tip separately.
func (s *Source) tipFrom(res eventsResponse) uint32 {
	var tip uint32
	for _, e := range res.Events {
		if uint32(e.Ledger) > tip {
			tip = uint32(e.Ledger)
		}
	}
	return tip
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}

var _ poller.EventSource = (*Source)(nil)
var _ interface {
	GetHealth(ctx context.Context) (*stellar.Health, error)
} = (*Client)(nil)
