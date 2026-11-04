package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestDecodeAPIErrorAndClassification(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		body          string
		wantCode      string
		wantNotFound  bool
		wantExists    bool
		wantInvalid   bool
	}{
		{
			name:         "not found error",
			status:       http.StatusNotFound,
			body:         `{"error":"monitor not found","code":"NOT_FOUND","request_id":"req-1"}`,
			wantCode:     CodeNotFound,
			wantNotFound: true,
			wantExists:   false,
			wantInvalid:  false,
		},
		{
			name:         "already exists conflict",
			status:       http.StatusUnprocessableEntity,
			body:         `{"error":"the request conflicts with an existing record","code":"ALREADY_EXISTS","request_id":"req-2"}`,
			wantCode:     CodeAlreadyExists,
			wantNotFound: false,
			wantExists:   true,
			wantInvalid:  false,
		},
		{
			name:         "duplicate alias code",
			status:       http.StatusConflict,
			body:         `{"error":"duplicate name","code":"DUPLICATE","request_id":"req-3"}`,
			wantCode:     "DUPLICATE",
			wantNotFound: false,
			wantExists:   true,
			wantInvalid:  false,
		},
		{
			name:         "invalid argument validation failure",
			status:       http.StatusBadRequest,
			body:         `{"error":"name is required","code":"INVALID_ARGUMENT","request_id":"req-4","details":[{"field":"name","reason":"name is required"}]}`,
			wantCode:     CodeInvalidArgument,
			wantNotFound: false,
			wantExists:   false,
			wantInvalid:  true,
		},
		{
			name:         "fallback HTML from proxy with 404",
			status:       http.StatusNotFound,
			body:         `<html>404 Not Found</html>`,
			wantCode:     "Not Found",
			wantNotFound: true,
			wantExists:   false,
			wantInvalid:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := decodeAPIError(tt.status, []byte(tt.body))
			if err == nil {
				t.Fatal("expected error")
			}
			apiErr, ok := err.(*APIError)
			if !ok {
				t.Fatalf("expected *APIError, got %T", err)
			}
			if apiErr.Code != tt.wantCode {
				t.Errorf("got code %q, want %q", apiErr.Code, tt.wantCode)
			}
			if got := IsNotFound(err); got != tt.wantNotFound {
				t.Errorf("IsNotFound = %v, want %v", got, tt.wantNotFound)
			}
			if got := IsAlreadyExists(err); got != tt.wantExists {
				t.Errorf("IsAlreadyExists = %v, want %v", got, tt.wantExists)
			}
			if got := IsInvalidArgument(err); got != tt.wantInvalid {
				t.Errorf("IsInvalidArgument = %v, want %v", got, tt.wantInvalid)
			}
		})
	}
}

func TestAPIClientSendsIdempotencyKeyOnWriteRequests(t *testing.T) {
	var seenMethod, seenKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMethod = r.Method
		seenKey = r.Header.Get(IdempotencyKeyHeader)
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	client := &apiClient{
		endpoint:   srv.URL,
		token:      "tok",
		httpClient: srv.Client(),
	}

	var out map[string]any
	if err := client.get(context.Background(), "/monitors/1", &out); err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if seenMethod != http.MethodGet || seenKey != "" {
		t.Fatalf("GET should not send Idempotency-Key, got method=%s key=%q", seenMethod, seenKey)
	}

	if err := client.create(context.Background(), "/monitors", map[string]any{"name": "m1"}, &out); err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if seenMethod != http.MethodPost || seenKey == "" {
		t.Fatalf("POST must send non-empty Idempotency-Key, got method=%s key=%q", seenMethod, seenKey)
	}
	firstCreateKey := seenKey

	if err := client.update(context.Background(), "/monitors/1", map[string]any{"name": "m2"}, &out); err != nil {
		t.Fatalf("update failed: %v", err)
	}
	if seenMethod != http.MethodPatch || seenKey == "" || seenKey == firstCreateKey {
		t.Fatalf("PATCH must send a fresh non-empty Idempotency-Key, got %q", seenKey)
	}

	if err := client.delete(WithIdempotencyKey(context.Background(), "explicit-del-key"), "/monitors/1"); err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	if seenMethod != http.MethodDelete || seenKey != "explicit-del-key" {
		t.Fatalf("DELETE must send explicit Idempotency-Key, got %q", seenKey)
	}
}

func TestAPIClientRetriesWriteWithSameIdempotencyKey(t *testing.T) {
	var calls atomic.Int32
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get(IdempotencyKeyHeader))
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"temporary outage","code":"SERVICE_UNAVAILABLE"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":42}`))
	}))
	defer srv.Close()

	client := &apiClient{
		endpoint:   srv.URL,
		httpClient: srv.Client(),
		maxRetries: 1,
	}

	var out map[string]any
	if err := client.create(context.Background(), "/channels", map[string]any{"name": "ops"}, &out); err != nil {
		t.Fatalf("create retry failed: %v", err)
	}
	if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
		t.Fatalf("expected 2 attempts with identical non-empty Idempotency-Key, got %v", keys)
	}
}
