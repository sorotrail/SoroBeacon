package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/store"
)

type duplicateStore struct {
	store.Store
	gotID int64
	err   error
}

func (d *duplicateStore) DuplicateMonitor(_ context.Context, id int64) (*store.Monitor, error) {
	d.gotID = id
	if d.err != nil {
		return nil, d.err
	}
	return &store.Monitor{
		ID:          99,
		Name:        "alpha (copy)",
		ContractIDs: []string{validContract},
		Enabled:     false,
		ChannelIDs:  []int64{1, 2},
	}, nil
}

func TestDuplicateMonitor_CreatedDisabled(t *testing.T) {
	st := &duplicateStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, err := http.Post(srv.URL+"/monitors/7/duplicate", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("POST /monitors/7/duplicate = %d, want 201 body=%s", res.StatusCode, body)
	}
	if st.gotID != 7 {
		t.Fatalf("DuplicateMonitor id = %d, want 7", st.gotID)
	}
	var m store.Monitor
	if err := json.NewDecoder(res.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m.ID != 99 || m.Name != "alpha (copy)" || m.Enabled {
		t.Fatalf("copy = %+v, want id=99 name=alpha (copy) enabled=false", m)
	}
}

func TestDuplicateMonitor_UnknownID(t *testing.T) {
	st := &duplicateStore{err: store.ErrNotFound}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, err := http.Post(srv.URL+"/monitors/7/duplicate", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
}

func TestDuplicateMonitor_InvalidID(t *testing.T) {
	st := &duplicateStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, err := http.Post(srv.URL+"/monitors/nope/duplicate", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if st.gotID != 0 {
		t.Fatalf("store should not be called for invalid id, got %d", st.gotID)
	}
}
