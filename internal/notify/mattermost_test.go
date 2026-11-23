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

// useServerClient points the shared httpClient at srv's TLS-trusting client
// for the duration of the test, so channels that insist on https:// URLs can
// be exercised against an httptest server.
func useServerClient(t *testing.T, srv *httptest.Server) {
	t.Helper()
	orig := httpClient
	httpClient = srv.Client()
	t.Cleanup(func() { httpClient = orig })
}

// captureServer starts a TLS test server that records the last request body
// and answers with status.
func captureServer(t *testing.T, status int, body *[]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*body, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	useServerClient(t, srv)
	return srv
}

var testChatAlert = Alert{
	ID:          7,
	MonitorName: "treasury-watch",
	RuleType:    "event_match",
	EventID:     "0000123-0000000001",
	ContractID:  "CABC123",
	Ledger:      4242,
	Severity:    "critical",
}

func TestNewMattermostRejectsBadWebhookURL(t *testing.T) {
	const secretURL = "http://mattermost.example.com/hooks/supersecret"
	tests := []struct {
		name   string
		config string
		want   string
	}{
		{"missing", `{}`, "webhook_url is required"},
		{"empty", `{"webhook_url": ""}`, "webhook_url is required"},
		{"plain http", fmt.Sprintf(`{"webhook_url": %q}`, secretURL), "must use HTTPS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewMattermost(json.RawMessage(tt.config))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			assert.NotContains(t, err.Error(), "supersecret")
		})
	}
}

func TestNewMattermostMalformedConfig(t *testing.T) {
	_, err := NewMattermost(json.RawMessage(`{"webhook_url": }`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid config")
}

func TestNewMattermostInvalidTemplate(t *testing.T) {
	_, err := NewMattermost(json.RawMessage(
		`{"webhook_url": "https://mm.example.com/hooks/x", "template": "{{.Unclosed"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid template")
}

func TestMattermostSendPayload(t *testing.T) {
	var got []byte
	srv := captureServer(t, http.StatusOK, &got)

	n, err := NewMattermost(json.RawMessage(fmt.Sprintf(
		`{"webhook_url": %q, "channel": "#alerts", "username": "sorobeacon"}`, srv.URL)))
	require.NoError(t, err)
	require.NoError(t, n.Send(context.Background(), testChatAlert))

	var payload mattermostPayload
	require.NoError(t, json.Unmarshal(got, &payload))
	assert.Contains(t, payload.Text, "treasury-watch")
	assert.Contains(t, payload.Text, "0000123-0000000001")
	assert.Equal(t, "#alerts", payload.Channel)
	assert.Equal(t, "sorobeacon", payload.Username)

	require.Len(t, payload.Attachments, 1)
	att := payload.Attachments[0]
	assert.Equal(t, "#d00000", att.Color)
	fields := map[string]string{}
	for _, f := range att.Fields {
		fields[f.Title] = f.Value
	}
	assert.Equal(t, map[string]string{
		"Monitor":     "treasury-watch",
		"Rule type":   "event_match",
		"Event ID":    "0000123-0000000001",
		"Contract ID": "CABC123",
		"Ledger":      "4242",
	}, fields)
}

func TestMattermostOptionalFieldsOmitted(t *testing.T) {
	var got []byte
	srv := captureServer(t, http.StatusOK, &got)

	n, err := NewMattermost(json.RawMessage(fmt.Sprintf(`{"webhook_url": %q}`, srv.URL)))
	require.NoError(t, err)
	require.NoError(t, n.Send(context.Background(), testChatAlert))

	// Decode into a map: an empty-string override would still be a present
	// key, and Mattermost would treat it as one.
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(got, &raw))
	assert.NotContains(t, raw, "channel")
	assert.NotContains(t, raw, "username")
	assert.Contains(t, raw, "text")
}

func TestMattermostDigestHasNoAttachment(t *testing.T) {
	var got []byte
	srv := captureServer(t, http.StatusOK, &got)

	n, err := NewMattermost(json.RawMessage(fmt.Sprintf(`{"webhook_url": %q}`, srv.URL)))
	require.NoError(t, err)
	require.NoError(t, n.Send(context.Background(), Alert{Digest: "SoroBeacon digest: 3 alert(s)"}))

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(got, &raw))
	assert.NotContains(t, raw, "attachments")
}

func TestMattermostSendNon2xx(t *testing.T) {
	var got []byte
	srv := captureServer(t, http.StatusForbidden, &got)

	n, err := NewMattermost(json.RawMessage(fmt.Sprintf(`{"webhook_url": %q}`, srv.URL)))
	require.NoError(t, err)

	err = n.Send(context.Background(), testChatAlert)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 403")
	assert.NotContains(t, err.Error(), srv.URL)
}

func TestMattermostSendNetworkFailureRedactsURL(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	u := srv.URL + "/hooks/supersecret"
	srv.Close()

	n, err := NewMattermost(json.RawMessage(fmt.Sprintf(`{"webhook_url": %q}`, u)))
	require.NoError(t, err)
	err = n.Send(context.Background(), testChatAlert)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "supersecret")
}

func TestDefaultFactoryRegistersNewChannels(t *testing.T) {
	types := DefaultFactory().Types()
	for _, name := range []string{TypeMattermost, TypeRocketChat, TypeZulip, TypePushover} {
		assert.Contains(t, types, name)
	}
}
