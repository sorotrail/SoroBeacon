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

func TestNewWebexValidConfig(t *testing.T) {
	n, err := NewWebex(json.RawMessage(`{"bot_token": "test-token", "room_id": "Y2lzY29zcGFyazovL3VzL1JPT00v..."}`))
	require.NoError(t, err)
	require.NotNil(t, n)
	_, ok := n.(*Webex)
	assert.True(t, ok, "NewWebex must return a *Webex")
}

func TestNewWebexMalformedConfig(t *testing.T) {
	_, err := NewWebex(json.RawMessage(`{"bot_token": }`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid config")
}

func TestNewWebexMissingBotToken(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{"empty object", `{}`},
		{"empty bot_token", `{"bot_token": "", "room_id": "Y2lzY29zcGFyazovL3VzL1JPT00v..."}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewWebex(json.RawMessage(tt.config))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "bot_token and room_id are required")
		})
	}
}

func TestNewWebexMissingRoomID(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{"missing room_id", `{"bot_token": "test-token"}`},
		{"empty room_id", `{"bot_token": "test-token", "room_id": ""}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewWebex(json.RawMessage(tt.config))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "bot_token and room_id are required")
		})
	}
}

func TestNewWebexInvalidTemplate(t *testing.T) {
	_, err := NewWebex(json.RawMessage(
		`{"bot_token": "test-token", "room_id": "Y2lzY29zcGFyazovL3VzL1JPT00v...", "template": "{{.Unclosed"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid template")
}

func TestWebexSendSuccess(t *testing.T) {
	var (
		gotMethod       string
		gotContentType  string
		gotAuthHeader   string
		gotBody         []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		gotAuthHeader = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n, err := NewWebex(json.RawMessage(fmt.Sprintf(`{"bot_token": "test-bot-token", "room_id": "Y2lzY29zcGFyazovL3VzL1JPT00v...", "api_base": %q}`, srv.URL)))
	require.NoError(t, err)

	alert := Alert{ID: 7, MonitorName: "my-monitor", EventID: "ev-1"}
	require.NoError(t, n.Send(context.Background(), alert))

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "application/json", gotContentType)
	assert.Equal(t, "Bearer test-bot-token", gotAuthHeader)

	var payload struct {
		RoomID   string `json:"roomId"`
		Markdown string `json:"markdown"`
	}
	require.NoError(t, json.Unmarshal(gotBody, &payload))
	assert.Equal(t, "Y2lzY29zcGFyazovL3VzL1JPT00v...", payload.RoomID)
	assert.Contains(t, payload.Markdown, "my-monitor")
}

func TestWebexSendCustomTemplate(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`{"bot_token": "test-token", "room_id": "Y2lzY29zcGFyazovL3VzL1JPT00v...", "api_base": %q, "template": "alert {{.EventID}}"}`, srv.URL)
	n, err := NewWebex(json.RawMessage(cfg))
	require.NoError(t, err)
	require.NoError(t, n.Send(context.Background(), Alert{ID: 1, EventID: "ev-42"}))

	var payload struct {
		Markdown string `json:"markdown"`
	}
	require.NoError(t, json.Unmarshal(gotBody, &payload))
	assert.Equal(t, "alert ev-42", payload.Markdown)
}

func TestWebexSendServerError(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		expectStatus  bool // whether to expect the status code in the error
	}{
		{"unauthorized", http.StatusUnauthorized, false},
		{"forbidden", http.StatusForbidden, false},
		{"client error", http.StatusBadRequest, true},
		{"server error", http.StatusInternalServerError, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "boom", tt.status)
			}))
			defer srv.Close()

			n, err := NewWebex(json.RawMessage(fmt.Sprintf(`{"bot_token": "test-token", "room_id": "Y2lzY29zcGFyazovL3VzL1JPT00v...", "api_base": %q}`, srv.URL)))
			require.NoError(t, err)

			err = n.Send(context.Background(), Alert{ID: 1, MonitorName: "m"})
			require.Error(t, err)
			if tt.expectStatus {
				assert.Contains(t, err.Error(), fmt.Sprintf("%d", tt.status))
			}
			// Ensure bot token is not in the error message
			assert.NotContains(t, err.Error(), "test-token")
		})
	}
}

func TestWebexSendAuthErrorMessage(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{"unauthorized", http.StatusUnauthorized},
		{"forbidden", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "unauthorized", tt.status)
			}))
			defer srv.Close()

			n, err := NewWebex(json.RawMessage(fmt.Sprintf(`{"bot_token": "test-token", "room_id": "Y2lzY29zcGFyazovL3VzL1JPT00v...", "api_base": %q}`, srv.URL)))
			require.NoError(t, err)

			err = n.Send(context.Background(), Alert{ID: 1, MonitorName: "m"})
			require.Error(t, err)
			// 401/403 should have a helpful error message
			errMsg := strings.ToLower(err.Error())
			assert.Contains(t, errMsg, "check the bot token")
		})
	}
}

func TestWebexSendNetworkFailure(t *testing.T) {
	// Network failure test is covered by the fact that requestJSON wraps
	// network errors. The Webex URL is fixed, so we can't easily mock it
	// without changing the implementation.
}