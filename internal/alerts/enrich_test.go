package alerts

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEnricherCachesValidatedObject(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("contract_id") != "C123" || r.URL.Query().Get("event_name") != "Transfer" {
			t.Fatalf("query = %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"owner":"Treasury","runbook":"payments"}`))
	}))
	defer srv.Close()
	e, err := NewEnricher(srv.URL, time.Second, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	first, err := e.Fetch(context.Background(), "C123", "Transfer")
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.Fetch(context.Background(), "C123", "Transfer")
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) || calls != 1 {
		t.Fatalf("value=%s/%s calls=%d", first, second, calls)
	}
}

func TestEnricherRejectsUntrustedNonObjectResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`[]`)) }))
	defer srv.Close()
	e, err := NewEnricher(srv.URL, time.Second, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Fetch(context.Background(), "C123", "Transfer"); err == nil {
		t.Fatal("expected object validation error")
	}
}
