package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// pageStore returns a fixed number of monitors and channels from the
// paginated listing methods so tests can drive the page-size heuristic
// without a real database.
type pageStore struct {
	store.Store
	n              int
	gotEnabledOnly *bool
	gotFilter      store.ListFilter
}

func (p *pageStore) ListMonitorsPage(ctx context.Context, f store.ListFilter) ([]store.Monitor, error) {
	p.gotEnabledOnly = &f.EnabledOnly
	p.gotFilter = f
	out := make([]store.Monitor, p.n)
	for i := range out {
		out[i] = store.Monitor{ID: int64(i + 1), Name: "m"}
	}
	return out, nil
}

func (p *pageStore) ListChannelsPage(ctx context.Context, f store.ListFilter) ([]store.Channel, error) {
	p.gotEnabledOnly = &f.EnabledOnly
	p.gotFilter = f
	out := make([]store.Channel, p.n)
	for i := range out {
		out[i] = store.Channel{ID: int64(i + 1), Name: "c", Type: "webhook"}
	}
	return out, nil
}

func getJSON(t *testing.T, st store.Store, path string) (int, map[string]any) {
	t.Helper()
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()
	res, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, body
}

func TestListMonitors_NextCursorOmittedOnShortPage(t *testing.T) {
	_, body := getJSON(t, &pageStore{n: 3}, "/monitors")
	if got := body["next_cursor"]; got != "" {
		t.Fatalf("next_cursor on a short page = %q, want empty", got)
	}
}

func TestListMonitors_NextCursorPresentOnFullPage(t *testing.T) {
	_, body := getJSON(t, &pageStore{n: 5}, "/monitors?limit=5")
	if got, ok := body["next_cursor"].(string); !ok || got == "" {
		t.Fatalf("next_cursor on a full page = %v, want the last monitor's id", body["next_cursor"])
	}
}

func TestListMonitors_NextCursorPresentOnFullDefaultPage(t *testing.T) {
	_, body := getJSON(t, &pageStore{n: 50}, "/monitors")
	if got, ok := body["next_cursor"].(string); !ok || got == "" {
		t.Fatalf("next_cursor on a full default-limit page = %v, want the last id", body["next_cursor"])
	}
}

func TestListMonitors_NextCursorOmittedOnEmptyPage(t *testing.T) {
	_, body := getJSON(t, &pageStore{n: 0}, "/monitors")
	if got := body["next_cursor"]; got != "" {
		t.Fatalf("next_cursor on an empty page = %q, want empty", got)
	}
	list, _ := body["monitors"].([]any)
	if len(list) != 0 {
		t.Fatalf("monitors = %v, want []", body["monitors"])
	}
}

func TestListMonitors_EnabledParamWiring(t *testing.T) {
	tests := []struct {
		query string
		want  bool
	}{
		{"", false},
		{"?enabled=true", true},
		{"?enabled=false", false},
		{"?enabled=garbage", false},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			ps := &pageStore{n: 1}
			code, _ := getJSON(t, ps, "/monitors"+tt.query)
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200", code)
			}
			if ps.gotEnabledOnly == nil {
				t.Fatal("ListMonitorsPage was not called")
			}
			if *ps.gotEnabledOnly != tt.want {
				t.Fatalf("enabledOnly = %v, want %v", *ps.gotEnabledOnly, tt.want)
			}
		})
	}
}

func TestListMonitors_InvalidLimitAndCursor(t *testing.T) {
	tests := []string{"?limit=0", "?limit=-1", "?limit=nope", "?cursor=abc"}
	for _, q := range tests {
		t.Run(q, func(t *testing.T) {
			code, body := getJSON(t, &pageStore{n: 1}, "/monitors"+q)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%v, want 400", code, body)
			}
		})
	}
}

func TestListMonitors_CursorAndLimitPassedThrough(t *testing.T) {
	ps := &pageStore{n: 2}
	code, _ := getJSON(t, ps, "/monitors?limit=10&cursor=42&enabled=true")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if ps.gotFilter.Limit != 10 || ps.gotFilter.AfterID != 42 || !ps.gotFilter.EnabledOnly {
		t.Fatalf("filter = %+v, want limit=10 afterID=42 enabled", ps.gotFilter)
	}
}

func TestListChannels_NextCursorOmittedOnShortPage(t *testing.T) {
	_, body := getJSON(t, &pageStore{n: 3}, "/channels")
	if got := body["next_cursor"]; got != "" {
		t.Fatalf("next_cursor on a short page = %q, want empty", got)
	}
}

func TestListChannels_NextCursorPresentOnFullPage(t *testing.T) {
	_, body := getJSON(t, &pageStore{n: 5}, "/channels?limit=5")
	if got, ok := body["next_cursor"].(string); !ok || got == "" {
		t.Fatalf("next_cursor on a full page = %v, want the last channel's id", body["next_cursor"])
	}
}
