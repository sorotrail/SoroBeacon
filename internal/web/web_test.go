package web

import (
	"context"
	"encoding/json"
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

func TestPrettyJSON(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"object", `{"a":1,"b":{"c":2}}`, "{\n  \"a\": 1,\n  \"b\": {\n    \"c\": 2\n  }\n}"},
		{"empty", "", ""},
		{"invalid falls back to raw", "not json", "not json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := prettyJSON(json.RawMessage(tt.in))
			if got != tt.want {
				t.Fatalf("prettyJSON(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// alertWithPayloadStore returns one alert with an object payload, so the
// alerts page has something to indent.
type alertWithPayloadStore struct {
	emptyStore
}

func (alertWithPayloadStore) ListAlerts(context.Context, store.AlertFilter) ([]store.Alert, error) {
	return []store.Alert{{ID: 1, Payload: json.RawMessage(`{"amount":"100"}`)}}, nil
}

func TestAlertsPageRendersIndentedPayload(t *testing.T) {
	s, err := New(alertWithPayloadStore{}, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/alerts")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)

	if !strings.Contains(html, "<pre>{\n  &#34;amount&#34;: &#34;100&#34;\n}</pre>") {
		t.Fatalf("expected an indented, HTML-escaped payload inside <pre>, got:\n%s", html)
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
