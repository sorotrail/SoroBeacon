package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/store"
)

const testContractID = "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"

// createMonitorStore records CreateMonitor so body-limit tests can tell a
// 413 from a 201 without a database.
type createMonitorStore struct {
	store.Store
	created int
}

func (c *createMonitorStore) CreateMonitor(_ context.Context, m *store.Monitor) error {
	c.created++
	m.ID = int64(c.created)
	return nil
}

func monitorJSONWithPad(t *testing.T, n int) []byte {
	t.Helper()
	prefix := `{"name":"n","contract_ids":["` + testContractID + `"],"pad":"`
	suffix := `"}`
	need := n - len(prefix) - len(suffix)
	if need < 0 {
		t.Fatalf("target size %d is smaller than the minimum monitor JSON (%d)", n, len(prefix)+len(suffix))
	}
	return []byte(prefix + strings.Repeat("a", need) + suffix)
}

func decodeEnvelope(t *testing.T, body io.Reader) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.NewDecoder(body).Decode(&env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	return env
}

func TestMaxBodyBytes_AcceptedUnderLimit(t *testing.T) {
	const limit = 256
	st := &createMonitorStore{}
	s := New(st, nil, nil, &fakeRPC{}, discardLogger()).WithMaxBodyBytes(limit)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	body := monitorJSONWithPad(t, limit-1)
	res, err := http.Post(srv.URL+"/monitors", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("POST under limit = %d, want 201; body %s", res.StatusCode, slurp(t, res.Body))
	}
	if st.created != 1 {
		t.Fatalf("CreateMonitor called %d times, want 1", st.created)
	}
}

func TestMaxBodyBytes_ExactlyAtLimit(t *testing.T) {
	const limit = 256
	st := &createMonitorStore{}
	s := New(st, nil, nil, &fakeRPC{}, discardLogger()).WithMaxBodyBytes(limit)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	body := monitorJSONWithPad(t, limit)
	res, err := http.Post(srv.URL+"/monitors", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("POST at limit = %d, want 201; body %s", res.StatusCode, slurp(t, res.Body))
	}
	if st.created != 1 {
		t.Fatalf("CreateMonitor called %d times, want 1", st.created)
	}
}

func TestMaxBodyBytes_OversizedReturns413Envelope(t *testing.T) {
	const limit = 256
	st := &createMonitorStore{}
	s := New(st, nil, nil, &fakeRPC{}, discardLogger()).WithMaxBodyBytes(limit)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	body := monitorJSONWithPad(t, limit+1)
	res, err := http.Post(srv.URL+"/monitors", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("POST over limit = %d, want 413", res.StatusCode)
	}
	env := decodeEnvelope(t, res.Body)
	if env["error"] != "request body too large" {
		t.Fatalf("error = %v, want request body too large", env["error"])
	}
	if env["code"] != http.StatusText(http.StatusRequestEntityTooLarge) {
		t.Fatalf("code = %v, want %s", env["code"], http.StatusText(http.StatusRequestEntityTooLarge))
	}
	if _, ok := env["request_id"]; !ok {
		t.Fatalf("envelope missing request_id: %v", env)
	}
	if st.created != 0 {
		t.Fatalf("CreateMonitor called %d times, want 0 on 413", st.created)
	}
}

func TestMaxBodyBytes_GETUnaffected(t *testing.T) {
	s := New(&fakeStore{}, nil, nil, &fakeRPC{}, discardLogger()).WithMaxBodyBytes(8)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/livez")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /livez with tiny body limit = %d, want 200", res.StatusCode)
	}
}

func slurp(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
