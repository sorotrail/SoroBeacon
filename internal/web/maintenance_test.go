package web

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
)

type maintenanceWebStore struct {
	emptyStore
	windows []store.MaintenanceWindow
	created *store.MaintenanceWindow
}

func (s *maintenanceWebStore) ListMonitors(context.Context, bool) ([]store.Monitor, error) {
	return []store.Monitor{{ID: 1, Name: "alpha", Enabled: true}}, nil
}

func (s *maintenanceWebStore) ListMaintenanceWindows(_ context.Context, f store.MaintenanceWindowFilter) ([]store.MaintenanceWindow, error) {
	if !f.Active && !f.Upcoming {
		return s.windows, nil
	}
	var out []store.MaintenanceWindow
	for _, w := range s.windows {
		if f.Active && !w.StartAt.After(f.At) && w.EndAt.After(f.At) {
			out = append(out, w)
		}
		if f.Upcoming && w.StartAt.After(f.At) {
			out = append(out, w)
		}
	}
	return out, nil
}

func (s *maintenanceWebStore) CreateMaintenanceWindow(_ context.Context, w *store.MaintenanceWindow) error {
	s.created = w
	return nil
}

func newMaintenanceServer(t *testing.T, st store.Store) *httptest.Server {
	t.Helper()
	s, err := New(st, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	return srv
}

func TestMaintenancePageListsActiveAndUpcoming(t *testing.T) {
	now := time.Now()
	st := &maintenanceWebStore{windows: []store.MaintenanceWindow{
		{ID: 1, Reason: "active-one", Scope: store.MaintenanceScopeGlobal, StartAt: now.Add(-time.Hour), EndAt: now.Add(time.Hour)},
		{ID: 2, Reason: "upcoming-one", Scope: store.MaintenanceScopeGlobal, StartAt: now.Add(time.Hour), EndAt: now.Add(2 * time.Hour)},
	}}
	html := getHTML(t, st, "/maintenance")
	for _, want := range []string{"active-one", "upcoming-one", "New window"} {
		if !strings.Contains(html, want) {
			t.Fatalf("maintenance page missing %q", want)
		}
	}
}

func TestCreateMaintenanceForm(t *testing.T) {
	st := &maintenanceWebStore{}
	srv := newMaintenanceServer(t, st)

	res, err := http.PostForm(srv.URL+"/maintenance", url.Values{
		"reason":   {"planned upgrade"},
		"scope":    {"global"},
		"start_at": {"2026-09-23T22:00"},
		"end_at":   {"2026-09-24T02:00"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after redirect", res.StatusCode)
	}
	if st.created == nil {
		t.Fatal("window was not created")
	}
	if st.created.Reason != "planned upgrade" || st.created.Scope != store.MaintenanceScopeGlobal {
		t.Fatalf("created = %+v", st.created)
	}
	if !st.created.EndAt.After(st.created.StartAt) {
		t.Fatalf("end_at must be after start_at: %+v", st.created)
	}
}

func TestCreateMaintenanceFormRejectsOpenEnded(t *testing.T) {
	st := &maintenanceWebStore{}
	srv := newMaintenanceServer(t, st)

	res, err := http.PostForm(srv.URL+"/maintenance", url.Values{
		"reason":   {"forever"},
		"scope":    {"global"},
		"start_at": {"2026-09-23T22:00"},
		"end_at":   {"2026-09-23T22:00"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if st.created != nil {
		t.Fatal("open-ended window must not be created")
	}
}
