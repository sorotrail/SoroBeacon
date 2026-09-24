package notify

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
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
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
