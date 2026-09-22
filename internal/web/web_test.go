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
	"time"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/poller"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
)

type stubPosition struct{ pos poller.Position }

func (s stubPosition) Position() poller.Position { return s.pos }

// emptyStore answers every page-rendering call with an empty result, so
// index/monitors/channels/alerts render without a real database.
type emptyStore struct {
	store.Store
}

func (emptyStore) GetStats(context.Context) (store.Stats, error) { return store.Stats{}, nil }
func (emptyStore) ListAlerts(context.Context, store.AlertFilter) ([]store.Alert, error) {
	return nil, nil
}
func (emptyStore) ListMonitors(context.Context, bool) ([]store.Monitor, error) { return nil, nil }
func (emptyStore) ListChannels(context.Context, bool) ([]store.Channel, error) { return nil, nil }
func (emptyStore) ListMonitorsPage(context.Context, store.ListFilter) ([]store.Monitor, error) {
	return nil, nil
}
func (emptyStore) ListChannelsPage(context.Context, store.ListFilter) ([]store.Channel, error) {
	return nil, nil
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(emptyStore{}, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

type pagingStore struct {
	emptyStore
	n int
}

func (p pagingStore) ListMonitorsPage(context.Context, store.ListFilter) ([]store.Monitor, error) {
	out := make([]store.Monitor, p.n)
	for i := range out {
		out[i] = store.Monitor{ID: int64(i + 1), Name: "m"}
	}
	return out, nil
}

func TestMonitorsPageShowsOlderLinkOnFullPage(t *testing.T) {
	s, err := New(pagingStore{n: 50}, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/monitors")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	if !strings.Contains(html, `/monitors?cursor=50`) {
		t.Fatalf("expected Older paging link for a full page, got:\n%s", html)
	}
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

func TestOverviewShowsPollerLagWhenReady(t *testing.T) {
	s := newTestServer(t).WithPoller(stubPosition{pos: poller.Position{
		LastProcessedLedger: 100,
		LatestChainLedger:   125,
		LastSuccessfulPoll:  time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC),
	}})
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{"ledger lag", "125", "100", "Last successful poll"} {
		if !strings.Contains(html, want) {
			t.Fatalf("overview missing %q in %s", want, html)
		}
	}
	if strings.Contains(html, "waiting for the first successful poll") {
		t.Fatal("ready poller should not show the waiting copy")
	}
}

func TestOverviewWaitingCopyBeforeFirstPoll(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t).Routes())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "waiting for the first successful poll") {
		t.Fatalf("overview should wait for first poll, got %s", body)
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

// ruleToggleStore backs one monitor with one rule, and actually flips
// Enabled on UpdateRule (rather than ignoring it), so the test can verify
// the toggle round-trips through Get/Update like the real store would.
type ruleToggleStore struct {
	emptyStore
	rule store.Rule
}

func (s *ruleToggleStore) GetMonitor(context.Context, int64) (*store.Monitor, error) {
	return &store.Monitor{ID: 1, Name: "m"}, nil
}
func (s *ruleToggleStore) ListRules(context.Context, int64, bool) ([]store.Rule, error) {
	return []store.Rule{s.rule}, nil
}
func (s *ruleToggleStore) GetRule(_ context.Context, id int64) (*store.Rule, error) {
	if id != s.rule.ID {
		return nil, store.ErrNotFound
	}
	r := s.rule
	return &r, nil
}
func (s *ruleToggleStore) UpdateRule(_ context.Context, r *store.Rule) error {
	s.rule = *r
	return nil
}

func TestToggleRule(t *testing.T) {
	st := &ruleToggleStore{rule: store.Rule{ID: 5, MonitorID: 1, Type: "transfer", Enabled: true}}
	s, err := New(st, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	res, err := client.Post(srv.URL+"/monitors/1/rules/5/toggle", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("toggle status = %d, want %d", res.StatusCode, http.StatusSeeOther)
	}
	if loc := res.Header.Get("Location"); loc != "/monitors/1" {
		t.Fatalf("redirect Location = %q, want /monitors/1", loc)
	}
	if st.rule.Enabled {
		t.Fatal("rule still enabled after toggle")
	}

	// Toggling again flips it back.
	res2, err := client.Post(srv.URL+"/monitors/1/rules/5/toggle", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatal(err)
	}
	res2.Body.Close()
	if !st.rule.Enabled {
		t.Fatal("rule still disabled after toggling twice")
	}

	// A rule that doesn't belong to the path's monitor must 404, not toggle.
	res3, err := client.Post(srv.URL+"/monitors/999/rules/5/toggle", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatal(err)
	}
	res3.Body.Close()
	if res3.StatusCode != http.StatusNotFound {
		t.Fatalf("toggle for wrong monitor = %d, want 404", res3.StatusCode)
	}
}

// deliveriesStore backs GET /alerts/{id}/deliveries with a fixed set of
// attempts and one named channel.
type deliveriesStore struct {
	emptyStore
	attempts []store.DeliveryAttempt
}

func (s deliveriesStore) ListDeliveryAttempts(context.Context, int64) ([]store.DeliveryAttempt, error) {
	return s.attempts, nil
}
func (deliveriesStore) ListChannels(context.Context, bool) ([]store.Channel, error) {
	return []store.Channel{{ID: 7, Name: "ops-webhook"}}, nil
}

func newDeliveriesServer(t *testing.T, st store.Store) *Server {
	t.Helper()
	s, err := New(st, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestAlertDeliveries_DistinguishesSuccessFromFailed(t *testing.T) {
	st := deliveriesStore{attempts: []store.DeliveryAttempt{
		{ID: 1, ChannelID: 7, Status: "success", ResponseSnippet: "200 OK"},
		{ID: 2, ChannelID: 7, Status: "failed", ResponseSnippet: "connection refused"},
	}}
	srv := httptest.NewServer(newDeliveriesServer(t, st).Routes())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/alerts/1/deliveries")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /alerts/1/deliveries = %d, want 200", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)

	if !strings.Contains(html, `class="pill on"`) {
		t.Errorf("expected a success (on) pill, got:\n%s", html)
	}
	if !strings.Contains(html, `class="pill off"`) {
		t.Errorf("expected a failed (off) pill, got:\n%s", html)
	}
	if !strings.Contains(html, "ops-webhook") {
		t.Errorf("expected the channel name resolved from ListChannels, got:\n%s", html)
	}
	if !strings.Contains(html, "connection refused") {
		t.Errorf("expected the failed attempt's response snippet, got:\n%s", html)
	}
}

func TestAlertDeliveries_EmptyIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(newDeliveriesServer(t, deliveriesStore{}).Routes())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/alerts/1/deliveries")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /alerts/1/deliveries = %d, want 200", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "No delivery attempts yet") {
		t.Fatalf("expected the empty-state message, got: %s", body)
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

func TestEmptyKind(t *testing.T) {
	mon := store.Monitor{ID: 1, Name: "alpha", Enabled: true}
	dis := store.Monitor{ID: 1, Name: "alpha", Enabled: false}
	ch := store.Channel{ID: 2, Name: "ops", Type: "webhook"}
	al := store.Alert{ID: 3}

	tests := []struct {
		name     string
		monitors []store.Monitor
		channels []store.Channel
		alerts   []store.Alert
		want     string
	}{
		{"fresh install", nil, nil, nil, "no_monitors"},
		{"all disabled", []store.Monitor{dis}, []store.Channel{ch}, nil, "monitors_disabled"},
		{"monitors but no channels", []store.Monitor{mon}, nil, nil, "no_channels"},
		{"healthy empty", []store.Monitor{mon}, []store.Channel{ch}, nil, "no_alerts"},
		{"has alerts", []store.Monitor{mon}, []store.Channel{ch}, []store.Alert{al}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := emptyKind(tt.monitors, tt.channels, tt.alerts); got != tt.want {
				t.Fatalf("emptyKind = %q, want %q", got, tt.want)
			}
		})
	}
}

type oneMonitorStore struct {
	emptyStore
	enabled bool
}

func (s oneMonitorStore) ListMonitors(context.Context, bool) ([]store.Monitor, error) {
	return []store.Monitor{{ID: 1, Name: "alpha", Enabled: s.enabled}}, nil
}
func (oneMonitorStore) GetStats(context.Context) (store.Stats, error) {
	return store.Stats{Monitors: 1}, nil
}

type readyNoAlertsStore struct{ emptyStore }

func (readyNoAlertsStore) ListMonitors(context.Context, bool) ([]store.Monitor, error) {
	return []store.Monitor{{ID: 1, Name: "alpha", Enabled: true}}, nil
}
func (readyNoAlertsStore) ListChannels(context.Context, bool) ([]store.Channel, error) {
	return []store.Channel{{ID: 2, Name: "ops", Type: "webhook", Enabled: true}}, nil
}
func (readyNoAlertsStore) GetStats(context.Context) (store.Stats, error) {
	return store.Stats{Monitors: 1, Channels: 1}, nil
}

func getHTML(t *testing.T, st store.Store, path string) string {
	t.Helper()
	s, err := New(st, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	res, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", path, res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestOnboardingEmptyStates(t *testing.T) {
	tests := []struct {
		name    string
		store   store.Store
		path    string
		want    []string
		notWant []string
	}{
		{
			name:  "overview nothing configured",
			store: emptyStore{},
			path:  "/",
			want:  []string{`class="empty"`, "Nothing is being watched yet", `href="/monitors"`},
			notWant: []string{
				"that's expected",
				"alerts have nowhere to go",
				"<td><code>",
			},
		},
		{
			name:    "overview monitors but no channels",
			store:   oneMonitorStore{enabled: true},
			path:    "/",
			want:    []string{`class="empty"`, "alerts have nowhere to go", `href="/channels"`},
			notWant: []string{"Nothing is being watched yet", "that's expected", "<td><code>"},
		},
		{
			name:    "overview all monitors disabled",
			store:   oneMonitorStore{enabled: false},
			path:    "/",
			want:    []string{`class="empty"`, "All monitors are disabled", `href="/monitors"`},
			notWant: []string{"Nothing is being watched yet", "that's expected"},
		},
		{
			name:  "overview healthy no alerts",
			store: readyNoAlertsStore{},
			path:  "/",
			want: []string{
				`class="empty"`,
				"that's expected",
				"after a monitor was created",
			},
			notWant: []string{"Nothing is being watched yet", "alerts have nowhere to go", "<td><code>"},
		},
		{
			name:    "monitors page first-run",
			store:   emptyStore{},
			path:    "/monitors",
			want:    []string{`class="empty"`, "Create your first monitor", "after it is created"},
			notWant: []string{"All monitors are disabled"},
		},
		{
			name:    "channels page with a monitor waiting",
			store:   oneMonitorStore{enabled: true},
			path:    "/channels",
			want:    []string{`class="empty"`, "No notification channels yet", "A monitor is already in place", `href="/monitors"`},
			notWant: []string{"that's expected"},
		},
		{
			name:    "alerts page healthy empty",
			store:   readyNoAlertsStore{},
			path:    "/alerts",
			want:    []string{`class="empty"`, "that's expected", "after a monitor was created"},
			notWant: []string{"Nothing is being watched yet", "<td><code>"},
		},
		{
			name:    "alerts page nothing configured",
			store:   emptyStore{},
			path:    "/alerts",
			want:    []string{`class="empty"`, "Nothing is being watched yet", "after a monitor was created", `href="/monitors"`},
			notWant: []string{"that's expected"},
		},
		{
			name:    "overview with real alerts is not an empty state",
			store:   alertWithPayloadStore{},
			path:    "/",
			want:    []string{"Recent alerts"},
			notWant: []string{`class="empty"`, "Nothing is being watched yet", "that's expected"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			html := getHTML(t, tt.store, tt.path)
			for _, s := range tt.want {
				if !strings.Contains(html, s) {
					t.Errorf("missing %q in %s:\n%s", s, tt.path, html)
				}
			}
			for _, s := range tt.notWant {
				if strings.Contains(html, s) {
					t.Errorf("unexpected %q in %s:\n%s", s, tt.path, html)
				}
			}
		})
	}
}
