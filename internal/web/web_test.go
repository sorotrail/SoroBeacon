package web

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestTestChannelHTML(t *testing.T) {
	t.Parallel()

	ok := testChannelHTML(nil)
	if !strings.Contains(ok, `class="pill on"`) {
		t.Fatalf("success markup missing on-pill: %s", ok)
	}
	if strings.Contains(ok, `class="pill off"`) {
		t.Fatalf("success markup must not use the off-pill: %s", ok)
	}

	plain := testChannelHTML(fmt.Errorf("connection refused"))
	if !strings.Contains(plain, `class="pill off"`) {
		t.Fatalf("failure markup missing off-pill: %s", plain)
	}
	if strings.Contains(plain, `class="pill on"`) {
		t.Fatalf("failure markup must not use the on-pill: %s", plain)
	}
	if !strings.Contains(plain, "connection refused") {
		t.Fatalf("failure markup missing error text: %s", plain)
	}

	httpErr := testChannelHTML(fmt.Errorf("status 403: provider said no"))
	if !strings.Contains(httpErr, `class="pill off"`) {
		t.Fatalf("HTTP failure markup missing off-pill: %s", httpErr)
	}
	if !strings.Contains(httpErr, "HTTP 403") {
		t.Fatalf("HTTP failure must state the status explicitly: %s", httpErr)
	}
	if !strings.Contains(httpErr, "provider said no") {
		t.Fatalf("HTTP failure missing provider body: %s", httpErr)
	}

	escaped := testChannelHTML(fmt.Errorf(`<script>alert("x")</script>`))
	if strings.Contains(escaped, `<script>`) {
		t.Fatalf("error text must be HTML-escaped, got: %s", escaped)
	}
	if !strings.Contains(escaped, "&lt;script&gt;") {
		t.Fatalf("expected escaped script tag, got: %s", escaped)
	}
}

type channelGetStore struct {
	emptyStore
	ch store.Channel
}

func (s channelGetStore) GetChannel(_ context.Context, id int64) (*store.Channel, error) {
	if s.ch.ID != id {
		return nil, store.ErrNotFound
	}
	c := s.ch
	return &c, nil
}

func (s channelGetStore) ListChannels(context.Context, bool) ([]store.Channel, error) {
	return []store.Channel{s.ch}, nil
}

type stubNotifier struct{ err error }

func (s stubNotifier) Send(context.Context, notify.Alert) error { return s.err }

func newChannelTestServer(t *testing.T, sendErr error) *httptest.Server {
	t.Helper()
	f := notify.DefaultFactory()
	f.Register("stub", func(json.RawMessage) (notify.Notifier, error) {
		return stubNotifier{err: sendErr}, nil
	})
	st := channelGetStore{ch: store.Channel{
		ID: 1, Name: "ops", Type: "stub", Config: json.RawMessage(`{"webhook_url":"https://secret.example"}`), Enabled: true,
	}}
	s, err := New(st, rules.NewRegistry(), f, slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return httptest.NewServer(s.Routes())
}

func TestTestChannelHandler_SuccessAndFailureMarkup(t *testing.T) {
	t.Parallel()

	okSrv := newChannelTestServer(t, nil)
	defer okSrv.Close()
	okBody := postTestChannel(t, okSrv, 1)
	if !strings.Contains(okBody, `class="pill on"`) || !strings.Contains(okBody, "sent") {
		t.Fatalf("success fragment: %s", okBody)
	}
	if strings.Contains(okBody, "secret.example") {
		t.Fatalf("must not echo channel config: %s", okBody)
	}

	failSrv := newChannelTestServer(t, fmt.Errorf("status 502: <upstream>"))
	defer failSrv.Close()
	failBody := postTestChannel(t, failSrv, 1)
	if !strings.Contains(failBody, `class="pill off"`) {
		t.Fatalf("failure fragment missing off-pill: %s", failBody)
	}
	if !strings.Contains(failBody, "HTTP 502") {
		t.Fatalf("failure fragment missing HTTP status: %s", failBody)
	}
	if strings.Contains(failBody, "<upstream>") {
		t.Fatalf("provider text must be escaped: %s", failBody)
	}
	if !strings.Contains(failBody, "&lt;upstream&gt;") {
		t.Fatalf("expected escaped provider text: %s", failBody)
	}
	if strings.Contains(failBody, "secret.example") {
		t.Fatalf("must not echo channel config: %s", failBody)
	}
}

func TestChannelsPage_TestResultIsLiveRegion(t *testing.T) {
	t.Parallel()

	srv := newChannelTestServer(t, nil)
	defer srv.Close()
	res, err := http.Get(srv.URL + "/channels")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	if !strings.Contains(html, `id="test-result-1"`) {
		t.Fatalf("expected a test-result slot, got:\n%s", html)
	}
	idx := strings.Index(html, `id="test-result-1"`)
	tagStart := strings.LastIndex(html[:idx], "<span")
	tagEnd := strings.Index(html[tagStart:], ">")
	tag := html[tagStart : tagStart+tagEnd+1]
	if !strings.Contains(tag, `aria-live="polite"`) {
		t.Fatalf("test-result slot must be a live region, tag=%s", tag)
	}
}

func postTestChannel(t *testing.T, srv *httptest.Server, id int64) string {
	t.Helper()
	res, err := http.Post(fmt.Sprintf("%s/channels/%d/test", srv.URL, id), "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST /channels/%d/test = %d, want 200", id, res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
