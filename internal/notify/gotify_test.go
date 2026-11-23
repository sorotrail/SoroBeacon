package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type gotifyRequest struct {
	Title    string `json:"title"`
	Message  string `json:"message"`
	Priority int    `json:"priority"`
}

func TestNewGotifyValidConfig(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{"minimal", `{"server_url": "https://gotify.example.com", "token": "app-token-123"}`},
		{"with priority", `{"server_url": "https://gotify.example.com", "token": "app-token-123", "priority": 8}`},
		{"with trailing slash", `{"server_url": "https://gotify.example.com/", "token": "app-token-123"}`},
		{"http scheme", `{"server_url": "http://localhost:8080", "token": "app-token-123"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, err := NewGotify(json.RawMessage(tt.config))
			require.NoError(t, err)
			require.NotNil(t, n)
			_, ok := n.(*Gotify)
			assert.True(t, ok, "NewGotify must return a *Gotify")
		})
	}
}

func TestNewGotifyMalformedConfig(t *testing.T) {
	_, err := NewGotify(json.RawMessage(`{"server_url": }`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid config")
}

func TestNewGotifyMissingServerURL(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{"empty object", `{}`},
		{"empty server_url", `{"server_url": "", "token": "t"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewGotify(json.RawMessage(tt.config))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "server_url is required")
		})
	}
}

func TestNewGotifyMissingToken(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{"empty token", `{"server_url": "https://gotify.example.com", "token": ""}`},
		{"no token key", `{"server_url": "https://gotify.example.com"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewGotify(json.RawMessage(tt.config))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "token is required")
		})
	}
}

func TestNewGotifyInvalidServerURLScheme(t *testing.T) {
	_, err := NewGotify(json.RawMessage(`{"server_url": "ftp://gotify.example.com", "token": "t"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must use HTTP or HTTPS")
}

func TestNewGotifyPriorityDefaults(t *testing.T) {
	n, err := NewGotify(json.RawMessage(`{"server_url": "https://gotify.example.com", "token": "t"}`))
	require.NoError(t, err)
	_ = n
	// We can't directly access cfg.Priority, but we can verify via send
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Re-create with test server URL
	n, err = NewGotify(json.RawMessage(fmt.Sprintf(`{"server_url": %q, "token": "t"}`, srv.URL)))
	require.NoError(t, err)

	require.NoError(t, n.Send(context.Background(), Alert{ID: 1, MonitorName: "m", RuleType: "r", ContractID: "c", EventID: "e"}))

	var payload gotifyRequest
	require.NoError(t, json.Unmarshal(gotBody, &payload))
	assert.Equal(t, defaultGotifyPriority, payload.Priority, "priority must default to 5")
}

func TestNewGotifyCustomPriority(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n, err := NewGotify(json.RawMessage(fmt.Sprintf(`{"server_url": %q, "token": "t", "priority": 10}`, srv.URL)))
	require.NoError(t, err)

	require.NoError(t, n.Send(context.Background(), Alert{ID: 1, MonitorName: "m", RuleType: "r", ContractID: "c", EventID: "e"}))

	var payload gotifyRequest
	require.NoError(t, json.Unmarshal(gotBody, &payload))
	assert.Equal(t, 10, payload.Priority)
}

func TestGotifyURLNormalization(t *testing.T) {
	tests := []struct {
		name       string
		inputURL   string
		expectPath string
	}{
		{"no trailing slash", "http://gotify.example.com", "/message"},
		{"with trailing slash", "http://gotify.example.com/", "/message"},
		{"with path", "http://gotify.example.com/gotify", "/gotify/message"},
		{"with path and slash", "http://gotify.example.com/gotify/", "/gotify/message"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotURL string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotURL = r.URL.String()
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			// Replace the host in the test URL with the test server
			testURL := strings.Replace(tt.inputURL, "gotify.example.com", strings.TrimPrefix(srv.URL, "http://"), 1)

			n, err := NewGotify(json.RawMessage(fmt.Sprintf(`{"server_url": %q, "token": "t"}`, testURL)))
			require.NoError(t, err)

			require.NoError(t, n.Send(context.Background(), Alert{ID: 1, MonitorName: "m", RuleType: "r", ContractID: "c", EventID: "e"}))

			assert.Equal(t, tt.expectPath+"?token=t", gotURL)
		})
	}
}

func TestGotifySendSuccess(t *testing.T) {
	var (
		gotMethod      string
		gotContentType string
		gotBody        []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n, err := NewGotify(json.RawMessage(fmt.Sprintf(`{"server_url": %q, "token": "app-token-secret"}`, srv.URL)))
	require.NoError(t, err)

	alert := Alert{
		ID:          7,
		MonitorName: "my-monitor",
		RuleType:    "event_emitted",
		RuleID:      3,
		ContractID:  "CABC123",
		EventID:     "ev-1",
		EventName:   "transfer",
		Ledger:      3721765,
		TxHash:      "4f2a...",
	}
	require.NoError(t, n.Send(context.Background(), alert))

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "application/json", gotContentType)

	var payload gotifyRequest
	require.NoError(t, json.Unmarshal(gotBody, &payload))
	assert.Equal(t, "SoroBeacon Alert", payload.Title)
	assert.Contains(t, payload.Message, "my-monitor")
	assert.Contains(t, payload.Message, "ev-1")
	assert.Contains(t, payload.Message, "transfer")
	assert.Equal(t, defaultGotifyPriority, payload.Priority)
}

func TestGotifySendServerError(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{"client error", http.StatusBadRequest},
		{"server error", http.StatusInternalServerError},
		{"unauthorized", http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "boom", tt.status)
			}))
			defer srv.Close()

			n, err := NewGotify(json.RawMessage(fmt.Sprintf(`{"server_url": %q, "token": "t"}`, srv.URL)))
			require.NoError(t, err)

			err = n.Send(context.Background(), Alert{ID: 1, MonitorName: "m", RuleType: "r", ContractID: "c", EventID: "e"})
			require.Error(t, err)
			assert.Contains(t, err.Error(), fmt.Sprintf("%d", tt.status))
		})
	}
}

func TestGotifyTokenNotLogged(t *testing.T) {
	const secretToken = "app-token-do-not-leak"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	n, err := NewGotify(json.RawMessage(fmt.Sprintf(`{"server_url": %q, "token": %q}`, srv.URL, secretToken)))
	require.NoError(t, err)

	err = n.Send(context.Background(), Alert{ID: 1, MonitorName: "m", RuleType: "r", ContractID: "c", EventID: "e"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
	assert.NotContains(t, err.Error(), secretToken, "errors must never leak the token")
	assert.NotContains(t, err.Error(), srv.URL, "errors must never leak the server URL with token in query")
}
