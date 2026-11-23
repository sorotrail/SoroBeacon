package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/store"
)

func TestRedactURLError(t *testing.T) {
	t.Run("url_error", func(t *testing.T) {
		secretPath := "/secret-path?token=12345"
		uerr := &url.Error{
			Op:  "Post",
			URL: "https://example.com" + secretPath,
			Err: errors.New("underlying connection error"),
		}

		err := redactURLError(uerr)
		if err == nil {
			t.Fatal("expected an error, got nil")
		}

		errStr := err.Error()
		if strings.Contains(errStr, secretPath) {
			t.Errorf("error string should not contain the secret path. Got: %s", errStr)
		}
		if strings.Contains(errStr, "12345") {
			t.Errorf("error string should not contain the token. Got: %s", errStr)
		}
	})

	t.Run("nil_error", func(t *testing.T) {
		var uerr error = nil //nolint:revive // explicit nil for testing
		err := redactURLError(uerr)
		if err != nil {
			t.Errorf("expected nil, got: %v", err)
		}
	})

	t.Run("non_url_error", func(t *testing.T) {
		origErr := errors.New("just a standard error")
		err := redactURLError(origErr)
		if err != origErr {
			t.Errorf("expected original error, got: %v", err)
		}
	})
}

func TestPostJSON(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		headersReceived := make(http.Header)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			headersReceived = r.Header
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		headers := map[string]string{
			"X-Custom-Header": "TestValue",
			"Authorization":   "Bearer token",
		}

		err := postJSON(context.Background(), srv.URL, []byte(`{"msg":"hello"}`), headers)
		if err != nil {
			t.Errorf("expected no error, got: %v", err)
		}

		if headersReceived.Get("X-Custom-Header") != "TestValue" {
			t.Errorf("expected X-Custom-Header=TestValue, got %q", headersReceived.Get("X-Custom-Header"))
		}
		if headersReceived.Get("Authorization") != "Bearer token" {
			t.Errorf("expected Authorization=Bearer token, got %q", headersReceived.Get("Authorization"))
		}
	})

	t.Run("http_failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "internal error details")
		}))
		defer srv.Close()

		err := postJSON(context.Background(), srv.URL, []byte(`{}`), nil)
		if err == nil {
			t.Fatal("expected error for non-2xx response, got nil")
		}
		if !strings.Contains(err.Error(), "status 500") {
			t.Errorf("expected error to contain 'status 500', got: %v", err)
		}
		if !strings.Contains(err.Error(), "internal error details") {
			t.Errorf("expected error to contain response body, got: %v", err)
		}
	})

	t.Run("transport_failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
		srv.Close() // Close before using to cause failure

		err := postJSON(context.Background(), srv.URL, []byte(`{}`), nil)
		if err == nil {
			t.Fatal("expected error for transport failure, got nil")
		}
		if strings.Contains(err.Error(), srv.URL) {
			t.Errorf("expected error to not contain URL, got: %v", err)
		}
	})

	t.Run("context_cancelled", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
		defer srv.Close()

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		err := postJSON(ctx, srv.URL, []byte(`{}`), nil)
		if err == nil {
			t.Fatal("expected error for cancelled context, got nil")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled error, got: %v", err)
		}
	})
}

// TestHangingServer_CutOffAtDeadline proves that a server which accepts a
// connection and sleeps past the configured deadline is cut off at the deadline,
// and does not block the worker indefinitely.
func TestHangingServer_CutOffAtDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Accept and hang past the configured client timeout.
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := postJSON(ctx, srv.URL, []byte(`{"hello":"world"}`), nil)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Less(t, elapsed, 250*time.Millisecond, "must abort at the deadline rather than wait for the hanging server")
	assert.Contains(t, err.Error(), "deadline exceeded")
}

// TestHangingResponseBody_CutOffAtDeadline proves that the timeout is honoured
// on the whole delivery including reading the response body, not just establishing
// the connection. A server that writes 200 headers and then hangs mid-body is
// aborted at the configured deadline and treated as a delivery failure.
func TestHangingResponseBody_CutOffAtDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Send 200 OK headers immediately, flush them, and then hang while
		// the client attempts to drain or read the body.
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := postJSON(ctx, srv.URL, []byte(`{"hello":"world"}`), nil)
	elapsed := time.Since(start)

	require.Error(t, err, "hanging response body must return an error")
	assert.Less(t, elapsed, 250*time.Millisecond, "must abort at the deadline while reading response body")
	assert.Contains(t, err.Error(), "deadline exceeded")
}

// TestHangingErrorResponseBody_CutOffAtDeadline proves that non-2xx responses
// whose error bodies hang are also cut off by the deadline.
func TestHangingErrorResponseBody_CutOffAtDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := postJSON(ctx, srv.URL, []byte(`{}`), nil)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Less(t, elapsed, 250*time.Millisecond)
	assert.Contains(t, err.Error(), "deadline exceeded")
}

type roundTripperFunc func(req *http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// TestDefaultTimeoutAppliedWhenUnset proves that bare contexts without a deadline
// receive DefaultChannelTimeout rather than running unbounded.
func TestDefaultTimeoutAppliedWhenUnset(t *testing.T) {
	oldTransport := httpClient.Transport
	defer func() { httpClient.Transport = oldTransport }()

	var gotDeadline bool
	var remaining time.Duration
	httpClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		deadline, ok := req.Context().Deadline()
		gotDeadline = ok
		if ok {
			remaining = time.Until(deadline)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("{}")),
			Header:     make(http.Header),
		}, nil
	})

	err := postJSON(context.Background(), "http://example.com", []byte(`{}`), nil)
	require.NoError(t, err)
	assert.True(t, gotDeadline, "context must have a deadline applied")
	assert.True(t, remaining > 10*time.Second && remaining <= store.DefaultChannelTimeout, "remaining time must be close to DefaultChannelTimeout")
}

// TestNoNewHTTPClientPerDelivery verifies that httpClient is a package-level
// singleton sharing its transport across deliveries so connections are reused.
func TestNoNewHTTPClientPerDelivery(t *testing.T) {
	require.NotNil(t, httpClient)
	require.NotNil(t, httpClient.Transport)

	clientRef := httpClient
	for i := 0; i < 3; i++ {
		assert.Same(t, clientRef, httpClient, "httpClient instance must be reused across deliveries")
	}
}

// TestWebhookChannelHonorsCustomTimeout runs an end-to-end delivery through
// the Webhook notifier verifying that the deadline set on ctx is honoured.
func TestWebhookChannelHonorsCustomTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh, err := NewWebhook([]byte(fmt.Sprintf(`{"url": %q, "secret": "s"}`, srv.URL)))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = wh.Send(ctx, Alert{ID: 1, MonitorName: "mon"})
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Less(t, elapsed, 250*time.Millisecond)
	assert.Contains(t, err.Error(), "deadline exceeded")
}

// TestParseTimeout validates the range bounds and parsing dialect.
func TestParseTimeout(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{"empty defaults to 15s", `""`, store.DefaultChannelTimeout, false},
		{"null defaults to 15s", `null`, store.DefaultChannelTimeout, false},
		{"integer 15 seconds", `15`, 15 * time.Second, false},
		{"integer 1 second (floor)", `1`, 1 * time.Second, false},
		{"integer 60 seconds (ceiling)", `60`, 60 * time.Second, false},
		{"duration string 15s", `"15s"`, 15 * time.Second, false},
		{"duration string 1m", `"1m"`, 60 * time.Second, false},
		{"bare numeric string", `"30"`, 30 * time.Second, false},
		{"zero rejected", `0`, 0, true},
		{"negative integer rejected", `-5`, 0, true},
		{"negative duration rejected", `"-5s"`, 0, true},
		{"exceeds ceiling integer (600s)", `600`, 0, true},
		{"exceeds ceiling duration (10m)", `"10m"`, 0, true},
		{"garbage string rejected", `"foo"`, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := store.ParseTimeout([]byte(tt.raw))
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
		})
	}
}
