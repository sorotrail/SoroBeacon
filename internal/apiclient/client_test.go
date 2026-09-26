package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// recordedRequest is one request a test client made, so a test can assert on
// the method, path, query, headers and body the client produced.
type recordedRequest struct {
	method  string
	path    string
	query   url.Values
	body    string
	headers http.Header
}

// recorder collects the requests made against a test server.
type recorder struct {
	requests []recordedRequest
}

// only fails the test unless exactly one request was made, which keeps a test
// from asserting on a call the client never issued.
func (r *recorder) only(t *testing.T) recordedRequest {
	t.Helper()
	if len(r.requests) != 1 {
		t.Fatalf("expected exactly one request, got %d", len(r.requests))
	}
	return r.requests[0]
}

// newClient starts a server that records every request and answers with
// handler, and returns a client pointed at it.
func newClient(t *testing.T, handler http.HandlerFunc) (*Client, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.requests = append(rec.requests, recordedRequest{
			method:  r.Method,
			path:    r.URL.Path,
			query:   r.URL.Query(),
			body:    string(body),
			headers: r.Header.Clone(),
		})
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL), rec
}

func TestNewAppliesDefaultsAndOptions(t *testing.T) {
	if got := New("").baseURL; got != DefaultBaseURL {
		t.Errorf("empty base URL should default to %q, got %q", DefaultBaseURL, got)
	}
	if got := New("http://beacon.example:9000/").baseURL; got != "http://beacon.example:9000" {
		t.Errorf("trailing slash should be trimmed, got %q", got)
	}
	if got := New("", WithToken("  abc ")).token; got != "abc" {
		t.Errorf("token should be trimmed, got %q", got)
	}
	if got := New("", WithHTTPClient(nil)).http; got == nil {
		t.Error("a nil HTTP client must not replace the default one")
	}
}

func TestListMonitorsSendsFiltersAndDecodesResponse(t *testing.T) {
	client, rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"monitors":[{
			"id":7,
			"name":"My token",
			"contract_ids":["CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"],
			"enabled":true,
			"created_at":"2026-09-24T10:00:00Z",
			"last_matched_at":"2026-09-24T11:00:00Z",
			"channel_ids":[3,4]
		}],"next_cursor":"7"}`)
	})
	client.token = "s3cret"

	enabled := true
	monitors, err := client.ListMonitors(context.Background(), ListOptions{
		Enabled: &enabled,
		Query:   "token",
		Limit:   5,
		// Type is a channel-only filter and must not be sent to /monitors.
		Type: "slack",
	})
	if err != nil {
		t.Fatalf("ListMonitors: %v", err)
	}

	req := rec.only(t)
	if req.method != http.MethodGet || req.path != "/api/v1/monitors" {
		t.Errorf("got %s %s, want GET /api/v1/monitors", req.method, req.path)
	}
	for key, want := range map[string]string{"enabled": "true", "q": "token", "limit": "5"} {
		if got := req.query.Get(key); got != want {
			t.Errorf("query %s = %q, want %q", key, got, want)
		}
	}
	if got := req.query.Get("type"); got != "" {
		t.Errorf("monitor listing must not send a type filter, got %q", got)
	}
	if got := req.headers.Get("Authorization"); got != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer s3cret")
	}

	if len(monitors) != 1 {
		t.Fatalf("expected 1 monitor, got %d", len(monitors))
	}
	got := monitors[0]
	if got.ID != 7 || got.Name != "My token" || !got.Enabled {
		t.Errorf("unexpected monitor: %+v", got)
	}
	if len(got.ChannelIDs) != 2 || got.ChannelIDs[0] != 3 || got.ChannelIDs[1] != 4 {
		t.Errorf("channel ids not decoded: %+v", got.ChannelIDs)
	}
	if got.LastMatchedAt == nil || got.LastMatchedAt.UTC().Format("2006-01-02T15:04:05Z") != "2026-09-24T11:00:00Z" {
		t.Errorf("last_matched_at not decoded: %+v", got.LastMatchedAt)
	}
}

func TestListChannelsSendsTypeFilter(t *testing.T) {
	client, rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"channels":[],"next_cursor":""}`)
	})

	if _, err := client.ListChannels(context.Background(), ListOptions{Type: "slack"}); err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	if got := rec.only(t).query.Get("type"); got != "slack" {
		t.Errorf("query type = %q, want %q", got, "slack")
	}
}

func TestCreateMonitorSendsBody(t *testing.T) {
	client, rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":1,"name":"My token","contract_ids":["CA7"],"enabled":false}`)
	})

	disabled := false
	monitor, err := client.CreateMonitor(context.Background(), MonitorCreate{
		Name:        "My token",
		ContractIDs: []string{"CA7"},
		Enabled:     &disabled,
		ChannelIDs:  []int64{2, 3},
	})
	if err != nil {
		t.Fatalf("CreateMonitor: %v", err)
	}

	req := rec.only(t)
	if req.method != http.MethodPost || req.path != "/api/v1/monitors" {
		t.Errorf("got %s %s, want POST /api/v1/monitors", req.method, req.path)
	}
	if got := req.headers.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	assertJSONEqual(t, req.body, `{
		"name": "My token",
		"contract_ids": ["CA7"],
		"enabled": false,
		"channel_ids": [2, 3]
	}`)

	if monitor == nil || monitor.ID != 1 || monitor.Enabled {
		t.Errorf("unexpected monitor: %+v", monitor)
	}
}

func TestUpdateMonitorOmitsUnsetFields(t *testing.T) {
	client, rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":1,"name":"My token","contract_ids":["CA7"],"enabled":false}`)
	})

	enabled := false
	if _, err := client.UpdateMonitor(context.Background(), 1, MonitorUpdate{Enabled: &enabled}); err != nil {
		t.Fatalf("UpdateMonitor: %v", err)
	}

	req := rec.only(t)
	if req.method != http.MethodPatch || req.path != "/api/v1/monitors/1" {
		t.Errorf("got %s %s, want PATCH /api/v1/monitors/1", req.method, req.path)
	}
	// A one-field patch must not resend the rest: the server applies the
	// body to the stored monitor, so an accidental field would overwrite it.
	assertJSONEqual(t, req.body, `{"enabled": false}`)
}

func TestRuleAndChannelPathsAndVerbs(t *testing.T) {
	client, rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/rules") && r.Method == http.MethodGet:
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/rules") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":9,"monitor_id":1,"type":"event_emitted","params":{"event_name":"transfer"},"enabled":true}`)
		case strings.HasSuffix(r.URL.Path, "/test"):
			fmt.Fprint(w, `{"status":"sent"}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})

	operations := []struct {
		name string
		want string
		call func() error
	}{
		{"monitor get", "GET /api/v1/monitors/1", func() error { _, err := client.GetMonitor(context.Background(), 1); return err }},
		{"monitor delete", "DELETE /api/v1/monitors/1", func() error { return client.DeleteMonitor(context.Background(), 1) }},
		{"rule list", "GET /api/v1/monitors/1/rules", func() error { _, err := client.ListRules(context.Background(), 1); return err }},
		{"rule add", "POST /api/v1/monitors/1/rules", func() error {
			_, err := client.CreateRule(context.Background(), 1, RuleCreate{Type: "event_emitted"})
			return err
		}},
		{"rule delete", "DELETE /api/v1/monitors/1/rules/9", func() error { return client.DeleteRule(context.Background(), 1, 9) }},
		{"channel delete", "DELETE /api/v1/channels/3", func() error { return client.DeleteChannel(context.Background(), 3) }},
		{"channel test", "POST /api/v1/channels/3/test", func() error { return client.TestChannel(context.Background(), 3) }},
	}

	for _, op := range operations {
		t.Run(op.name, func(t *testing.T) {
			rec.requests = nil
			if err := op.call(); err != nil {
				t.Fatalf("%s: %v", op.name, err)
			}
			req := rec.only(t)
			if got := req.method + " " + req.path; got != op.want {
				t.Errorf("got %s, want %s", got, op.want)
			}
		})
	}
}

func TestCreateRuleSendsRawParams(t *testing.T) {
	client, rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":2,"monitor_id":1,"type":"frequency_threshold","params":{"count":50,"window":"5m"},"enabled":true}`)
	})

	rule, err := client.CreateRule(context.Background(), 1, RuleCreate{
		Type:   "frequency_threshold",
		Params: json.RawMessage(`{"count":50,"window":"5m"}`),
	})
	if err != nil {
		t.Fatalf("CreateRule: %v", err)
	}
	assertJSONEqual(t, rec.only(t).body, `{"type":"frequency_threshold","params":{"count":50,"window":"5m"}}`)
	if rule == nil || rule.MonitorID != 1 || rule.Type != "frequency_threshold" {
		t.Errorf("unexpected rule: %+v", rule)
	}
}

func TestChannelConfigIsSentButNeverDecoded(t *testing.T) {
	client, rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		// The API strips config from responses; a rogue or older server
		// sending it back must still not put it on the wire here.
		fmt.Fprint(w, `{"id":1,"name":"ops-slack","type":"slack","enabled":true,
			"created_at":"2026-09-24T10:00:00Z",
			"config":{"webhook_url":"https://hooks.slack.com/services/T000/SECRET"}}`)
	})

	channel, err := client.CreateChannel(context.Background(), ChannelCreate{
		Name:   "ops-slack",
		Type:   "slack",
		Config: json.RawMessage(`{"webhook_url":"https://hooks.slack.com/services/T000/SECRET"}`),
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if got := rec.only(t).body; !strings.Contains(got, "webhook_url") {
		t.Errorf("config should be sent on create, got %s", got)
	}

	// Whatever the server returned, the decoded channel carries no config.
	encoded, err := json.Marshal(channel)
	if err != nil {
		t.Fatalf("marshal channel: %v", err)
	}
	if strings.Contains(string(encoded), "SECRET") {
		t.Errorf("a channel's config must never be decoded or re-encoded, got %s", encoded)
	}
}

func TestAPIErrorSurfacesEnvelope(t *testing.T) {
	client, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"not found","code":"Not Found","request_id":"9f2b","details":[]}`)
	})

	_, err := client.GetMonitor(context.Background(), 42)
	if err == nil {
		t.Fatal("expected an error for a 404")
	}
	if err.Error() != "not found" {
		t.Errorf("error message = %q, want the envelope's message", err.Error())
	}
	if !IsNotFound(err) {
		t.Error("IsNotFound should report true for a 404 envelope")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.Status != http.StatusNotFound || apiErr.Code != "Not Found" || apiErr.RequestID != "9f2b" {
		t.Errorf("unexpected APIError: %+v", apiErr)
	}
}

func TestTestChannelSurfacesDeliveryFailure(t *testing.T) {
	client, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"status":"failed","error":"discord returned 401"}`)
	})

	err := client.TestChannel(context.Background(), 1)
	if err == nil || err.Error() != "discord returned 401" {
		t.Fatalf("expected the delivery failure to surface, got %v", err)
	}
	if IsNotFound(err) {
		t.Error("a 502 is not a not-found")
	}
}

func TestDecodeAPIError(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		message   string
		code      string
		requestID string
		details   int
	}{
		{
			name:      "envelope",
			status:    http.StatusBadRequest,
			body:      `{"error":"validation failed","code":"Bad Request","request_id":"abc","details":[{"field":"name","reason":"name is required"}]}`,
			message:   "validation failed",
			code:      "Bad Request",
			requestID: "abc",
			details:   1,
		},
		{
			name:    "plain text body",
			status:  http.StatusBadGateway,
			body:    "upstream said no",
			message: "Bad Gateway",
		},
		{
			name:    "empty body",
			status:  http.StatusInternalServerError,
			body:    "",
			message: "Internal Server Error",
		},
		{
			name:    "json without an error field",
			status:  http.StatusForbidden,
			body:    `{"status":"failed"}`,
			message: "Forbidden",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var apiErr *APIError
			if !errors.As(decodeAPIError(tt.status, []byte(tt.body)), &apiErr) {
				t.Fatal("expected an *APIError")
			}
			if apiErr.Status != tt.status {
				t.Errorf("status = %d, want %d", apiErr.Status, tt.status)
			}
			if apiErr.Message != tt.message {
				t.Errorf("message = %q, want %q", apiErr.Message, tt.message)
			}
			if apiErr.Code != tt.code || apiErr.RequestID != tt.requestID {
				t.Errorf("code/request id = %q/%q, want %q/%q", apiErr.Code, apiErr.RequestID, tt.code, tt.requestID)
			}
			if len(apiErr.Details) != tt.details {
				t.Errorf("details = %d, want %d", len(apiErr.Details), tt.details)
			}
		})
	}
}

func TestTransportErrorNamesTheInstance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	_, err := New(url).ListMonitors(context.Background(), ListOptions{})
	if err == nil {
		t.Fatal("expected an error from an unreachable instance")
	}
	if !strings.Contains(err.Error(), url) {
		t.Errorf("error should name the unreachable instance: %v", err)
	}
	if IsNotFound(err) {
		t.Error("a transport error is not a not-found")
	}
}

// assertJSONEqual compares two JSON documents semantically, so a test is
// about the payload rather than key order and whitespace.
func assertJSONEqual(t *testing.T, got, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal([]byte(got), &gotValue); err != nil {
		t.Fatalf("response body is not JSON: %v (%s)", err, got)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("test expectation is not JSON: %v", err)
	}
	gotJSON, _ := json.Marshal(gotValue)
	wantJSON, _ := json.Marshal(wantValue)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("body = %s, want %s", gotJSON, wantJSON)
	}
}
