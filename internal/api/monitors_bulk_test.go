package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/store"
)

type bulkStore struct {
	store.Store
	known       map[int64]bool
	lastIDs     []int64
	lastEnabled bool
	calls       int
}

func (b *bulkStore) SetMonitorsEnabled(_ context.Context, ids []int64, enabled bool) (int, []int64, error) {
	b.calls++
	b.lastIDs = append([]int64(nil), ids...)
	b.lastEnabled = enabled
	updated := 0
	unknown := make([]int64, 0)
	seen := map[int64]struct{}{}
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if _, ok := b.known[id]; ok {
			b.known[id] = enabled
			updated++
		} else {
			unknown = append(unknown, id)
		}
	}
	return updated, unknown, nil
}

func postBulk(t *testing.T, st store.Store, body any) (*http.Response, []byte) {
	t.Helper()
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	t.Cleanup(srv.Close)
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Post(srv.URL+"/monitors/bulk", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res, raw
}

func TestBulkMonitors_SuccessReportsUnknown(t *testing.T) {
	st := &bulkStore{known: map[int64]bool{1: true, 2: true}}
	res, raw := postBulk(t, st, map[string]any{"ids": []int64{1, 2, 99}, "enabled": false})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", res.StatusCode, raw)
	}
	if st.calls != 1 {
		t.Fatalf("calls = %d, want 1", st.calls)
	}
	if st.lastEnabled {
		t.Fatal("expected enabled=false")
	}
	var got struct {
		Updated    int     `json:"updated"`
		UnknownIDs []int64 `json:"unknown_ids"`
		Enabled    bool    `json:"enabled"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Updated != 2 {
		t.Fatalf("updated = %d, want 2", got.Updated)
	}
	if len(got.UnknownIDs) != 1 || got.UnknownIDs[0] != 99 {
		t.Fatalf("unknown_ids = %v, want [99]", got.UnknownIDs)
	}
	if got.Enabled {
		t.Fatal("enabled = true, want false")
	}
	if st.known[1] || st.known[2] {
		t.Fatalf("known still enabled: %+v", st.known)
	}
}

func TestBulkMonitors_EmptyIDs(t *testing.T) {
	st := &bulkStore{known: map[int64]bool{1: true}}
	res, raw := postBulk(t, st, map[string]any{"ids": []int64{}, "enabled": false})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", res.StatusCode, raw)
	}
	if st.calls != 0 {
		t.Fatalf("store called %d times after empty ids, want 0", st.calls)
	}
	var env errorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Details) == 0 || env.Details[0].Field != "ids" {
		t.Fatalf("details = %+v, want ids", env.Details)
	}
}

func TestBulkMonitors_MissingEnabled(t *testing.T) {
	st := &bulkStore{known: map[int64]bool{1: true}}
	res, raw := postBulk(t, st, map[string]any{"ids": []int64{1}})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", res.StatusCode, raw)
	}
	if st.calls != 0 {
		t.Fatalf("store called %d times, want 0", st.calls)
	}
	var env errorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, d := range env.Details {
		got[d.Field] = d.Reason
	}
	if got["enabled"] != "enabled is required" {
		t.Fatalf("enabled detail = %q", got["enabled"])
	}
}

func TestBulkMonitors_Oversize(t *testing.T) {
	st := &bulkStore{known: map[int64]bool{}}
	ids := make([]int64, 101)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	res, raw := postBulk(t, st, map[string]any{"ids": ids, "enabled": true})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", res.StatusCode, raw)
	}
	if st.calls != 0 {
		t.Fatalf("store called %d times, want 0", st.calls)
	}
}

func TestBulkMonitors_MaxSizeAccepted(t *testing.T) {
	known := map[int64]bool{}
	ids := make([]int64, 100)
	for i := range ids {
		ids[i] = int64(i + 1)
		known[ids[i]] = true
	}
	st := &bulkStore{known: known}
	res, raw := postBulk(t, st, map[string]any{"ids": ids, "enabled": false})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", res.StatusCode, raw)
	}
	if st.calls != 1 {
		t.Fatalf("calls = %d, want 1", st.calls)
	}
}
