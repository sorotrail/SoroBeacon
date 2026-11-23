package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// maintenanceStore is an in-memory store.Store with just the maintenance
// methods under test; the embedded interface covers the rest.
type maintenanceStore struct {
	store.Store
	windows []store.MaintenanceWindow
	nextID  int64
}

func (m *maintenanceStore) CreateMaintenanceWindow(_ context.Context, w *store.MaintenanceWindow) error {
	m.nextID++
	w.ID = m.nextID
	m.windows = append(m.windows, *w)
	return nil
}

func (m *maintenanceStore) ListMaintenanceWindows(_ context.Context, _ store.MaintenanceWindowFilter) ([]store.MaintenanceWindow, error) {
	return m.windows, nil
}

func (m *maintenanceStore) GetMaintenanceWindow(_ context.Context, id int64) (*store.MaintenanceWindow, error) {
	for i := range m.windows {
		if m.windows[i].ID == id {
			w := m.windows[i]
			return &w, nil
		}
	}
	return nil, store.ErrNotFound
}

func (m *maintenanceStore) UpdateMaintenanceWindow(_ context.Context, w *store.MaintenanceWindow) error {
	for i := range m.windows {
		if m.windows[i].ID == w.ID {
			m.windows[i] = *w
			return nil
		}
	}
	return store.ErrNotFound
}

func (m *maintenanceStore) DeleteMaintenanceWindow(_ context.Context, id int64) error {
	for i := range m.windows {
		if m.windows[i].ID == id {
			m.windows = append(m.windows[:i], m.windows[i+1:]...)
			return nil
		}
	}
	return store.ErrNotFound
}

func maintenanceServer(t *testing.T) (*httptest.Server, *maintenanceStore) {
	t.Helper()
	st := &maintenanceStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	t.Cleanup(srv.Close)
	return srv, st
}

func doJSON(t *testing.T, method, url string, body any) (*http.Response, errorEnvelope) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	var env errorEnvelope
	_ = json.Unmarshal(raw, &env)
	return res, env
}

func TestCreateMaintenanceWindow_Valid(t *testing.T) {
	srv, st := maintenanceServer(t)
	start := time.Date(2026, 9, 23, 22, 0, 0, 0, time.UTC)

	res, env := doJSON(t, http.MethodPost, srv.URL+"/maintenance-windows", map[string]any{
		"reason":   "planned upgrade",
		"scope":    "global",
		"start_at": start,
		"end_at":   start.Add(2 * time.Hour),
	})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %+v", res.StatusCode, env)
	}
	if len(st.windows) != 1 || st.windows[0].Reason != "planned upgrade" {
		t.Fatalf("windows = %+v", st.windows)
	}
}

func TestCreateMaintenanceWindow_ValidationDetails(t *testing.T) {
	srv, _ := maintenanceServer(t)
	start := time.Date(2026, 9, 23, 22, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		body  map[string]any
		field string
	}{
		{
			name:  "monitor scope needs monitor_id",
			body:  map[string]any{"reason": "r", "scope": "monitor", "start_at": start, "end_at": start.Add(time.Hour)},
			field: "monitor_id",
		},
		{
			name:  "contract scope needs contract_id",
			body:  map[string]any{"reason": "r", "scope": "contract", "start_at": start, "end_at": start.Add(time.Hour)},
			field: "contract_id",
		},
		{
			name:  "global scope rejects identifiers",
			body:  map[string]any{"reason": "r", "scope": "global", "contract_id": "CAAA", "start_at": start, "end_at": start.Add(time.Hour)},
			field: "scope",
		},
		{
			name:  "end must follow start",
			body:  map[string]any{"reason": "r", "scope": "global", "start_at": start, "end_at": start},
			field: "end_at",
		},
		{
			name:  "open-ended rejected",
			body:  map[string]any{"reason": "r", "scope": "global", "start_at": start},
			field: "end_at",
		},
		{
			name:  "reason required",
			body:  map[string]any{"scope": "global", "start_at": start, "end_at": start.Add(time.Hour)},
			field: "reason",
		},
		{
			name:  "unknown scope",
			body:  map[string]any{"reason": "r", "scope": "everything", "start_at": start, "end_at": start.Add(time.Hour)},
			field: "scope",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, env := doJSON(t, http.MethodPost, srv.URL+"/maintenance-windows", tt.body)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", res.StatusCode)
			}
			found := false
			for _, d := range env.Details {
				if d.Field == tt.field {
					found = true
				}
			}
			if !found {
				t.Fatalf("details = %+v, want field %q", env.Details, tt.field)
			}
		})
	}
}

func TestMaintenanceWindow_GetPatchDelete(t *testing.T) {
	srv, st := maintenanceServer(t)
	start := time.Date(2026, 9, 23, 22, 0, 0, 0, time.UTC)
	st.windows = []store.MaintenanceWindow{{
		ID: 1, Reason: "old", Scope: store.MaintenanceScopeGlobal,
		StartAt: start, EndAt: start.Add(time.Hour),
	}}

	res, _ := doJSON(t, http.MethodGet, srv.URL+"/maintenance-windows/1", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", res.StatusCode)
	}

	res, env := doJSON(t, http.MethodPatch, srv.URL+"/maintenance-windows/1", map[string]any{"reason": "new"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d, want 200; %+v", res.StatusCode, env)
	}
	if st.windows[0].Reason != "new" {
		t.Fatalf("reason = %q, want new", st.windows[0].Reason)
	}

	res, _ = doJSON(t, http.MethodDelete, srv.URL+"/maintenance-windows/1", nil)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", res.StatusCode)
	}
	res, _ = doJSON(t, http.MethodDelete, srv.URL+"/maintenance-windows/1", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want 404", res.StatusCode)
	}
}

func TestListMaintenanceWindows_EmptyArray(t *testing.T) {
	srv, _ := maintenanceServer(t)
	res, err := http.Get(srv.URL + "/maintenance-windows")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var body struct {
		Windows []store.MaintenanceWindow `json:"maintenance_windows"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v; body %s", err, raw)
	}
	if body.Windows == nil {
		t.Fatalf("maintenance_windows must be an empty array, not null: %s", raw)
	}
}
