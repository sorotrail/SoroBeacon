package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// emptyStore answers every page-rendering call with an empty result, so
// index/monitors/channels/alerts render without a real database.
type emptyStore struct {
	store.Store
}

func (emptyStore) GetStats(context.Context) (store.Stats, error)                     { return store.Stats{}, nil }
func (emptyStore) ListAlerts(context.Context, store.AlertFilter) ([]store.Alert, error) {
	return nil, nil
}
func (emptyStore) ListMonitors(context.Context, bool) ([]store.Monitor, error) { return nil, nil }
func (emptyStore) ListChannels(context.Context, bool) ([]store.Channel, error) { return nil, nil }

func newTestServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(emptyStore{}, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestNavHighlightsActivePage(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t).Routes())
	defer srv.Close()

	tests := []struct {
		path   string
		active string // link text expected to carry class="active"
	}{
		{"/", "Overview"},
		{"/monitors", "Monitors"},
		{"/channels", "Channels"},
		{"/alerts", "Alerts"},
	}
	linkNames := []string{"Overview", "Monitors", "Channels", "Alerts"}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			res, err := http.Get(srv.URL + tt.path)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200", tt.path, res.StatusCode)
			}
			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatal(err)
			}
			html := string(body)

			for _, name := range linkNames {
				// The active link's <a ...> tag must carry class="active";
				// every other nav link must not.
				idx := strings.Index(html, ">"+name+"</a>")
				if idx < 0 {
					t.Fatalf("nav link %q not found in %s response", name, tt.path)
				}
				tagStart := strings.LastIndex(html[:idx], "<a ")
				tag := html[tagStart:idx]
				hasActive := strings.Contains(tag, `class="active"`)
				wantActive := name == tt.active
				if hasActive != wantActive {
					t.Fatalf("%s: nav link %q active=%v, want %v (tag: %s)", tt.path, name, hasActive, wantActive, tag)
				}
			}
		})
	}
}

func TestFavicon(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t).Routes())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/favicon.ico")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /favicon.ico = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "image/svg+xml" {
		t.Fatalf("Content-Type = %q, want image/svg+xml", ct)
	}
}
