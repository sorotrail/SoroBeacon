package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSignGoldenVector pins the canonical signing string and digest for a
// fixed secret, timestamp and body. Receivers reimplementing verification in
// another language can check themselves against these exact bytes:
//
//	HMAC-SHA256(key="s3cret", msg="1700000000." + `{"event_id":"e1"}`)
func TestSignGoldenVector(t *testing.T) {
	const want = "b7ae34b78dfdf65e437098c4dc42d89f96f5dd9152180c66b3c5aec58df53b7a"
	got := Sign("s3cret", "1700000000", []byte(`{"event_id":"e1"}`))
	assert.Equal(t, want, got)
}

func TestWebhookSignsTimestampAndBody(t *testing.T) {
	var gotBody []byte
	var gotSig, gotTS string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotSig = r.Header.Get(SignatureHeader)
		gotTS = r.Header.Get(TimestampHeader)
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`{"url": %q, "secret": "s3cret"}`, srv.URL)
	n, err := NewWebhook(json.RawMessage(cfg))
	require.NoError(t, err)

	alert := Alert{ID: 1, MonitorName: "m", EventID: "e1"}
	require.NoError(t, n.Send(context.Background(), alert))

	// The timestamp must be a sane Unix time and, crucially, the signature
	// must be computed over "<timestamp>.<body>" — recomputing from the
	// delivered header is exactly what a receiver does.
	ts, err := strconv.ParseInt(gotTS, 10, 64)
	require.NoError(t, err, "timestamp header must be decimal Unix seconds")
	assert.WithinDuration(t, time.Unix(ts, 0), time.Now(), time.Minute)
	assert.Equal(t, Sign("s3cret", gotTS, gotBody), gotSig)

	var sent Alert
	require.NoError(t, json.Unmarshal(gotBody, &sent))
	assert.Equal(t, alert.EventID, sent.EventID)
}

func TestWebhookRotationSendsBothSignatures(t *testing.T) {
	var gotBody []byte
	var gotSig, gotPrevSig, gotTS string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotSig = r.Header.Get(SignatureHeader)
		gotPrevSig = r.Header.Get(SignaturePreviousHeader)
		gotTS = r.Header.Get(TimestampHeader)
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`{"url": %q, "secret": "new-secret", "previous_secret": "old-secret"}`, srv.URL)
	n, err := NewWebhook(json.RawMessage(cfg))
	require.NoError(t, err)
	require.NoError(t, n.Send(context.Background(), Alert{ID: 1, EventID: "e1"}))

	// A receiver still on the old key verifies the previous header; one on
	// the new key verifies the primary header. Both cover the same bytes.
	assert.Equal(t, Sign("new-secret", gotTS, gotBody), gotSig)
	assert.Equal(t, Sign("old-secret", gotTS, gotBody), gotPrevSig)
	assert.NotEqual(t, gotSig, gotPrevSig)
}

func TestWebhookOmitsPreviousSignatureWithoutPreviousSecret(t *testing.T) {
	var gotPrevSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPrevSig = r.Header.Get(SignaturePreviousHeader)
	}))
	defer srv.Close()

	n, err := NewWebhook(json.RawMessage(fmt.Sprintf(`{"url": %q, "secret": "x"}`, srv.URL)))
	require.NoError(t, err)
	require.NoError(t, n.Send(context.Background(), Alert{ID: 1}))

	assert.Empty(t, gotPrevSig, "no previous signature unless previous_secret is configured")
}

func TestWebhookNon2xxIsError(t *testing.T) {
	const secret = "super-secret-do-not-leak"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()

	n, err := NewWebhook(json.RawMessage(fmt.Sprintf(`{"url": %q, "secret": %q}`, srv.URL, secret)))
	require.NoError(t, err)

	err = n.Send(context.Background(), Alert{ID: 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
	assert.NotContains(t, err.Error(), srv.URL, "errors must not leak the webhook URL")
	assert.NotContains(t, err.Error(), secret, "errors must not leak the webhook secret")
}

func TestWebhookConfigValidation(t *testing.T) {
	_, err := NewWebhook(json.RawMessage(`{"secret": "x"}`))
	assert.Error(t, err)
	_, err = NewWebhook(json.RawMessage(`{"url": "https://example.com"}`))
	assert.Error(t, err)
	// previous_secret is optional and alone does not satisfy the secret requirement.
	_, err = NewWebhook(json.RawMessage(`{"url": "https://example.com", "previous_secret": "old"}`))
	assert.Error(t, err)
}
