package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListAlerts_SetsCacheControlNoStore(t *testing.T) {
	srv := httptest.NewServer(newProbeServer(&alertsStore{n: 0}, &fakeRPC{}))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/alerts")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /alerts = %d, want 200", res.StatusCode)
	}
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

func TestWriteErr_SetsCacheControlNoStore(t *testing.T) {
	srv := httptest.NewServer(newProbeServer(&alertsStore{n: 0}, &fakeRPC{}))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/alerts/not-an-id/deliveries")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET /alerts/not-an-id/deliveries = %d, want 400", res.StatusCode)
	}
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}
