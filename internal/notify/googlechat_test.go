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

func TestNewGoogleChatValidConfig(t *testing.T) {
	n, err := NewGoogleChat(json.RawMessage(`{"webhook_url": "https://chat.googleapis.com/v1/spaces/AAAA/messages?key=...&token=..."}`))
	require.NoError(t, err)
	require.NotNil(t, n)
	_, ok := n.(*GoogleChat)
	assert.True(t, ok, "NewGoogleChat must return a *GoogleChat")
}

func TestNewGoogleChatMalformedConfig(t *testing.T) {
	_, err := NewGoogleChat(json.RawMessage(`{"webhook_url": }`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid config")
}

func TestNewGoogleChatMissingWebhookURL(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{"empty object", `{}`},
		{"empty webhook_url", `{"webhook_url": ""}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewGoogleChat(json.RawMessage(tt.config))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "webhook_url is required")
		})
	}
}

func TestNewGoogleChatNonHTTPSWebhookURL(t *testing.T) {
	_, err := NewGoogleChat(json.RawMessage(`{"webhook_url": "http://example.com/webhook"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must use HTTPS")
}

func TestGoogleChatSendSuccess(t *testing.T) {
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

	n, err := NewGoogleChat(json.RawMessage(fmt.Sprintf(`{"webhook_url": %q}`, srv.URL)))
	require.NoError(t, err)

	alert := Alert{
		ID:          7,
		MonitorName: "my-monitor",
		RuleType:    "event_emitted",
		ContractID:  "CABC123",
		EventID:     "ev-1",
		EventName:   "transfer",
	}
	require.NoError(t, n.Send(context.Background(), alert))

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "application/json", gotContentType)

	var payload googleChatCard
	require.NoError(t, json.Unmarshal(gotBody, &payload))
	require.Len(t, payload.CardsV2, 1)
	card := payload.CardsV2[0]
	assert.Equal(t, "sorobeacon-alert", card.CardID)
	assert.Equal(t, "SoroBeacon Alert", card.Card.Header.Title)
	assert.Equal(t, "my-monitor", card.Card.Header.Subtitle)
	require.Len(t, card.Card.Sections, 1)
	require.Len(t, card.Card.Sections[0].Widgets, 6)

	// Check that all expected fields are present
	labels := make(map[string]string)
	for _, w := range card.Card.Sections[0].Widgets {
		if w.KeyValue != nil {
			labels[w.KeyValue.TopLabel] = w.KeyValue.Content
		}
	}
	assert.Equal(t, "my-monitor", labels["Monitor"])
	assert.Equal(t, "event_emitted", labels["Rule Type"])
	assert.Equal(t, "CABC123", labels["Contract ID"])
	assert.Equal(t, "ev-1", labels["Event ID"])
	assert.Contains(t, labels["Timestamp"], "UTC")
	assert.Equal(t, "transfer", labels["Event"])
}

func TestGoogleChatSendServerError(t *testing.T) {
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

			n, err := NewGoogleChat(json.RawMessage(fmt.Sprintf(`{"webhook_url": %q}`, srv.URL)))
			require.NoError(t, err)

			err = n.Send(context.Background(), Alert{ID: 1, MonitorName: "m", RuleType: "r", ContractID: "c", EventID: "e"})
			require.Error(t, err)
			assert.Contains(t, err.Error(), fmt.Sprintf("%d", tt.status))
		})
	}
}

func TestGoogleChatSendNetworkFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	n, err := NewGoogleChat(json.RawMessage(fmt.Sprintf(`{"webhook_url": %q}`, url)))
	require.NoError(t, err)

	err = n.Send(context.Background(), Alert{ID: 1, MonitorName: "m", RuleType: "r", ContractID: "c", EventID: "e"})
	require.Error(t, err)
}

func TestGoogleChatWebhookURLNotLogged(t *testing.T) {
	const secretWebhook = "https://chat.googleapis.com/v1/spaces/AAAA/messages?key=secret&token=secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	// Use the real secret URL in config but point to test server
	n, err := NewGoogleChat(json.RawMessage(fmt.Sprintf(`{"webhook_url": %q}`, srv.URL)))
	require.NoError(t, err)

	err = n.Send(context.Background(), Alert{ID: 1, MonitorName: "m", RuleType: "r", ContractID: "c", EventID: "e"})
	require.Error(t, err)
	// The error should contain the status code but NOT the webhook URL
	assert.Contains(t, err.Error(), "401")
	assert.NotContains(t, err.Error(), srv.URL)
	assert.NotContains(t, err.Error(), secretWebhook)
}
