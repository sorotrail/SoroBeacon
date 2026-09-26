// Package apiclient is a small HTTP client for SoroBeacon's JSON API.
//
// It is the single implementation shared by the CLI (cmd/sorobeacon) and any
// future tooling: everything the API exposes to an operator — monitors, rules
// and channels — goes through these methods, so a change to the wire format
// is fixed in one place. It deliberately does not depend on internal/store,
// so a tool can use it without pulling in the database stack.
//
// Failures from the server arrive as *APIError, carrying the error envelope's
// message, code, request id and field details. Callers are expected to print
// that message and exit non-zero; nothing here panics or dumps a stack trace
// for an ordinary failure such as a 404.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is where the CLI looks for an instance when SOROBEACON_URL
// is unset, matching the docker-compose quickstart.
const DefaultBaseURL = "http://localhost:8080"

// requestTimeout bounds one API call. The API's own writes are fast, and a
// script should not hang forever on a connection that never completes.
const requestTimeout = 30 * time.Second

// maxResponseBytes caps how much of a response body is read, so a
// misdirected URL that answers with a huge page cannot exhaust memory.
const maxResponseBytes = 4 << 20

// Client talks to one SoroBeacon instance. It is safe for concurrent use.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithToken sets the bearer token sent as Authorization on every request.
// An instance with API_TOKEN unset needs none.
func WithToken(token string) Option {
	return func(c *Client) { c.token = strings.TrimSpace(token) }
}

// WithHTTPClient overrides the HTTP client, which is how tests point one at
// an httptest server.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// New returns a client for the instance at baseURL. An empty baseURL means
// DefaultBaseURL; a trailing slash is trimmed so paths join cleanly.
func New(baseURL string, opts ...Option) *Client {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: requestTimeout},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Monitor mirrors the API's monitor resource.
type Monitor struct {
	ID            int64      `json:"id"`
	Name          string     `json:"name"`
	ContractIDs   []string   `json:"contract_ids"`
	Enabled       bool       `json:"enabled"`
	CreatedAt     time.Time  `json:"created_at"`
	LastMatchedAt *time.Time `json:"last_matched_at"`
	ChannelIDs    []int64    `json:"channel_ids"`
}

// Rule mirrors the API's rule resource. Params stays raw JSON because each
// rule type defines its own shape.
type Rule struct {
	ID        int64           `json:"id"`
	MonitorID int64           `json:"monitor_id"`
	Type      string          `json:"type"`
	Params    json.RawMessage `json:"params"`
	Enabled   bool            `json:"enabled"`
}

// Channel mirrors the API's channel resource. Config is absent on purpose:
// it holds webhook URLs, bot tokens and SMTP credentials, the API never
// returns it, and a type without the field cannot leak it into output.
type Channel struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

// MonitorCreate is the body of POST /monitors.
type MonitorCreate struct {
	Name        string   `json:"name"`
	ContractIDs []string `json:"contract_ids"`
	Enabled     *bool    `json:"enabled,omitempty"`
	ChannelIDs  []int64  `json:"channel_ids,omitempty"`
}

// MonitorUpdate is the body of PATCH /monitors/{id}. Nil fields are left
// alone, so a caller can change one attribute without resending the rest.
type MonitorUpdate struct {
	Name        *string   `json:"name,omitempty"`
	ContractIDs *[]string `json:"contract_ids,omitempty"`
	Enabled     *bool     `json:"enabled,omitempty"`
	ChannelIDs  *[]int64  `json:"channel_ids,omitempty"`
}

// RuleCreate is the body of POST /monitors/{id}/rules.
type RuleCreate struct {
	Type    string          `json:"type"`
	Params  json.RawMessage `json:"params,omitempty"`
	Enabled *bool           `json:"enabled,omitempty"`
}

// ChannelCreate is the body of POST /channels. Config is sent but never
// returned; the server validates it and stores it encrypted when a
// CONFIG_ENCRYPTION_KEY is configured.
type ChannelCreate struct {
	Name    string          `json:"name"`
	Type    string          `json:"type"`
	Config  json.RawMessage `json:"config,omitempty"`
	Enabled *bool           `json:"enabled,omitempty"`
}

// ListOptions filters a listing request. The zero value lists everything the
// instance has (up to the server's page limit).
type ListOptions struct {
	// Enabled, when set, keeps only monitors/channels in that state.
	Enabled *bool
	// Query filters by name or contract id (monitors only).
	Query string
	// Type filters by channel type (channels only).
	Type string
	// Limit caps the page size.
	Limit int
}

// FieldError is one entry of the error envelope's optional details array: a
// dotted JSON path and why it failed.
type FieldError struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// APIError is a non-2xx response. Message is the server's error envelope
// "error" field when the body is one, and the HTTP status text otherwise, so
// a proxy's HTML error page is never printed as the reason.
type APIError struct {
	Status    int
	Code      string
	Message   string
	RequestID string
	Details   []FieldError
}

// Error returns the human-readable reason, which is what callers print.
func (e *APIError) Error() string { return e.Message }

// IsNotFound reports whether err is a 404 from the API, so callers can give a
// friendlier message without string matching.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

// ListMonitors returns the monitors matching opts.
func (c *Client) ListMonitors(ctx context.Context, opts ListOptions) ([]Monitor, error) {
	var out struct {
		Monitors []Monitor `json:"monitors"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/monitors", opts.query(false), nil, &out); err != nil {
		return nil, err
	}
	return out.Monitors, nil
}

// GetMonitor returns one monitor.
func (c *Client) GetMonitor(ctx context.Context, id int64) (*Monitor, error) {
	var m Monitor
	if err := c.do(ctx, http.MethodGet, "/api/v1/monitors/"+idPath(id), nil, nil, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// CreateMonitor creates a monitor and returns it as stored.
func (c *Client) CreateMonitor(ctx context.Context, in MonitorCreate) (*Monitor, error) {
	var m Monitor
	if err := c.do(ctx, http.MethodPost, "/api/v1/monitors", nil, in, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// UpdateMonitor patches a monitor and returns it as stored.
func (c *Client) UpdateMonitor(ctx context.Context, id int64, in MonitorUpdate) (*Monitor, error) {
	var m Monitor
	if err := c.do(ctx, http.MethodPatch, "/api/v1/monitors/"+idPath(id), nil, in, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// DeleteMonitor removes a monitor.
func (c *Client) DeleteMonitor(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/monitors/"+idPath(id), nil, nil, nil)
}

// ListRules returns every rule of one monitor, enabled or not.
func (c *Client) ListRules(ctx context.Context, monitorID int64) ([]Rule, error) {
	var out []Rule
	path := "/api/v1/monitors/" + idPath(monitorID) + "/rules"
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateRule adds a rule to a monitor.
func (c *Client) CreateRule(ctx context.Context, monitorID int64, in RuleCreate) (*Rule, error) {
	var r Rule
	path := "/api/v1/monitors/" + idPath(monitorID) + "/rules"
	if err := c.do(ctx, http.MethodPost, path, nil, in, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// DeleteRule removes one rule of a monitor.
func (c *Client) DeleteRule(ctx context.Context, monitorID, ruleID int64) error {
	path := "/api/v1/monitors/" + idPath(monitorID) + "/rules/" + idPath(ruleID)
	return c.do(ctx, http.MethodDelete, path, nil, nil, nil)
}

// ListChannels returns the channels matching opts. The response never carries
// channel config.
func (c *Client) ListChannels(ctx context.Context, opts ListOptions) ([]Channel, error) {
	var out struct {
		Channels []Channel `json:"channels"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/channels", opts.query(true), nil, &out); err != nil {
		return nil, err
	}
	return out.Channels, nil
}

// CreateChannel creates a channel. Config is validated by the server.
func (c *Client) CreateChannel(ctx context.Context, in ChannelCreate) (*Channel, error) {
	var ch Channel
	if err := c.do(ctx, http.MethodPost, "/api/v1/channels", nil, in, &ch); err != nil {
		return nil, err
	}
	return &ch, nil
}

// DeleteChannel removes a channel.
func (c *Client) DeleteChannel(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/channels/"+idPath(id), nil, nil, nil)
}

// TestChannel sends a synthetic alert through one channel.
func (c *Client) TestChannel(ctx context.Context, id int64) error {
	path := "/api/v1/channels/" + idPath(id) + "/test"
	return c.do(ctx, http.MethodPost, path, nil, nil, nil)
}

// query renders the filter as query parameters. withType is false for
// endpoints that have no type filter, so a shared ListOptions cannot send a
// parameter the server would reject.
func (o ListOptions) query(withType bool) url.Values {
	q := url.Values{}
	if o.Enabled != nil {
		q.Set("enabled", strconv.FormatBool(*o.Enabled))
	}
	if o.Query != "" {
		q.Set("q", o.Query)
	}
	if withType && o.Type != "" {
		q.Set("type", o.Type)
	}
	if o.Limit > 0 {
		q.Set("limit", strconv.Itoa(o.Limit))
	}
	return q
}

func idPath(id int64) string { return strconv.FormatInt(id, 10) }

// do performs one request and decodes the response into out. A nil out (and
// any 204) discards the body, which is what DELETE needs.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request body: %w", err)
		}
		payload = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, payload)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request to %s failed: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("read response from %s: %w", c.baseURL, err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return decodeAPIError(resp.StatusCode, data)
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response from %s: %w", c.baseURL, err)
	}
	return nil
}

// decodeAPIError turns an error response into an *APIError. The envelope is
// {"error","code","request_id","details"}; anything else (a proxy's plain
// text or an HTML page) falls back to the status text.
func decodeAPIError(status int, body []byte) error {
	apiErr := &APIError{Status: status, Message: http.StatusText(status)}
	var envelope struct {
		Error     string       `json:"error"`
		Code      string       `json:"code"`
		RequestID string       `json:"request_id"`
		Details   []FieldError `json:"details"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		if envelope.Error != "" {
			apiErr.Message = envelope.Error
		}
		apiErr.Code = envelope.Code
		apiErr.RequestID = envelope.RequestID
		apiErr.Details = envelope.Details
	}
	return apiErr
}
