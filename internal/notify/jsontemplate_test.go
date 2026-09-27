package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJSONTemplateRendersBodyAndSends(t *testing.T) {
	var gotBody []byte
	var gotMethod string
	var gotHeaders http.Header

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotMethod = r.Method
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`{
		"url": %q,
		"method": "POST",
		"headers": {"Authorization": "Bearer secret-token", "X-Custom": "custom-value"},
		"body_template": "{\"title\": \"{{.MonitorName}}\", \"ref\": \"{{.EventID}}\", \"ledger\": {{.Ledger}}}"
	}`, srv.URL)

	n, err := NewJSONTemplate(json.RawMessage(cfg))
	require.NoError(t, err)

	alert := Alert{
		ID:          42,
		MonitorID:   1,
		MonitorName: "Token treasury",
		RuleID:      3,
		RuleType:    "value_threshold",
		EventID:     "evt-123",
		ContractID:  "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA",
		EventName:   "transfer",
		Ledger:      3721765,
		TxHash:      "4f2a...",
		CreatedAt:   mustParseTime(t, "2026-07-21T09:41:32Z"),
	}

	require.NoError(t, n.Send(context.Background(), alert))

	assert.Equal(t, http.MethodPost, gotMethod)

	var sent map[string]any
	require.NoError(t, json.Unmarshal(gotBody, &sent))
	assert.Equal(t, "Token treasury", sent["title"])
	assert.Equal(t, "evt-123", sent["ref"])
	assert.Equal(t, float64(3721765), sent["ledger"])

	// Verify custom headers were sent
	assert.Equal(t, "Bearer secret-token", gotHeaders.Get("Authorization"))
	assert.Equal(t, "custom-value", gotHeaders.Get("X-Custom"))

	// Content-Type should be set by requestJSON
	assert.Equal(t, "application/json", gotHeaders.Get("Content-Type"))
}

func TestJSONTemplateMethodDefaultsToPOST(t *testing.T) {
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`{"url": %q, "body_template": "{\"x\": 1}"}`, srv.URL)
	n, err := NewJSONTemplate(json.RawMessage(cfg))
	require.NoError(t, err)

	require.NoError(t, n.Send(context.Background(), Alert{ID: 1}))
	assert.Equal(t, http.MethodPost, gotMethod)
}

func TestJSONTemplateMethodPUT(t *testing.T) {
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`{"url": %q, "method": "PUT", "body_template": "{\"x\": 1}"}`, srv.URL)
	n, err := NewJSONTemplate(json.RawMessage(cfg))
	require.NoError(t, err)

	require.NoError(t, n.Send(context.Background(), Alert{ID: 1}))
	assert.Equal(t, http.MethodPut, gotMethod)
}

func TestJSONTemplateMethodPATCH(t *testing.T) {
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`{"url": %q, "method": "PATCH", "body_template": "{\"x\": 1}"}`, srv.URL)
	n, err := NewJSONTemplate(json.RawMessage(cfg))
	require.NoError(t, err)

	require.NoError(t, n.Send(context.Background(), Alert{ID: 1}))
	assert.Equal(t, http.MethodPatch, gotMethod)
}

func TestJSONTemplateMethodCaseInsensitive(t *testing.T) {
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`{"url": %q, "method": "post", "body_template": "{\"x\": 1}"}`, srv.URL)
	n, err := NewJSONTemplate(json.RawMessage(cfg))
	require.NoError(t, err)

	require.NoError(t, n.Send(context.Background(), Alert{ID: 1}))
	assert.Equal(t, http.MethodPost, gotMethod)
}

func TestJSONTemplateInvalidMethodRejected(t *testing.T) {
	_, err := NewJSONTemplate(json.RawMessage(`{"url": "https://example.com", "method": "GET", "body_template": "{\"x\": 1}"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "method must be one of POST, PUT, PATCH")

	_, err = NewJSONTemplate(json.RawMessage(`{"url": "https://example.com", "method": "DELETE", "body_template": "{\"x\": 1}"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "method must be one of POST, PUT, PATCH")
}

func TestJSONTemplateMalformedTemplateRejectedAtConstruction(t *testing.T) {
	// Malformed template: unclosed action (missing closing }})
	_, err := NewJSONTemplate(json.RawMessage(`{"url": "https://example.com", "body_template": "{\"title\": \"{{.MonitorName}\"}"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid body_template")
}

func TestJSONTemplateMissingURLRejected(t *testing.T) {
	_, err := NewJSONTemplate(json.RawMessage(`{"body_template": "{\"x\": 1}"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "url is required")
}

func TestJSONTemplateMissingBodyTemplateRejected(t *testing.T) {
	_, err := NewJSONTemplate(json.RawMessage(`{"url": "https://example.com"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "body_template is required")

	_, err = NewJSONTemplate(json.RawMessage(`{"url": "https://example.com", "body_template": ""}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "body_template is required")

	_, err = NewJSONTemplate(json.RawMessage(`{"url": "https://example.com", "body_template": "   "}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "body_template is required")
}

func TestJSONTemplateNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`{"url": %q, "body_template": "{\"x\": 1}"}`, srv.URL)
	n, err := NewJSONTemplate(json.RawMessage(cfg))
	require.NoError(t, err)

	err = n.Send(context.Background(), Alert{ID: 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
	assert.Contains(t, err.Error(), "internal server error")
	assert.NotContains(t, err.Error(), srv.URL, "errors must not leak the webhook URL")
}

func TestJSONTemplateHeadersAreSecretsNotInError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	secretHeader := "Bearer super-secret-token-do-not-leak"
	cfg := fmt.Sprintf(`{"url": %q, "headers": {"Authorization": %q}, "body_template": "{\"x\": 1}"}`, srv.URL, secretHeader)
	n, err := NewJSONTemplate(json.RawMessage(cfg))
	require.NoError(t, err)

	err = n.Send(context.Background(), Alert{ID: 1})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secretHeader, "errors must not leak header secrets")
}

func TestJSONTemplateTemplateExecutionError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Template references a non-existent field
	cfg := fmt.Sprintf(`{"url": %q, "body_template": "{\"x\": \"{{.NonExistentField}}\"}"}`, srv.URL)
	n, err := NewJSONTemplate(json.RawMessage(cfg))
	require.NoError(t, err)

	// Template parses but fails at execution - should return error
	err = n.Send(context.Background(), Alert{ID: 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "render body")
}

func TestJSONTemplateComplexTemplate(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Use a template without Format to avoid quote escaping issues in JSON
	templateStr := `{"monitor":"{{.MonitorName}}","event_id":"{{.EventID}}","contract":"{{.ContractID}}","ledger":{{.Ledger}},"tx_hash":"{{.TxHash}}","event_name":"{{.EventName}}","rule_type":"{{.RuleType}}","rule_id":{{.RuleID}},"monitor_id":{{.MonitorID}},"alert_id":{{.ID}},"created_at_unix":{{.CreatedAt.Unix}}}`

	cfg := map[string]any{
		"url":           srv.URL,
		"body_template": templateStr,
	}
	cfgBytes, err := json.Marshal(cfg)
	require.NoError(t, err)

	n, err := NewJSONTemplate(cfgBytes)
	require.NoError(t, err)

	alert := Alert{
		ID:          100,
		MonitorID:   5,
		MonitorName: "Test Monitor",
		RuleID:      10,
		RuleType:    "event_emitted",
		EventID:     "evt-456",
		ContractID:  "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAH",
		EventName:   "swap",
		Ledger:      1234567,
		TxHash:      "abc123",
		CreatedAt:   mustParseTime(t, "2026-08-15T14:30:00Z"),
	}

	require.NoError(t, n.Send(context.Background(), alert))

	var sent map[string]any
	require.NoError(t, json.Unmarshal(gotBody, &sent))
	assert.Equal(t, "Test Monitor", sent["monitor"])
	assert.Equal(t, "evt-456", sent["event_id"])
	assert.Equal(t, "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAH", sent["contract"])
	assert.Equal(t, float64(1234567), sent["ledger"])
	assert.Equal(t, "abc123", sent["tx_hash"])
	assert.Equal(t, "swap", sent["event_name"])
	assert.Equal(t, "event_emitted", sent["rule_type"])
	assert.Equal(t, float64(10), sent["rule_id"])
	assert.Equal(t, float64(5), sent["monitor_id"])
	assert.Equal(t, float64(100), sent["alert_id"])
	// Just verify the timestamp is a reasonable Unix timestamp (not zero)
	createdAtUnix := sent["created_at_unix"].(float64)
	assert.True(t, createdAtUnix > 1000000000, "timestamp should be a valid Unix time")
	assert.True(t, createdAtUnix < 2000000000, "timestamp should be a valid Unix time")
}

// mustParseTime parses a time string for test setup; panics on error.
func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return tm
}