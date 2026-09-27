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

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
)

type searchStore struct {
	emptyStore
	searches []store.SavedSearch
	nextID   int64
}

func (s *searchStore) ListSavedSearches(context.Context) ([]store.SavedSearch, error) {
	return s.searches, nil
}

func (s *searchStore) CreateSavedSearch(_ context.Context, ss *store.SavedSearch) error {
	s.nextID++
	ss.ID = s.nextID
	if ss.IsDefault {
		for i := range s.searches {
			s.searches[i].IsDefault = false
		}
	}
	s.searches = append(s.searches, *ss)
	return nil
}

func (s *searchStore) GetSavedSearch(_ context.Context, id int64) (*store.SavedSearch, error) {
	for _, ss := range s.searches {
		if ss.ID == id {
			return &ss, nil
		}
	}
	return nil, store.ErrNotFound
}

func (s *searchStore) DeleteSavedSearch(_ context.Context, id int64) error {
	for i, ss := range s.searches {
		if ss.ID == id {
			s.searches = append(s.searches[:i], s.searches[i+1:]...)
			return nil
		}
	}
	return store.ErrNotFound
}

func (s *searchStore) SetDefaultSearch(_ context.Context, id int64) error {
	found := false
	for i := range s.searches {
		s.searches[i].IsDefault = s.searches[i].ID == id
		if s.searches[i].ID == id {
			found = true
		}
	}
	if !found {
		return store.ErrNotFound
	}
	return nil
}

func (s *searchStore) ClearDefaultSearch(_ context.Context, id int64) error {
	for i := range s.searches {
		if s.searches[i].ID == id {
			s.searches[i].IsDefault = false
			return nil
		}
	}
	return store.ErrNotFound
}

func TestSearchesPage(t *testing.T) {
	st := &searchStore{}
	srv, err := New(st, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	res, err := http.Get(ts.URL + "/searches")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
}

func TestCreateAndDeleteSearch(t *testing.T) {
	st := &searchStore{}
	srv, err := New(st, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	form := url.Values{"name": {"morning"}, "monitor_id": {"5"}, "sort": {"created_at_desc"}}
	res, err := client.PostForm(ts.URL+"/searches", form)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("create status = %d, want 303", res.StatusCode)
	}
	if len(st.searches) != 1 {
		t.Fatalf("expected 1 search, got %d", len(st.searches))
	}
	if st.searches[0].Name != "morning" {
		t.Errorf("name = %q, want morning", st.searches[0].Name)
	}
	if st.searches[0].Filter.MonitorID != 5 {
		t.Errorf("monitor_id = %d, want 5", st.searches[0].Filter.MonitorID)
	}

	res, err = client.Post(ts.URL+"/searches/1/delete", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete status = %d, want 303", res.StatusCode)
	}
	if len(st.searches) != 0 {
		t.Fatalf("expected 0 searches after delete, got %d", len(st.searches))
	}
}

func TestDefaultSearchRedirect(t *testing.T) {
	st := &searchStore{
		searches: []store.SavedSearch{
			{ID: 1, Name: "default", Filter: store.SavedSearchFilter{MonitorID: 3, Sort: "created_at_asc"}, IsDefault: true},
		},
		nextID: 1,
	}
	srv, err := New(st, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/alerts", nil)
	redir := srv.defaultSearchRedirect(req)
	if !strings.Contains(redir, "monitor_id=3") {
		t.Errorf("expected monitor_id=3 in redirect, got: %s", redir)
	}
	if !strings.Contains(redir, "sort=created_at_asc") {
		t.Errorf("expected sort=created_at_asc in redirect, got: %s", redir)
	}
}

func TestStaleMonitorDegrades(t *testing.T) {
	st := &searchStore{
		searches: []store.SavedSearch{
			{ID: 1, Name: "stale", Filter: store.SavedSearchFilter{MonitorID: 999}},
		},
		nextID: 1,
	}
	srv, err := New(st, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	res, err := http.Get(ts.URL + "/searches")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
}
