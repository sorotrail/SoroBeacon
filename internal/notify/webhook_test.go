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

func TestWebhookSignsPayload(t *testing.T) {
	var gotBody []byte
	var gotSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotSig = r.Header.Get(SignatureHeader)
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`{"url": %q, "secret": "s3cret"}`, srv.URL)
	n, err := NewWebhook(json.RawMessage(cfg))
	require.NoError(t, err)

	alert := Alert{ID: 1, MonitorName: "m", EventID: "e1"}
	require.NoError(t, n.Send(context.Background(), alert))

	assert.Equal(t, Sign("s3cret", gotBody), gotSig, "signature must cover the exact body sent")

	var sent Alert
	require.NoError(t, json.Unmarshal(gotBody, &sent))
	assert.Equal(t, alert.EventID, sent.EventID)
}

func TestWebhookNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()

	n, err := NewWebhook(json.RawMessage(fmt.Sprintf(`{"url": %q, "secret": "x"}`, srv.URL)))
	require.NoError(t, err)

	err = n.Send(context.Background(), Alert{ID: 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
	assert.NotContains(t, err.Error(), srv.URL, "errors must not leak the webhook URL")
}

func TestWebhookConfigValidation(t *testing.T) {
	_, err := NewWebhook(json.RawMessage(`{"secret": "x"}`))
	assert.Error(t, err)
	_, err = NewWebhook(json.RawMessage(`{"url": "https://example.com"}`))
	assert.Error(t, err)
}
