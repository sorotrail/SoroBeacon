package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type opsgenieRequest struct {
	Message     string            `json:"message"`
	Alias       string            `json:"alias"`
	Description string            `json:"description"`
	Priority    string            `json:"priority"`
	Details     map[string]string `json:"details"`
	Responders  []string          `json:"responders,omitempty"`
}

func TestNewOpsgenieValidConfig(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{"minimal", `{"api_key": "geniekey-123"}`},
		{"with region us", `{"api_key": "geniekey-123", "region": "us"}`},
		{"with region eu", `{"api_key": "geniekey-123", "region": "eu"}`},
		{"with priority", `{"api_key": "geniekey-123", "priority": "P1"}`},
		{"with responders", `{"api_key": "geniekey-123", "responders": ["team:payments", "user:ops"]}`},
		{"full config", `{"api_key": "geniekey-123", "region": "eu", "priority": "P2", "responders": ["team:payments"]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, err := NewOpsgenie(json.RawMessage(tt.config))
			require.NoError(t, err)
			require.NotNil(t, n)
			_, ok := n.(*Opsgenie)
			assert.True(t, ok, "NewOpsgenie must return a *Opsgenie")
		})
	}
}

func TestNewOpsgenieMalformedConfig(t *testing.T) {
	_, err := NewOpsgenie(json.RawMessage(`{"api_key": }`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid config")
}

func TestNewOpsgenieMissingAPIKey(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{"empty object", `{}`},
		{"empty api_key", `{"api_key": ""}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewOpsgenie(json.RawMessage(tt.config))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "api_key is required")
		})
	}
}

func TestNewOpsgenieInvalidRegion(t *testing.T) {
	_, err := NewOpsgenie(json.RawMessage(`{"api_key": "key", "region": "asia"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid region")
	assert.Contains(t, err.Error(), "us or eu")
}

func TestNewOpsgenieInvalidPriority(t *testing.T) {
	_, err := NewOpsgenie(json.RawMessage(`{"api_key": "key", "priority": "P0"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid priority")
	assert.Contains(t, err.Error(), "P1..P5")
}

func TestOpsgenieSendSuccess(t *testing.T) {
	var (
		gotMethod      string
		gotContentType string
		gotBody        []byte
		gotAuthHeader  string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		gotAuthHeader = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	n, err := NewOpsgenie(json.RawMessage(fmt.Sprintf(`{"api_key": "geniekey-secret", "region": "us", "api_base": %q}`, srv.URL)))
	require.NoError(t, err)

	alert := Alert{
		ID:          7,
		MonitorID:   9,
		MonitorName: "my-monitor",
		RuleID:      3,
		RuleType:    "event_emitted",
		ContractID:  "CABC123",
		EventID:     "ev-1",
		EventName:   "transfer",
		Ledger:      3721765,
		TxHash:      "4f2a...",
	}
	require.NoError(t, n.Send(context.Background(), alert))

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "application/json", gotContentType)
	assert.Equal(t, "GenieKey geniekey-secret", gotAuthHeader)

	var payload opsgenieRequest
	require.NoError(t, json.Unmarshal(gotBody, &payload))
	assert.Equal(t, "SoroBeacon alert: my-monitor", payload.Message)
	assert.Equal(t, "sorobeacon-3-ev-1", payload.Alias, "alias must be deterministic for (rule_id, event_id)")
	assert.Equal(t, "P3", payload.Priority, "priority defaults to P3")
	assert.Contains(t, payload.Description, "my-monitor")
	assert.Contains(t, payload.Description, "transfer")
	assert.Equal(t, "9", payload.Details["monitor_id"])
	assert.Equal(t, "my-monitor", payload.Details["monitor_name"])
	assert.Equal(t, "3", payload.Details["rule_id"])
	assert.Equal(t, "event_emitted", payload.Details["rule_type"])
	assert.Equal(t, "ev-1", payload.Details["event_id"])
	assert.Equal(t, "CABC123", payload.Details["contract_id"])
	assert.Equal(t, "transfer", payload.Details["event_name"])
	assert.Equal(t, "3721765", payload.Details["ledger"])
	assert.Equal(t, "4f2a...", payload.Details["tx_hash"])
}

func TestOpsgenieSendWithCustomPriorityAndResponders(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	n, err := NewOpsgenie(json.RawMessage(fmt.Sprintf(`{"api_key": "key", "priority": "P1", "responders": ["team:payments", "user:john"], "api_base": %q}`, srv.URL)))
	require.NoError(t, err)

	require.NoError(t, n.Send(context.Background(), Alert{ID: 1, MonitorName: "m", RuleID: 2, EventID: "e"}))

	var payload opsgenieRequest
	require.NoError(t, json.Unmarshal(gotBody, &payload))
	assert.Equal(t, "P1", payload.Priority)
	assert.Equal(t, []string{"team:payments", "user:john"}, payload.Responders)
}

func TestOpsgenieRegionSelection(t *testing.T) {
	tests := []struct {
		name     string
		region   string
		expected string
	}{
		{"us region", "us", "https://api.opsgenie.com"},
		{"eu region", "eu", "https://api.eu.opsgenie.com"},
		{"default region", "", "https://api.opsgenie.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := fmt.Sprintf(`{"api_key": "key", "region": %q}`, tt.region)
			if tt.region == "" {
				config = `{"api_key": "key"}`
			}
			n, err := NewOpsgenie(json.RawMessage(config))
			require.NoError(t, err)
			o := n.(*Opsgenie)
			assert.Equal(t, tt.expected, o.cfg.baseURL)
		})
	}
}

func TestOpsgenieDedupAlias(t *testing.T) {
	var aliases []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req opsgenieRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		aliases = append(aliases, req.Alias)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	n, err := NewOpsgenie(json.RawMessage(fmt.Sprintf(`{"api_key": "key", "api_base": %q}`, srv.URL)))
	require.NoError(t, err)

	alert := Alert{ID: 1, RuleID: 5, EventID: "ev-42"}
	require.NoError(t, n.Send(context.Background(), alert))
	require.NoError(t, n.Send(context.Background(), alert)) // same alert again

	require.Len(t, aliases, 2)
	assert.Equal(t, "sorobeacon-5-ev-42", aliases[0])
	assert.Equal(t, "sorobeacon-5-ev-42", aliases[1], "alias must be stable for same (rule_id, event_id)")
}

func TestOpsgenieNon2xxIsErrorWithoutAPIKey(t *testing.T) {
	const secretKey = "geniekey-do-not-leak"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	n, err := NewOpsgenie(json.RawMessage(fmt.Sprintf(`{"api_key": %q, "api_base": %q}`, secretKey, srv.URL)))
	require.NoError(t, err)

	err = n.Send(context.Background(), Alert{ID: 1, MonitorName: "m", RuleID: 2, EventID: "e"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
	assert.NotContains(t, err.Error(), secretKey, "errors must never leak the api_key")
}

func TestOpsgenieAPIKeyNotInError(t *testing.T) {
	const secretKey = "geniekey-should-not-appear"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	n, err := NewOpsgenie(json.RawMessage(fmt.Sprintf(`{"api_key": %q, "api_base": %q}`, secretKey, srv.URL)))
	require.NoError(t, err)

	err = n.Send(context.Background(), Alert{ID: 1, MonitorName: "m", RuleID: 2, EventID: "e"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secretKey)
}
