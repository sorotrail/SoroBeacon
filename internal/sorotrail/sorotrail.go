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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sorotrail/sorobeacon/internal/config"
	"github.com/sorotrail/sorobeacon/internal/poller"
	"github.com/sorotrail/sorobeacon/internal/stellar"
)

var (
	// ErrInvalidCursor is returned when the upstream SoroTrail indexer
	// rejects an opaque pagination cursor as malformed, expired, or stuck.
	// The poller re-anchors from its durable ledger checkpoint rather than
	// wedging on the rejected cursor forever.
	ErrInvalidCursor = poller.ErrInvalidCursor

	// ErrIncompatibleUpstream is returned when the configured SOROTRAIL_URL
	// responds with a non-SoroTrail payload (such as HTML or an unrelated JSON
	// schema).
	ErrIncompatibleUpstream = errors.New("sorotrail: incompatible upstream response")

	// ErrUpstreamUnreachable is returned when the upstream endpoint cannot be
	// reached due to a transport failure or transient 5xx response.
	ErrUpstreamUnreachable = errors.New("sorotrail: upstream unreachable")
)

// Client is a SoroTrail HTTP API client with bounded transient retry and
// consecutive-failure tracking.
type Client struct {
	base                string
	http                *http.Client
	retryAttempts       int
	retryBaseDelay      time.Duration
	consecutiveFailures atomic.Int64
}

// NewClient returns a client for a SoroTrail base URL, e.g.
// http://sorotrail:8080. The base URL carries no path; /api/v1/... is
// appended here.
func NewClient(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{
		base:           strings.TrimRight(baseURL, "/"),
		http:           httpClient,
		retryAttempts:  2,
		retryBaseDelay: 25 * time.Millisecond,
	}
}

// WithRetry configures the bounded retry attempts and base backoff delay used
// for transient upstream failures (HTTP 429, 502, 503, 504).
func (c *Client) WithRetry(attempts int, baseDelay time.Duration) *Client {
	if attempts < 1 {
		attempts = 1
	}
	c.retryAttempts = attempts
	c.retryBaseDelay = baseDelay
	return c
}

// ConsecutiveFailures returns the number of consecutive failed HTTP calls to
// the upstream SoroTrail service. Resets to 0 on any successful response.
func (c *Client) ConsecutiveFailures() int {
	return int(c.consecutiveFailures.Load())
}

// Stats is the response payload of GET /api/v1/stats. It supports both legacy
// field names (first_ledger, last_ledger) and the richer SoroTrail indexer
// fields (oldest_stored_ledger, last_ingested_ledger, chain_head_ledger,
// ingest_lag_ledgers, verified_through_ledger, retracted_from_ledger,
// retracted_event_ids).
type Stats struct {
	TotalEvents           int64      `json:"total_events"`
	FirstLedger           int64      `json:"first_ledger"`
	LastLedger            int64      `json:"last_ledger"`
	OldestStoredLedger    uint32     `json:"oldest_stored_ledger"`
	LastIngestedLedger    uint32     `json:"last_ingested_ledger"`
	ChainHeadLedger       *uint32    `json:"chain_head_ledger,omitempty"`
	IngestLagLedgers      *int64     `json:"ingest_lag_ledgers,omitempty"`
	VerifiedThroughLedger uint32     `json:"verified_through_ledger,omitempty"`
	RetractedFromLedger   uint32     `json:"retracted_from_ledger,omitempty"`
	RetractedEventIDs     []string   `json:"retracted_event_ids,omitempty"`
	LastSuccessfulPoll    *time.Time `json:"last_successful_poll,omitempty"`
	NetworkPassphrase     string     `json:"network_passphrase"`
}

// TipLedger returns the newest ledger SoroTrail has ingested.
func (s Stats) TipLedger() uint32 {
	if s.LastIngestedLedger > 0 {
		return s.LastIngestedLedger
	}
	if s.LastLedger > 0 {
		return uint32(s.LastLedger)
	}
	return 0
}

// OldestLedger returns the oldest ledger SoroTrail still retains.
func (s Stats) OldestLedger() uint32 {
	if s.OldestStoredLedger > 0 {
		return s.OldestStoredLedger
	}
	if s.FirstLedger > 0 {
		return uint32(s.FirstLedger)
	}
	return 0
}

// ChainHead returns the chain tip known to SoroTrail. When SoroTrail reports
// chain_head_ledger or ingest_lag_ledgers, ChainHead reflects the true chain
// head rather than only SoroTrail's own ingested tip.
func (s Stats) ChainHead() uint32 {
	tip := s.TipLedger()
	if s.ChainHeadLedger != nil && *s.ChainHeadLedger > tip {
		return *s.ChainHeadLedger
	}
	if s.IngestLagLedgers != nil && *s.IngestLagLedgers > 0 {
		return tip + uint32(*s.IngestLagLedgers)
	}
	return tip
}

// UpstreamLag returns how many ledgers SoroTrail itself trails the chain head.
func (s Stats) UpstreamLag() uint32 {
	if s.IngestLagLedgers != nil {
		if *s.IngestLagLedgers > 0 {
			return uint32(*s.IngestLagLedgers)
		}
		return 0
	}
	tip := s.TipLedger()
	head := s.ChainHead()
	if head > tip {
		return head - tip
	}
	return 0
}

// HealthResponse is the payload of GET /api/v1/health on SoroTrail.
type HealthResponse struct {
	Status             string  `json:"status"`
	NetworkPassphrase  string  `json:"network_passphrase"`
	LastIngestedLedger uint32  `json:"last_ingested_ledger"`
	LastLedger         uint32  `json:"last_ledger"`
	LatestLedger       uint32  `json:"latest_ledger"`
	OldestStoredLedger uint32  `json:"oldest_stored_ledger"`
	FirstLedger        uint32  `json:"first_ledger"`
	ChainHeadLedger    *uint32 `json:"chain_head_ledger,omitempty"`
	IngestLagLedgers   *int64  `json:"ingest_lag_ledgers,omitempty"`
}

// eventsResponse is GET /api/v1/events.
type eventsResponse struct {
	Events              []event  `json:"events"`
	NextCursor          string   `json:"next_cursor,omitempty"`
	LatestLedger        uint32   `json:"latest_ledger,omitempty"`
	RetractedFromLedger uint32   `json:"retracted_from_ledger,omitempty"`
	RetractedEventIDs   []string `json:"retracted_event_ids,omitempty"`
}

// event is one SoroTrail event row. Topics and value arrive decoded, in
// the same single-key wrapper vocabulary the local decoder produces
// ({"symbol": "..."}, {"i128": "..."}), so they pass straight through to
// the rules engine.
type event struct {
	ID          string          `json:"id"`
	ContractID  string          `json:"contract_id"`
	Ledger      int64           `json:"ledger"`
	TxHash      string          `json:"tx_hash"`
	Topics      json.RawMessage `json:"topics"`
	Value       json.RawMessage `json:"value"`
	ClosedAt    time.Time       `json:"ledger_closed_at"`
	Retracted   bool            `json:"retracted,omitempty"`
	RetractedAt *time.Time      `json:"retracted_at,omitempty"`
}

type statusError struct {
	path       string
	statusCode int
	body       string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("sorotrail %s: status %d: %.200s", e.path, e.statusCode, e.body)
}

func isTransientStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func (c *Client) getRawJSON(ctx context.Context, path string, query url.Values) ([]byte, error) {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	attempts := c.retryAttempts
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 && c.retryBaseDelay > 0 {
			delay := c.retryBaseDelay * time.Duration(1<<(attempt-1))
			select {
			case <-ctx.Done():
				c.consecutiveFailures.Add(1)
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			c.consecutiveFailures.Add(1)
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		res, err := c.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("%w: sorotrail %s: %w", ErrUpstreamUnreachable, path, err)
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(res.Body, 32<<20))
		_ = res.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("%w: sorotrail %s: read response: %w", ErrUpstreamUnreachable, path, readErr)
			continue
		}
		if res.StatusCode != http.StatusOK {
			sErr := &statusError{path: path, statusCode: res.StatusCode, body: string(body)}
			if isTransientStatus(res.StatusCode) {
				lastErr = fmt.Errorf("%w: %w", ErrUpstreamUnreachable, sErr)
				continue
			}
			c.consecutiveFailures.Add(1)
			return nil, sErr
		}
		c.consecutiveFailures.Store(0)
		return body, nil
	}
	c.consecutiveFailures.Add(1)
	return nil, lastErr
}

func (c *Client) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	body, err := c.getRawJSON(ctx, path, query)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		c.consecutiveFailures.Add(1)
		return fmt.Errorf("%w: sorotrail %s: decode response: %w", ErrIncompatibleUpstream, path, err)
	}
	return nil
}

// Stats fetches the indexer's ledger coverage and health metadata.
func (c *Client) Stats(ctx context.Context) (Stats, error) {
	body, err := c.getRawJSON(ctx, "/api/v1/stats", nil)
	if err != nil {
		var sErr *statusError
		if errors.As(err, &sErr) && (sErr.statusCode == http.StatusNotFound || sErr.statusCode == http.StatusMethodNotAllowed) {
			return Stats{}, fmt.Errorf("%w: %w", ErrIncompatibleUpstream, err)
		}
		return Stats{}, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		c.consecutiveFailures.Add(1)
		return Stats{}, fmt.Errorf("%w: sorotrail /api/v1/stats: decode response: %w", ErrIncompatibleUpstream, err)
	}
	if !hasRecognizedStatsField(raw) {
		c.consecutiveFailures.Add(1)
		return Stats{}, fmt.Errorf("%w: sorotrail /api/v1/stats: response does not match SoroTrail stats schema", ErrIncompatibleUpstream)
	}
	var s Stats
	if err := json.Unmarshal(body, &s); err != nil {
		c.consecutiveFailures.Add(1)
		return Stats{}, fmt.Errorf("%w: sorotrail /api/v1/stats: decode response: %w", ErrIncompatibleUpstream, err)
	}
	return s, nil
}

func hasRecognizedStatsField(raw map[string]json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	known := []string{
		"first_ledger",
		"last_ledger",
		"oldest_stored_ledger",
		"last_ingested_ledger",
		"total_events",
		"network_passphrase",
		"chain_head_ledger",
		"ingest_lag_ledgers",
		"verified_through_ledger",
		"retracted_from_ledger",
		"retracted_event_ids",
	}
	for _, k := range known {
		if _, ok := raw[k]; ok {
			return true
		}
	}
	return false
}

// NetworkPassphrase returns the identity advertised by the upstream SoroTrail
// service, checking /api/v1/stats first and falling back to /api/v1/health.
// Older SoroTrail versions omit this field.
func (c *Client) NetworkPassphrase(ctx context.Context) (string, error) {
	s, err := c.Stats(ctx)
	if err != nil {
		return "", err
	}
	if s.NetworkPassphrase != "" {
		return s.NetworkPassphrase, nil
	}
	var h HealthResponse
	if hErr := c.getJSON(ctx, "/api/v1/health", nil, &h); hErr == nil && h.NetworkPassphrase != "" {
		return h.NetworkPassphrase, nil
	}
	return "", nil
}

// VerifyUpstream verifies that the upstream endpoint speaks the SoroTrail API
// and, when the upstream advertises a network passphrase, that it matches
// expectedPassphrase.
func (c *Client) VerifyUpstream(ctx context.Context, expectedPassphrase string) error {
	passphrase, err := c.NetworkPassphrase(ctx)
	if err != nil {
		return err
	}
	if expectedPassphrase != "" && passphrase != "" {
		if err := config.VerifyPassphrase(expectedPassphrase, passphrase); err != nil {
			return fmt.Errorf("sorotrail upstream %s: %w", c.base, err)
		}
	}
	return nil
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
	if err != nil {
		var sErr *statusError
		if cursor != "" && errors.As(err, &sErr) {
			lowerBody := strings.ToLower(sErr.body)
			if sErr.statusCode == http.StatusBadRequest ||
				sErr.statusCode == http.StatusGone ||
				sErr.statusCode == http.StatusUnprocessableEntity ||
				strings.Contains(lowerBody, "cursor") {
				return eventsResponse{}, fmt.Errorf("%w: %w", ErrInvalidCursor, err)
			}
		}
		return eventsResponse{}, err
	}
	if cursor != "" && res.NextCursor != "" && res.NextCursor == cursor {
		return eventsResponse{}, fmt.Errorf("%w: upstream returned identical next_cursor %q", ErrInvalidCursor, cursor)
	}
	return res, nil
}

func isUnhealthyStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "degraded", "unready", "unhealthy", "error", "down", "failing":
		return true
	default:
		return false
	}
}

// Health adapts the indexer to the api.HealthChecker interface, so the
// /health and /readyz probes work identically in upstream mode. It checks
// /api/v1/health when available (failing when SoroTrail reports itself
// degraded or unready) and falls back to /api/v1/stats against older
// SoroTrail releases that only expose /api/v1/stats.
func (c *Client) Health(ctx context.Context) (*stellar.Health, error) {
	var h HealthResponse
	hErr := c.getJSON(ctx, "/api/v1/health", nil, &h)
	if hErr != nil {
		var sErr *statusError
		if !errors.As(hErr, &sErr) || sErr.statusCode != http.StatusNotFound {
			return nil, hErr
		}
	} else if isUnhealthyStatus(h.Status) {
		c.consecutiveFailures.Add(1)
		return nil, fmt.Errorf("sorotrail /api/v1/health: upstream status %q", h.Status)
	}

	s, err := c.Stats(ctx)
	if err != nil {
		if hErr == nil {
			latest := h.LatestLedger
			if latest == 0 {
				latest = h.LastIngestedLedger
			}
			if latest == 0 {
				latest = h.LastLedger
			}
			oldest := h.OldestStoredLedger
			if oldest == 0 {
				oldest = h.FirstLedger
			}
			return &stellar.Health{
				Status:       "healthy",
				LatestLedger: latest,
				OldestLedger: oldest,
			}, nil
		}
		return nil, err
	}
	return &stellar.Health{
		Status:       "healthy",
		LatestLedger: s.TipLedger(),
		OldestLedger: s.OldestLedger(),
	}, nil
}

// GetHealth satisfies api.HealthChecker.
func (c *Client) GetHealth(ctx context.Context) (*stellar.Health, error) {
	return c.Health(ctx)
}

// Source implements poller.EventSource against a SoroTrail indexer.
type Source struct {
	client             *Client
	expectedPassphrase string

	mu                  sync.Mutex
	verified            bool
	lastStats           Stats
	prevVerifiedThrough uint32
	retractedFromLedger uint32
	retractedEventIDs   []string
}

// NewSource wires an upstream-mode event source.
func NewSource(c *Client) *Source {
	return &Source{client: c}
}

// WithExpectedPassphrase configures the Stellar network passphrase that the
// upstream SoroTrail instance must match before events are ingested.
func (s *Source) WithExpectedPassphrase(passphrase string) *Source {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expectedPassphrase = passphrase
	s.verified = false
	return s
}

func (s *Source) ensureVerified(ctx context.Context, st Stats) error {
	s.mu.Lock()
	expected := s.expectedPassphrase
	verified := s.verified
	s.mu.Unlock()

	if verified || expected == "" {
		return nil
	}
	passphrase := st.NetworkPassphrase
	if passphrase == "" {
		var h HealthResponse
		if err := s.client.getJSON(ctx, "/api/v1/health", nil, &h); err == nil {
			passphrase = h.NetworkPassphrase
		}
	}
	if passphrase != "" {
		if err := config.VerifyPassphrase(expected, passphrase); err != nil {
			return fmt.Errorf("sorotrail upstream %s: %w", s.client.base, err)
		}
	}
	s.mu.Lock()
	s.verified = true
	s.mu.Unlock()
	return nil
}

func (s *Source) recordStats(st Stats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st.VerifiedThroughLedger > 0 && s.prevVerifiedThrough > 0 && st.VerifiedThroughLedger < s.prevVerifiedThrough {
		rewind := st.VerifiedThroughLedger + 1
		if s.retractedFromLedger == 0 || rewind < s.retractedFromLedger {
			s.retractedFromLedger = rewind
		}
	}
	if st.VerifiedThroughLedger > 0 {
		s.prevVerifiedThrough = st.VerifiedThroughLedger
	}
	if st.RetractedFromLedger > 0 {
		if s.retractedFromLedger == 0 || st.RetractedFromLedger < s.retractedFromLedger {
			s.retractedFromLedger = st.RetractedFromLedger
		}
	}
	for _, id := range st.RetractedEventIDs {
		s.addRetractedEventIDLocked(id)
	}
	s.lastStats = st
}

func (s *Source) addRetractedEventIDLocked(id string) {
	if id == "" {
		return
	}
	for _, existing := range s.retractedEventIDs {
		if existing == id {
			return
		}
	}
	s.retractedEventIDs = append(s.retractedEventIDs, id)
}

func (s *Source) recordEventRetractions(fromLedger uint32, ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fromLedger > 0 && (s.retractedFromLedger == 0 || fromLedger < s.retractedFromLedger) {
		s.retractedFromLedger = fromLedger
	}
	for _, id := range ids {
		s.addRetractedEventIDLocked(id)
	}
}

// UpstreamStatus reports the latest chain head, upstream lag, and consecutive
// failure count observed from the SoroTrail indexer.
func (s *Source) UpstreamStatus() (chainHead uint32, upstreamLag uint32, consecutiveFailures int) {
	s.mu.Lock()
	st := s.lastStats
	s.mu.Unlock()
	return st.ChainHead(), st.UpstreamLag(), s.client.ConsecutiveFailures()
}

// ConsumeRetractions returns and clears any upstream retraction signals
// (retracted_from_ledger, regressed verified_through_ledger, or retracted
// event rows) accumulated since the previous call.
func (s *Source) ConsumeRetractions() (retractedFromLedger uint32, retractedEventIDs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	from := s.retractedFromLedger
	ids := s.retractedEventIDs
	s.retractedFromLedger = 0
	s.retractedEventIDs = nil
	return from, ids
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
	if err := s.ensureVerified(ctx, st); err != nil {
		return 0, err
	}
	s.recordStats(st)
	tip := st.TipLedger()
	if tip == 0 {
		return 0, fmt.Errorf("sorotrail: indexer reports no ingested ledgers")
	}
	return tip, nil
}

// OldestLedger reports the oldest ledger the indexer has ingested, so a
// backfill can clamp a requested range to what is actually available.
// SoroTrail keeps history durably past the RPC's retention window, so this is
// usually the first ledger it ever saw; a fresh indexer that has ingested
// nothing yet reports 0 ("unknown").
func (s *Source) OldestLedger(ctx context.Context) (uint32, error) {
	st, err := s.client.Stats(ctx)
	if err != nil {
		return 0, err
	}
	if err := s.ensureVerified(ctx, st); err != nil {
		return 0, err
	}
	s.recordStats(st)
	return st.OldestLedger(), nil
}

// FetchEvents pages the indexer's /events. SoroTrail paginates ascending by
// event ID with an opaque cursor, which passes through untouched: one
// request covers the whole contract union, so there is no batching state
// to encode.
func (s *Source) FetchEvents(ctx context.Context, startLedger uint32, watch []poller.Watch, cursor string, limit int) (poller.FetchPage, error) {
	if limit <= 0 {
		limit = 50
	}

	s.mu.Lock()
	needVerify := s.expectedPassphrase != "" && !s.verified
	s.mu.Unlock()

	if cursor == "" || needVerify {
		if st, err := s.client.Stats(ctx); err == nil {
			if vErr := s.ensureVerified(ctx, st); vErr != nil {
				return poller.FetchPage{}, vErr
			}
			s.recordStats(st)
		} else if needVerify {
			return poller.FetchPage{}, err
		}
	}

	contracts := make([]string, 0, len(watch))
	for _, w := range watch {
		contracts = append(contracts, w.ContractID)
	}
	res, err := s.client.Events(ctx, joinComma(contracts), int64(startLedger), cursor, limit)
	if err != nil {
		return poller.FetchPage{}, err
	}

	var retractedIDs []string
	retractedFrom := res.RetractedFromLedger
	if len(res.RetractedEventIDs) > 0 {
		retractedIDs = append(retractedIDs, res.RetractedEventIDs...)
	}

	page := poller.FetchPage{}
	for _, e := range res.Events {
		if e.Retracted || e.RetractedAt != nil {
			retractedIDs = append(retractedIDs, e.ID)
			if e.Ledger > 0 {
				l := uint32(e.Ledger)
				if retractedFrom == 0 || l < retractedFrom {
					retractedFrom = l
				}
			}
			continue
		}
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

	if retractedFrom > 0 || len(retractedIDs) > 0 {
		s.recordEventRetractions(retractedFrom, retractedIDs)
	}

	page.LatestLedger = s.tipFrom(ctx, res)
	if len(res.Events) > 0 {
		page.NextCursor = res.NextCursor
	}
	return page, nil
}

// tipFrom derives a page's latest ledger from the newest event in it. On the
// final page of a stream (when NextCursor is empty, including short or empty
// pages), it falls back to the upstream indexer's ingested tip so the poller
// checkpoint advances to the ledger SoroTrail has actually indexed through.
func (s *Source) tipFrom(ctx context.Context, res eventsResponse) uint32 {
	var tip uint32
	for _, e := range res.Events {
		if e.Ledger > 0 && uint32(e.Ledger) > tip {
			tip = uint32(e.Ledger)
		}
	}
	if res.LatestLedger > tip {
		tip = res.LatestLedger
	}
	if res.NextCursor != "" && len(res.Events) > 0 && tip > 0 {
		return tip
	}

	s.mu.Lock()
	statsTip := s.lastStats.TipLedger()
	s.mu.Unlock()

	if statsTip > tip {
		tip = statsTip
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
var _ poller.RetentionReporter = (*Source)(nil)
var _ interface {
	GetHealth(ctx context.Context) (*stellar.Health, error)
} = (*Client)(nil)
