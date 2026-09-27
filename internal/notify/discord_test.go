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

func TestNewDiscordValidConfig(t *testing.T) {
	n, err := NewDiscord(json.RawMessage(`{"webhook_url": "https://discord.com/api/webhooks/..."}`))
	require.NoError(t, err)
	require.NotNil(t, n)
	_, ok := n.(*Discord)
	assert.True(t, ok, "NewDiscord must return a *Discord")
}

func TestNewDiscordMalformedConfig(t *testing.T) {
	_, err := NewDiscord(json.RawMessage(`{"webhook_url": }`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid config")
}

func TestNewDiscordMissingWebhookURL(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{"empty object", `{}`},
		{"empty webhook_url", `{"webhook_url": ""}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewDiscord(json.RawMessage(tt.config))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "webhook_url is required")
		})
	}
}

func TestNewDiscordInvalidTemplate(t *testing.T) {
	_, err := NewDiscord(json.RawMessage(
		`{"webhook_url": "https://discord.com/api/webhooks/...", "template": "{{.Unclosed"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid template")
}

func TestDiscordSendSuccess(t *testing.T) {
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

	n, err := NewDiscord(json.RawMessage(fmt.Sprintf(`{"webhook_url": %q}`, srv.URL)))
	require.NoError(t, err)

	alert := Alert{ID: 7, MonitorName: "my-monitor", EventID: "ev-1"}
	require.NoError(t, n.Send(context.Background(), alert))

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "application/json", gotContentType)

	var payload struct {
		Content string `json:"content"`
	}
	require.NoError(t, json.Unmarshal(gotBody, &payload))
	assert.Contains(t, payload.Content, "my-monitor")
}

func TestDiscordSendCustomTemplate(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`{"webhook_url": %q, "template": "alert {{.EventID}}"}`, srv.URL)
	n, err := NewDiscord(json.RawMessage(cfg))
	require.NoError(t, err)
	require.NoError(t, n.Send(context.Background(), Alert{ID: 1, EventID: "ev-42"}))

	var payload struct {
		Content string `json:"content"`
	}
	require.NoError(t, json.Unmarshal(gotBody, &payload))
	assert.Equal(t, "alert ev-42", payload.Content)
}

func TestDiscordSendServerError(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{"client error", http.StatusBadRequest},
		{"server error", http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "boom", tt.status)
			}))
			defer srv.Close()

			n, err := NewDiscord(json.RawMessage(fmt.Sprintf(`{"webhook_url": %q}`, srv.URL)))
			require.NoError(t, err)

			err = n.Send(context.Background(), Alert{ID: 1, MonitorName: "m"})
			require.Error(t, err)
			assert.Contains(t, err.Error(), fmt.Sprintf("%d", tt.status))
		})
	}
}

func TestDiscordSendNetworkFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	n, err := NewDiscord(json.RawMessage(fmt.Sprintf(`{"webhook_url": %q}`, url)))
	require.NoError(t, err)

	err = n.Send(context.Background(), Alert{ID: 1, MonitorName: "m"})
	require.Error(t, err)
}
