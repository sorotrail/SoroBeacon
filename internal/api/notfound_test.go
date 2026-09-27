package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUnknownAPIRouteReturnsJSON404(t *testing.T) {
	srv := httptest.NewServer(newProbeServer(&fakeStore{}, &fakeRPC{}))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/definitely-not-a-route")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	assertJSONErrorEnvelope(t, res, http.StatusNotFound)
}

func TestWrongMethodReturnsJSON405WithAllow(t *testing.T) {
	srv := httptest.NewServer(newProbeServer(&fakeStore{}, &fakeRPC{}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	assertJSONErrorEnvelope(t, res, http.StatusMethodNotAllowed)

	allow := res.Header.Get("Allow")
	if allow == "" {
		t.Fatal("405 missing Allow header")
	}
	if !strings.Contains(allow, http.MethodGet) {
		t.Fatalf("Allow=%q, want GET among permitted methods", allow)
	}
	if strings.Contains(allow, http.MethodPost) {
		t.Fatalf("Allow=%q lists POST, which /health does not accept", allow)
	}
}

func TestKnownRouteStillServes(t *testing.T) {
	srv := httptest.NewServer(newProbeServer(&fakeStore{}, &fakeRPC{}))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /health = %d, want 200", res.StatusCode)
	}
}

func assertJSONErrorEnvelope(t *testing.T, res *http.Response, wantStatus int) {
	t.Helper()
	if res.StatusCode != wantStatus {
		t.Fatalf("status = %d, want %d", res.StatusCode, wantStatus)
	}
	ct := res.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body is not JSON: %s\n%s", err, body)
	}
	if _, ok := env["error"].(string); !ok || env["error"] == "" {
		t.Fatalf("envelope missing error string: %v", env)
	}
	if _, ok := env["code"].(string); !ok || env["code"] == "" {
		t.Fatalf("envelope missing code: %v", env)
	}
	if _, ok := env["request_id"].(string); !ok || env["request_id"] == "" {
		t.Fatalf("envelope missing request_id: %v", env)
	}
}
