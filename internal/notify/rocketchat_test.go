package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRocketChatRejectsBadWebhookURL(t *testing.T) {
	tests := []struct {
		name   string
		config string
		want   string
	}{
		{"missing", `{}`, "webhook_url is required"},
		{"empty", `{"webhook_url": ""}`, "webhook_url is required"},
		{"plain http", `{"webhook_url": "http://chat.example.com/hooks/supersecret/token"}`, "must use HTTPS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewRocketChat(json.RawMessage(tt.config))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			assert.NotContains(t, err.Error(), "supersecret")
		})
	}
}

func TestNewRocketChatMalformedConfig(t *testing.T) {
	_, err := NewRocketChat(json.RawMessage(`{"webhook_url": }`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid config")
}

func TestRocketChatSendPayload(t *testing.T) {
	var got []byte
	srv := captureServer(t, http.StatusOK, &got)

	n, err := NewRocketChat(json.RawMessage(fmt.Sprintf(
		`{"webhook_url": %q, "alias": "SoroBeacon", "emoji": ":satellite:"}`, srv.URL)))
	require.NoError(t, err)
	require.NoError(t, n.Send(context.Background(), testChatAlert))

	var payload rocketChatPayload
	require.NoError(t, json.Unmarshal(got, &payload))
	assert.Contains(t, payload.Text, "treasury-watch")
	assert.Contains(t, payload.Text, "0000123-0000000001")
	assert.Equal(t, "SoroBeacon", payload.Alias)
	assert.Equal(t, ":satellite:", payload.Emoji)

	require.Len(t, payload.Attachments, 1)
	fields := map[string]string{}
	for _, f := range payload.Attachments[0].Fields {
		fields[f.Title] = f.Value
	}
	assert.Equal(t, "treasury-watch", fields["Monitor"])
	assert.Equal(t, "0000123-0000000001", fields["Event ID"])
	assert.Equal(t, "CABC123", fields["Contract ID"])
	assert.Equal(t, "4242", fields["Ledger"])
}

func TestRocketChatOptionalFieldsOmitted(t *testing.T) {
	var got []byte
	srv := captureServer(t, http.StatusOK, &got)

	n, err := NewRocketChat(json.RawMessage(fmt.Sprintf(`{"webhook_url": %q}`, srv.URL)))
	require.NoError(t, err)
	require.NoError(t, n.Send(context.Background(), testChatAlert))

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(got, &raw))
	assert.NotContains(t, raw, "alias")
	assert.NotContains(t, raw, "emoji")
	assert.Contains(t, raw, "attachments")
}

func TestRocketChatSendNon2xxReportsStatusOnly(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A misbehaving integration script echoing the request path back.
		http.Error(w, "bad hook "+r.URL.Path, http.StatusBadRequest)
	}))
	defer srv.Close()
	useServerClient(t, srv)

	n, err := NewRocketChat(json.RawMessage(fmt.Sprintf(`{"webhook_url": %q}`, srv.URL+"/hooks/supersecret/token")))
	require.NoError(t, err)

	err = n.Send(context.Background(), testChatAlert)
	require.Error(t, err)
	assert.Equal(t, "rocketchat: status 400", err.Error())
}
