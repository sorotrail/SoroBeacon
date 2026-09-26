package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/buildinfo"
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
func (emptyStore) AlertCountsByDay(context.Context, int) ([]store.AlertDayCount, error) {
	return nil, nil
}
func (emptyStore) ListAlerts(context.Context, store.AlertFilter) ([]store.Alert, error) {
	return nil, nil
}
func (emptyStore) GetAlert(context.Context, int64) (*store.Alert, error) {
	return nil, store.ErrNotFound
}
func (emptyStore) ListMonitors(context.Context, bool) ([]store.Monitor, error) { return nil, nil }
func (emptyStore) ListChannels(context.Context, bool) ([]store.Channel, error) { return nil, nil }
func (emptyStore) ListMonitorsPage(context.Context, store.ListFilter) ([]store.Monitor, error) {
	return nil, nil
}
func (emptyStore) ListChannelsPage(context.Context, store.ListFilter) ([]store.Channel, error) {
	return nil, nil
}
func (emptyStore) ListSavedSearches(context.Context) ([]store.SavedSearch, error) { return nil, nil }
func (emptyStore) CreateSavedSearch(context.Context, *store.SavedSearch) error    { return nil }
func (emptyStore) GetSavedSearch(context.Context, int64) (*store.SavedSearch, error) {
	return nil, store.ErrNotFound
}
func (emptyStore) DeleteSavedSearch(context.Context, int64) error                      { return nil }
func (emptyStore) SetDefaultSearch(context.Context, int64) error                       { return nil }
func (emptyStore) ClearDefaultSearch(context.Context, int64) error                     { return nil }
func (emptyStore) CreateMonitorTemplate(context.Context, *store.MonitorTemplate) error { return nil }
func (emptyStore) GetMonitorTemplate(context.Context, int64) (*store.MonitorTemplate, error) {
	return nil, store.ErrNotFound
}
func (emptyStore) ListMonitorTemplates(context.Context) ([]store.MonitorTemplate, error) {
	return nil, nil
}
func (emptyStore) UpdateMonitorTemplate(context.Context, *store.MonitorTemplate) error { return nil }
func (emptyStore) DeleteMonitorTemplate(context.Context, int64) error                  { return nil }
func (emptyStore) CreateAuditEntry(context.Context, *store.AuditEntry) error           { return nil }
func (emptyStore) ListAuditEntries(context.Context, store.AuditFilter) ([]store.AuditEntry, error) {
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

type channelDeleteStore struct {
	emptyStore
	channel  store.Channel
	monitors []store.Monitor
	deleted  bool
}

func (s *channelDeleteStore) GetChannel(context.Context, int64) (*store.Channel, error) {
	return &s.channel, nil
}

func (s *channelDeleteStore) ListMonitorsForChannel(context.Context, int64) ([]store.Monitor, error) {
	return s.monitors, nil
}

func (s *channelDeleteStore) DeleteChannel(context.Context, int64) error {
	s.deleted = true
	return nil
}

func newChannelDeleteServer(t *testing.T, st *channelDeleteStore) (*Server, *httptest.Server) {
	t.Helper()
	s, err := New(st, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, httptest.NewServer(s.Routes())
}

func TestDeleteChannelConfirmationAllowsZeroAttachments(t *testing.T) {
	st := &channelDeleteStore{channel: store.Channel{ID: 7, Name: "unused"}}
	_, srv := newChannelDeleteServer(t, st)
	defer srv.Close()

	res, err := http.PostForm(srv.URL+"/channels/7/delete", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "No monitors currently depend") {
		t.Fatalf("initial confirmation = %d, %s", res.StatusCode, body)
	}
	if st.deleted {
		t.Fatal("channel deleted before confirmation")
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err = client.PostForm(srv.URL+"/channels/7/delete", url.Values{"confirm": {"1"}, "confirmed_signature": {""}})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther || !st.deleted {
		t.Fatalf("confirmed delete = %d, deleted=%v", res.StatusCode, st.deleted)
	}
}

func TestDeleteChannelConfirmationRejectsStaleAttachmentCount(t *testing.T) {
	st := &channelDeleteStore{
		channel:  store.Channel{ID: 7, Name: "shared"},
		monitors: []store.Monitor{{ID: 1, Name: "old", ChannelIDs: []int64{7}}},
	}
	_, srv := newChannelDeleteServer(t, st)
	defer srv.Close()

	res, err := http.PostForm(srv.URL+"/channels/7/delete", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	st.monitors = []store.Monitor{
		{ID: 1, Name: "old", ChannelIDs: []int64{7}},
		{ID: 2, Name: "new", ChannelIDs: []int64{7}},
	}

	bodyForm := url.Values{"confirm": {"1"}, "confirmed_signature": {"1:7,;"}}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err = client.PostForm(srv.URL+"/channels/7/delete", bodyForm)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "attachments changed") || !strings.Contains(string(body), "new") {
		t.Fatalf("stale confirmation = %d, %s", res.StatusCode, body)
	}
	if st.deleted {
		t.Fatal("channel deleted using stale attachment count")
	}
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

func TestMonitorsPageLastMatchedCues(t *testing.T) {
	never := store.Monitor{ID: 1, Name: "never-matched", Enabled: true}
	recent := time.Now().UTC().Add(-time.Hour)
	old := time.Now().UTC().Add(-48 * time.Hour)
	ok := store.Monitor{ID: 2, Name: "recent-match", Enabled: true, LastMatchedAt: &recent}
	silent := store.Monitor{ID: 3, Name: "long-silent", Enabled: true, LastMatchedAt: &old}
	s, err := New(matchCueStore{rows: []store.Monitor{never, ok, silent}}, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
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
	for _, want := range []string{
		`Last matched`,
		`class="pill never">never`,
		`class="pill silent">silent`,
		`datetime="` + recent.Format(time.RFC3339),
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("monitors page missing %q in %s", want, html)
		}
	}
	if strings.Count(html, `class="pill never">never`) != 1 {
		t.Fatalf("want one never cue, got html %s", html)
	}
}

type matchCueStore struct {
	emptyStore
	rows []store.Monitor
}

func (m matchCueStore) ListMonitorsPage(context.Context, store.ListFilter) ([]store.Monitor, error) {
	return m.rows, nil
}

func TestMonitorsPageShowsBulkActionBar(t *testing.T) {
	s, err := New(pagingStore{n: 2}, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
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
	for _, want := range []string{
		`action="/monitors/bulk"`,
		`name="ids"`,
		`Enable selected`,
		`Disable selected`,
		`form="bulk-monitors"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("monitors page missing %q in %s", want, html)
		}
	}
}

func TestMonitorsPageFilterControlsAndPreservedPaging(t *testing.T) {
	s, err := New(pagingStore{n: 50}, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/monitors?q=treasury&enabled=true&sort=id")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	if !strings.Contains(html, `name="q"`) || !strings.Contains(html, `value="treasury"`) {
		t.Fatalf("expected name search control populated from q, got:\n%s", html)
	}
	if !strings.Contains(html, `value="true" selected`) && !strings.Contains(html, `value="true" selected>`) {
		if !strings.Contains(html, `<option value="true" selected`) {
			t.Fatalf("expected enabled=true selected, got:\n%s", html)
		}
	}
	if !strings.Contains(html, `/monitors?`) || !strings.Contains(html, `cursor=50`) {
		t.Fatalf("expected Older link to keep cursor, got:\n%s", html)
	}
	if !strings.Contains(html, `q=treasury`) || !strings.Contains(html, `enabled=true`) || !strings.Contains(html, `sort=id`) {
		t.Fatalf("expected Older link to preserve filters, got:\n%s", html)
	}
}

type alertPagingStore struct {
	emptyStore
	n int
}

func (p alertPagingStore) ListAlerts(context.Context, store.AlertFilter) ([]store.Alert, error) {
	out := make([]store.Alert, p.n)
	for i := range out {
		out[i] = store.Alert{ID: int64(i + 1)}
	}
	return out, nil
}

func TestAlertsPageShowsOlderLinkOnFullPage(t *testing.T) {
	s, err := New(alertPagingStore{n: 50}, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
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
	if !strings.Contains(html, `/alerts?cursor=50`) {
		t.Fatalf("expected Older paging link for a full page, got:\n%s", html)
	}
}

func TestAlertsPageFilterControlsAndPreservedPaging(t *testing.T) {
	s, err := New(alertPagingStore{n: 50}, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/alerts?monitor_id=7&rule_id=9&contract_id=CAAA&sort=created_at_asc")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	if !strings.Contains(html, `name="rule_id"`) || !strings.Contains(html, `name="contract_id"`) || !strings.Contains(html, `name="sort"`) {
		t.Fatalf("expected filter controls, got:\n%s", html)
	}
	if !strings.Contains(html, `value="CAAA"`) {
		t.Fatalf("expected contract_id populated, got:\n%s", html)
	}
	if !strings.Contains(html, `value="created_at_asc" selected`) && !strings.Contains(html, `<option value="created_at_asc" selected`) {
		t.Fatalf("expected oldest sort selected, got:\n%s", html)
	}
	if !strings.Contains(html, `/alerts?`) || !strings.Contains(html, `cursor=50`) {
		t.Fatalf("expected Older link to keep cursor, got:\n%s", html)
	}
	if !strings.Contains(html, `monitor_id=7`) || !strings.Contains(html, `rule_id=9`) ||
		!strings.Contains(html, `contract_id=CAAA`) || !strings.Contains(html, `sort=created_at_asc`) {
		t.Fatalf("expected Older link to preserve filters, got:\n%s", html)
	}
}

func TestAlertFilterQueryOmitsDefaults(t *testing.T) {
	if got := alertFilterQuery(0, 0, "", "", ""); got != "" {
		t.Fatalf("defaults = %q, want empty so ?cursor= stays stable", got)
	}
	if got := alertFilterQuery(0, 0, "", "", "created_at_desc"); got != "" {
		t.Fatalf("default sort = %q, want empty", got)
	}
	got := alertFilterQuery(7, 9, "CAAA", "", "created_at_asc")
	if !strings.Contains(got, "monitor_id=7") || !strings.Contains(got, "rule_id=9") ||
		!strings.Contains(got, "contract_id=CAAA") || !strings.Contains(got, "sort=created_at_asc") ||
		!strings.HasSuffix(got, "&") {
		t.Fatalf("got %q", got)
	}
}

// TestAlertExportHref covers the dashboard CSV link: it must target the
// JSON API's export endpoint, drop the (meaningless) cursor, and preserve
// the filters applied to the list on screen.
func TestAlertExportHref(t *testing.T) {
	if got := alertExportHref(0, 0, "", "", ""); got != "/api/v1/alerts.csv" {
		t.Fatalf("defaults = %q, want the bare export URL", got)
	}
	got := string(alertExportHref(7, 9, "CAAA", "", "created_at_asc"))
	for _, want := range []string{"/api/v1/alerts.csv?", "monitor_id=7", "rule_id=9", "contract_id=CAAA", "sort=created_at_asc"} {
		if !strings.Contains(got, want) {
			t.Fatalf("export href %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "cursor=") {
		t.Fatalf("export href must not carry the list cursor: %q", got)
	}
}

func TestAlertsPageRendersExportLink(t *testing.T) {
	s, err := New(alertPagingStore{n: 1}, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/alerts?monitor_id=7&contract_id=CAAA")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	if !strings.Contains(html, `/api/v1/alerts.csv?`) || !strings.Contains(html, `monitor_id=7`) {
		t.Fatalf("expected export link preserving filters, got:\n%s", html)
	}
}

func TestMonitorFilterQueryOmitsDefaults(t *testing.T) {
	if got := monitorFilterQuery("", "", ""); got != "" {
		t.Fatalf("defaults = %q, want empty so ?cursor= stays stable", got)
	}
	if got := monitorFilterQuery("", "", "name"); got != "" {
		t.Fatalf("default sort = %q, want empty", got)
	}
	got := monitorFilterQuery("treasury", "false", "id")
	if !strings.Contains(got, "q=treasury") || !strings.Contains(got, "enabled=false") || !strings.Contains(got, "sort=id") || !strings.HasSuffix(got, "&") {
		t.Fatalf("got %q", got)
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

type duplicateWebStore struct {
	emptyStore
	gotID int64
	err   error
}

func (d *duplicateWebStore) DuplicateMonitor(_ context.Context, id int64) (*store.Monitor, error) {
	d.gotID = id
	if d.err != nil {
		return nil, d.err
	}
	return &store.Monitor{ID: 99, Name: "alpha (copy)", Enabled: false}, nil
}

func (d *duplicateWebStore) GetMonitor(_ context.Context, id int64) (*store.Monitor, error) {
	return &store.Monitor{ID: id, Name: "alpha", Enabled: true, ContractIDs: []string{"C"}}, nil
}

func (d *duplicateWebStore) ListRules(context.Context, int64, bool) ([]store.Rule, error) {
	return nil, nil
}

func TestDuplicateMonitorFormRedirectsToCopy(t *testing.T) {
	st := &duplicateWebStore{}
	s, err := New(st, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	res, err := client.Post(srv.URL+"/monitors/7/duplicate", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", res.StatusCode)
	}
	if loc := res.Header.Get("Location"); loc != "/monitors/99" {
		t.Fatalf("Location = %q, want /monitors/99", loc)
	}
	if st.gotID != 7 {
		t.Fatalf("DuplicateMonitor id = %d, want 7", st.gotID)
	}
}

func TestMonitorPageHasDuplicateButton(t *testing.T) {
	st := &duplicateWebStore{}
	s, err := New(st, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/monitors/7")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	if !strings.Contains(html, `action="/monitors/7/duplicate"`) {
		t.Fatalf("monitor page missing duplicate form, got:\n%s", html)
	}
	if !strings.Contains(html, "Duplicate") {
		t.Fatalf("monitor page missing Duplicate button, got:\n%s", html)
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

func TestTimezoneDefaultIsLabelledUTC(t *testing.T) {
	s := newTestServer(t).WithPoller(stubPosition{pos: poller.Position{
		LastProcessedLedger: 100,
		LatestChainLedger:   125,
		LastSuccessfulPoll:  time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC),
	}})
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	tests := []struct {
		cookie string
		wantTZ string
	}{
		{"", "utc"},
		{"utc", "utc"},
		{"local", "local"},
		{"garbage", "utc"},
	}
	for _, tt := range tests {
		t.Run(tt.cookie, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
			if err != nil {
				t.Fatal(err)
			}
			if tt.cookie != "" {
				req.AddCookie(&http.Cookie{Name: tzCookie, Value: tt.cookie})
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatal(err)
			}
			html := string(body)
			if !strings.Contains(html, `datetime="2026-09-22T00:00:00Z"`) {
				t.Fatalf("missing RFC3339 datetime in %s", html[:min(len(html), 800)])
			}
			if !strings.Contains(html, ">2026-09-22 00:00:00 UTC</time>") {
				t.Fatalf("missing labelled UTC fallback in %s", html[:min(len(html), 800)])
			}
			wantAttr := `data-tz="` + tt.wantTZ + `"`
			if !strings.Contains(html, wantAttr) {
				t.Fatalf("missing %s in %s", wantAttr, html[:min(len(html), 800)])
			}
			if !strings.Contains(html, `action="/timezone"`) || !strings.Contains(html, `name="tz"`) {
				t.Fatal("timezone control missing from layout")
			}
			selected := `<option value="` + tt.wantTZ + `" selected`
			if !strings.Contains(html, selected) {
				t.Fatalf("expected %s, got %s", selected, html[:min(len(html), 800)])
			}
		})
	}
}

func TestSetTimezoneWritesCookieAndRedirects(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t).Routes())
	defer srv.Close()

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/timezone", strings.NewReader("tz=local"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", srv.URL+"/alerts")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusSeeOther)
	}
	if loc := res.Header.Get("Location"); loc != "/alerts" {
		t.Fatalf("Location = %q, want /alerts", loc)
	}
	var got string
	for _, c := range res.Cookies() {
		if c.Name == tzCookie {
			got = c.Value
		}
	}
	if got != "local" {
		t.Fatalf("cookie = %q, want local", got)
	}
}

func TestSetTimezoneRejectsExternalReferer(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t).Routes())
	defer srv.Close()

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/timezone", strings.NewReader("tz=local"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", "https://evil.example/steal")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if loc := res.Header.Get("Location"); loc != "/" {
		t.Fatalf("Location = %q, want /", loc)
	}
}

func TestTemplatesDoNotCallFormatDirectly(t *testing.T) {
	entries, err := templatesFS.ReadDir("templates")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := templatesFS.ReadFile("templates/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), ".Format ") {
			t.Errorf("%s still calls .Format directly", e.Name())
		}
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

func TestThemeAttributeReflectsCookie(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t).Routes())
	defer srv.Close()

	tests := []struct {
		cookie string
		want   string
	}{
		{"", `<html lang="en" data-theme="system">`},
		{"system", `<html lang="en" data-theme="system">`},
		{"light", `<html lang="en" data-theme="light">`},
		{"dark", `<html lang="en" data-theme="dark">`},
		{"garbage", `<html lang="en" data-theme="system">`},
	}
	for _, tt := range tests {
		t.Run(tt.cookie, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
			if err != nil {
				t.Fatal(err)
			}
			if tt.cookie != "" {
				req.AddCookie(&http.Cookie{Name: themeCookie, Value: tt.cookie})
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatal(err)
			}
			html := string(body)
			if !strings.Contains(html, tt.want) {
				head := html
				if len(head) > 400 {
					head = head[:400]
				}
				t.Fatalf("missing %q in %s", tt.want, head)
			}
			if !strings.Contains(html, `action="/theme"`) || !strings.Contains(html, `name="theme"`) {
				t.Fatal("theme toggle missing from layout")
			}
		})
	}
}

func TestSetThemeWritesCookieAndRedirects(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t).Routes())
	defer srv.Close()

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/theme", strings.NewReader("theme=dark"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", srv.URL+"/monitors")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /theme = %d, want 303", res.StatusCode)
	}
	if loc := res.Header.Get("Location"); loc != "/monitors" {
		t.Fatalf("Location = %q, want /monitors", loc)
	}
	var got string
	for _, c := range res.Cookies() {
		if c.Name == themeCookie {
			got = c.Value
		}
	}
	if got != "dark" {
		t.Fatalf("theme cookie = %q, want dark", got)
	}
}

func TestSafeReturnRejectsExternalReferer(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://beacon.test/theme", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "beacon.test"
	req.Header.Set("Referer", "https://evil.example/steal")
	if got := safeReturn(req); got != "/" {
		t.Fatalf("external referer = %q, want /", got)
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
func (alertWithPayloadStore) AlertCountsByDay(context.Context, int) ([]store.AlertDayCount, error) {
	return []store.AlertDayCount{{Day: "2026-09-22", Count: 1}}, nil
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

func (s deliveriesStore) ListDeliveryAttempts(context.Context, int64, string) ([]store.DeliveryAttempt, error) {
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
	if !strings.Contains(html, `hx-post="/alerts/1/deliveries/7/retry"`) {
		t.Errorf("failed row should offer Retry, got:\n%s", html)
	}
	// The success row must not grow a Retry button — a stray click would
	// double-notify. Count the one button from the failed row only.
	if n := strings.Count(html, ">Retry</button>"); n != 1 {
		t.Errorf("Retry buttons = %d, want 1 (failed row only), got:\n%s", n, html)
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
	if cc := res.Header.Get("Cache-Control"); cc != "public, max-age=604800" {
		t.Fatalf("Cache-Control = %q, want public, max-age=604800", cc)
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

func TestAlertChartSVGEmptyWhenAllZero(t *testing.T) {
	days := []store.AlertDayCount{{Day: "2026-09-01", Count: 0}, {Day: "2026-09-02", Count: 0}}
	if got := alertChartSVG(days); got != "" {
		t.Fatalf("zero series SVG = %q, want empty", got)
	}
	if got := alertChartSVG(nil); got != "" {
		t.Fatalf("nil series SVG = %q, want empty", got)
	}
}

func TestAlertChartSVGBars(t *testing.T) {
	days := []store.AlertDayCount{{Day: "2026-09-01", Count: 1}, {Day: "2026-09-02", Count: 3}}
	got := string(alertChartSVG(days))
	for _, want := range []string{"<svg", "UTC", "2026-09-01: 1", "2026-09-02: 3", "role=\"img\""} {
		if !strings.Contains(got, want) {
			t.Fatalf("svg missing %q: %s", want, got)
		}
	}
}

type chartStore struct {
	emptyStore
	days []store.AlertDayCount
}

func (c chartStore) AlertCountsByDay(context.Context, int) ([]store.AlertDayCount, error) {
	return c.days, nil
}
func (chartStore) ListMonitors(context.Context, bool) ([]store.Monitor, error) {
	return []store.Monitor{{ID: 1, Name: "alpha", Enabled: true}}, nil
}
func (chartStore) ListChannels(context.Context, bool) ([]store.Channel, error) {
	return []store.Channel{{ID: 2, Name: "ops", Type: "webhook", Enabled: true}}, nil
}
func (chartStore) ListAlerts(context.Context, store.AlertFilter) ([]store.Alert, error) {
	return []store.Alert{{ID: 1, MonitorID: 1, RuleID: 1, EventID: "e"}}, nil
}

func TestOverviewRendersAlertChart(t *testing.T) {
	body := getHTML(t, chartStore{days: []store.AlertDayCount{
		{Day: "2026-08-24", Count: 0},
		{Day: "2026-08-25", Count: 2},
	}}, "/")
	for _, want := range []string{`class="alert-chart"`, "<svg", "Buckets are UTC", "2026-08-25: 2"} {
		if !strings.Contains(body, want) {
			t.Fatalf("overview missing %q in %s", want, body)
		}
	}
}

func TestOverviewChartEmptyState(t *testing.T) {
	body := getHTML(t, emptyStore{}, "/")
	if !strings.Contains(body, "No alerts in the last 30 days") {
		t.Fatalf("overview missing empty chart state: %s", body)
	}
	if strings.Contains(body, "<svg") {
		t.Fatalf("overview rendered a broken/flat axis on empty series: %s", body)
	}
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

func TestFocusVisibleStyles(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t).Routes())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)

	if strings.Contains(html, "outline: none") || strings.Contains(html, "outline:none") {
		t.Fatal("layout still suppresses outlines without a replacement")
	}
	for _, sel := range []string{
		"a:focus-visible",
		"button:focus-visible",
		"input:focus-visible",
		"textarea:focus-visible",
		"select:focus-visible",
		"summary:focus-visible",
	} {
		if !strings.Contains(html, sel) {
			t.Errorf("missing %s in dashboard CSS", sel)
		}
	}
	if !strings.Contains(html, "outline: 3px solid var(--focus-ring)") {
		t.Fatal("missing 3px :focus-visible outline using --focus-ring")
	}
}

// alertDetailStore backs GET /alerts/{id} with one populated alert,
// optional deliveries, and the related monitor/rule/channel.
type alertDetailStore struct {
	emptyStore
	alert    store.Alert
	monitor  store.Monitor
	rule     store.Rule
	attempts []store.DeliveryAttempt
}

func (s alertDetailStore) GetAlert(_ context.Context, id int64) (*store.Alert, error) {
	if id != s.alert.ID {
		return nil, store.ErrNotFound
	}
	a := s.alert
	return &a, nil
}
func (s alertDetailStore) GetMonitor(_ context.Context, id int64) (*store.Monitor, error) {
	if id != s.monitor.ID {
		return nil, store.ErrNotFound
	}
	m := s.monitor
	return &m, nil
}
func (s alertDetailStore) GetRule(_ context.Context, id int64) (*store.Rule, error) {
	if id != s.rule.ID {
		return nil, store.ErrNotFound
	}
	r := s.rule
	return &r, nil
}
func (s alertDetailStore) ListDeliveryAttempts(context.Context, int64, string) ([]store.DeliveryAttempt, error) {
	return s.attempts, nil
}
func (s alertDetailStore) ListChannels(context.Context, bool) ([]store.Channel, error) {
	return []store.Channel{{ID: 7, Name: "ops-webhook"}}, nil
}

func sampleAlertDetail() alertDetailStore {
	closed := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	return alertDetailStore{
		alert: store.Alert{
			ID:        42,
			MonitorID: 3,
			RuleID:    9,
			EventID:   "evt-abc",
			CreatedAt: time.Date(2026, 9, 21, 12, 1, 0, 0, time.UTC),
			Payload: json.RawMessage(`{
				"contract_id":"CA7QYNF7",
				"event_name":"transfer",
				"ledger":12345,
				"ledger_closed_at":"2026-09-21T12:00:00Z",
				"topics":["transfer","GFROM","GTO"],
				"value":{"i128":"100"}
			}`),
		},
		monitor: store.Monitor{ID: 3, Name: "Treasury"},
		rule:    store.Rule{ID: 9, MonitorID: 3, Type: "token_event"},
		attempts: []store.DeliveryAttempt{
			{ID: 1, ChannelID: 7, Status: "success", ResponseSnippet: "200 OK", AttemptedAt: closed},
			{ID: 2, ChannelID: 7, Status: "failed", ResponseSnippet: "timeout", AttemptedAt: closed},
		},
	}
}

func TestAlertDetailPageRendersPopulatedAlert(t *testing.T) {
	st := sampleAlertDetail()
	s, err := New(st, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/alerts/42")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /alerts/42 = %d, want 200", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	wants := []string{
		"Alert #42",
		`href="/monitors/3"`,
		"Treasury",
		"token_event",
		"#9",
		"CA7QYNF7",
		"transfer",
		"evt-abc",
		"12345",
		"2026-09-21 12:00:00 UTC",
		`class="decoded"`,
		">from<",
		"ops-webhook",
		`class="pill on"`,
		`class="pill off"`,
		"timeout",
		`href="/alerts"`,
	}
	for _, want := range wants {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q in:\n%s", want, html)
		}
	}
	idx := strings.Index(html, ">Alerts</a>")
	if idx < 0 {
		t.Fatal("Alerts nav link missing")
	}
	tag := html[strings.LastIndex(html[:idx], "<a "):idx]
	if !strings.Contains(tag, `class="active"`) {
		t.Fatalf("Alerts nav should stay active on the detail page, tag=%s", tag)
	}
}

func TestAlertDetailPageEmptyDeliveries(t *testing.T) {
	st := sampleAlertDetail()
	st.attempts = nil
	s, err := New(st, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/alerts/42")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "No delivery attempts yet") {
		t.Fatalf("expected empty delivery state, got: %s", body)
	}
}

func TestAlertDetailNotFoundAndMalformed(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t).Routes())
	defer srv.Close()
	for _, path := range []string{"/alerts/99", "/alerts/abc"} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, res.StatusCode)
		}
	}
}

func TestAlertsListLinksToDetail(t *testing.T) {
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
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), `href="/alerts/1"`) {
		t.Fatalf("alerts list should link each row to /alerts/{id}, got:\n%s", body)
	}
}

func TestFooterRendersInjectedBuildInfo(t *testing.T) {
	prevV, prevC := buildinfo.Version, buildinfo.Commit
	t.Cleanup(func() {
		buildinfo.Version, buildinfo.Commit = prevV, prevC
	})
	buildinfo.Version = "v9.8.7"
	buildinfo.Commit = "abc1234"

	srv := httptest.NewServer(newTestServer(t).Routes())
	defer srv.Close()

	paths := []struct {
		path   string
		status int
	}{
		{"/", http.StatusOK},
		{"/monitors", http.StatusOK},
		{"/channels", http.StatusOK},
		{"/alerts", http.StatusOK},
		{"/no-such-page", http.StatusNotFound},
	}
	wantLink := `href="https://github.com/sorotrail/SoroBeacon/commit/abc1234"`
	for _, tt := range paths {
		t.Run(tt.path, func(t *testing.T) {
			res, err := http.Get(srv.URL + tt.path)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != tt.status {
				t.Fatalf("GET %s = %d, want %d", tt.path, res.StatusCode, tt.status)
			}
			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatal(err)
			}
			html := string(body)
			if !strings.Contains(html, `class="site-footer muted"`) {
				t.Fatalf("%s: footer missing class=site-footer (dropped from layout?)\n%s", tt.path, html)
			}
			if !strings.Contains(html, "v9.8.7") {
				t.Fatalf("%s: footer missing injected version, got:\n%s", tt.path, html)
			}
			if !strings.Contains(html, wantLink) {
				t.Fatalf("%s: footer missing commit link %s, got:\n%s", tt.path, wantLink, html)
			}
			if !strings.Contains(html, ">abc1234</a>") {
				t.Fatalf("%s: footer missing linked short commit, got:\n%s", tt.path, html)
			}
		})
	}
}

func TestFooterDevBuildIsPlainText(t *testing.T) {
	prevV, prevC := buildinfo.Version, buildinfo.Commit
	t.Cleanup(func() {
		buildinfo.Version, buildinfo.Commit = prevV, prevC
	})
	buildinfo.Version = "dev"
	buildinfo.Commit = "none"

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
	html := string(body)
	if !strings.Contains(html, ">dev<") {
		t.Fatalf("dev version not rendered, got:\n%s", html)
	}
	if !strings.Contains(html, ">none</span>") {
		t.Fatalf("none commit should be plain text, got:\n%s", html)
	}
	if strings.Contains(html, "github.com/sorotrail/SoroBeacon/commit") {
		t.Fatalf("dev/none must not link to a commit page, got:\n%s", html)
	}
}

func TestFooterEmptyBuildInfoFallsBack(t *testing.T) {
	prevV, prevC := buildinfo.Version, buildinfo.Commit
	t.Cleanup(func() {
		buildinfo.Version, buildinfo.Commit = prevV, prevC
	})
	buildinfo.Version = "  "
	buildinfo.Commit = ""

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
	html := string(body)
	if !strings.Contains(html, ">dev<") {
		t.Fatalf("empty version should fall back to dev, got:\n%s", html)
	}
	if !strings.Contains(html, ">none</span>") {
		t.Fatalf("empty commit should fall back to none, got:\n%s", html)
	}
}

func TestCommitURL(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"abc1234", "https://github.com/sorotrail/SoroBeacon/commit/abc1234"},
		{"ABCDEF0", "https://github.com/sorotrail/SoroBeacon/commit/ABCDEF0"},
		{"deadbeefcafebabe", "https://github.com/sorotrail/SoroBeacon/commit/deadbeefcafebabe"},
		{"none", ""},
		{"dev", ""},
		{"", ""},
		{"abc12", ""},     // too short
		{"not-a-sha", ""}, // hyphen
		{"ggggggg", ""},   // not hex
	}
	for _, tt := range tests {
		if got := commitURL(tt.in); got != tt.want {
			t.Errorf("commitURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// copyIDStore backs overview/monitors/alerts/detail with one long contract
// ID and one long event ID so copy-button markup can be asserted.
const (
	testContractID = "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"
	testEventID    = "000123456789abcdef000123456789abcdef000123456789abcdef00"
)

type copyIDStore struct {
	emptyStore
}

func (copyIDStore) ListMonitors(context.Context, bool) ([]store.Monitor, error) {
	return []store.Monitor{{ID: 1, Name: "alpha", ContractIDs: []string{testContractID}, Enabled: true}}, nil
}

// The monitors page reads through ListMonitorsPage since pagination
// landed; keep both in step so the page is not rendered empty.
func (c copyIDStore) ListMonitorsPage(ctx context.Context, _ store.ListFilter) ([]store.Monitor, error) {
	return c.ListMonitors(ctx, false)
}

func (copyIDStore) GetMonitor(_ context.Context, id int64) (*store.Monitor, error) {
	if id != 1 {
		return nil, store.ErrNotFound
	}
	m := store.Monitor{ID: 1, Name: "alpha", ContractIDs: []string{testContractID}, Enabled: true}
	return &m, nil
}

func (copyIDStore) ListRules(context.Context, int64, bool) ([]store.Rule, error) {
	return nil, nil
}

func (copyIDStore) ListAlerts(context.Context, store.AlertFilter) ([]store.Alert, error) {
	return []store.Alert{{ID: 9, MonitorID: 1, RuleID: 3, EventID: testEventID}}, nil
}

func TestCopyIDButtonsRenderFullIdentifier(t *testing.T) {
	s, err := New(copyIDStore{}, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	pages := []struct {
		path string
		id   string
	}{
		{"/", testEventID},
		{"/alerts", testEventID},
		{"/monitors", testContractID},
		{"/monitors/1", testContractID},
	}
	for _, tt := range pages {
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
			if !strings.Contains(html, `class="copy-btn"`) {
				t.Fatalf("%s: missing copy button (class=copy-btn)", tt.path)
			}
			if !strings.Contains(html, `data-copy="`+tt.id+`"`) {
				t.Fatalf("%s: missing data-copy=%q in markup:\n%s", tt.path, tt.id, html)
			}
			if !strings.Contains(html, `title="`+tt.id+`"`) {
				t.Fatalf("%s: full identifier not recoverable from title", tt.path)
			}
			if !strings.Contains(html, `aria-label="Copy identifier"`) {
				t.Fatalf("%s: copy button missing accessible label", tt.path)
			}
			if !strings.Contains(html, "clip.writeText") {
				t.Fatalf("%s: layout script with clipboard writeText missing", tt.path)
			}
		})
	}
}

// xssEventStore renders an event ID that would break out of an attribute
// if the template failed to escape it.
type xssEventStore struct {
	emptyStore
}

func (xssEventStore) ListAlerts(context.Context, store.AlertFilter) ([]store.Alert, error) {
	return []store.Alert{{ID: 1, EventID: `"><img src=x onerror=alert(1)>`}}, nil
}

func TestCopyIDEscapesIdentifier(t *testing.T) {
	s, err := New(xssEventStore{}, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
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
	if strings.Contains(html, `"><img`) || strings.Contains(html, `"<img src=x`) {
		t.Fatalf("event ID was not HTML-escaped:\n%s", html)
	}
	if !strings.Contains(html, `&lt;img`) {
		t.Fatalf("expected escaped &lt;img in markup:\n%s", html)
	}
	if !strings.Contains(html, `class="copy-btn"`) {
		t.Fatal("copy button missing on escaped-ID page")
	}
}

func TestTruncateID(t *testing.T) {
	const longID = "CDLZFC3SI2Z2B6C4A7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWHGCYSX"
	tests := []struct {
		name string
		in   string
		keep int
		want string
	}{
		{"long", longID, 8, "CDLZFC3S…VWHGCYSX"},
		{"short", "short-id", 8, "short-id"},
		{"exactly at threshold", strings.Repeat("a", 16), 8, strings.Repeat("a", 16)},
		{"empty", "", 8, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateID(tt.in, tt.keep)
			if got != tt.want {
				t.Fatalf("truncateID(%q, %d) = %q, want %q", tt.in, tt.keep, got, tt.want)
			}
		})
	}
}

// monitorWithLongIDStore returns one monitor whose contract ID is a
// 56-character Stellar address, so the monitors table exercises truncation.
type monitorWithLongIDStore struct {
	emptyStore
}

func (monitorWithLongIDStore) ListMonitors(context.Context, bool) ([]store.Monitor, error) {
	return []store.Monitor{{
		ID:          1,
		Name:        "m",
		Enabled:     true,
		ContractIDs: []string{"CDLZFC3SI2Z2B6C4A7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWHGCYSX"},
	}}, nil
}

// The monitors page reads through ListMonitorsPage since pagination
// landed; keep both in step so the page is not rendered empty.
func (m monitorWithLongIDStore) ListMonitorsPage(ctx context.Context, _ store.ListFilter) ([]store.Monitor, error) {
	return m.ListMonitors(ctx, false)
}

func TestMonitorsTableTruncatesLongContractIDs(t *testing.T) {
	s, err := New(monitorWithLongIDStore{}, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
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
	const full = "CDLZFC3SI2Z2B6C4A7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWHGCYSX"
	if !strings.Contains(html, `title="`+full+`"`) {
		t.Fatalf("expected title with full contract ID, got:\n%s", html)
	}
	if !strings.Contains(html, "CDLZFC3S…VWHGCYSX") {
		t.Fatalf("expected middle-truncated contract ID, got:\n%s", html)
	}
	if strings.Count(html, full) != 2 {
		t.Fatalf("full contract ID should appear twice (title + data-copy), got %d in:\n%s", strings.Count(html, full), html)
	}
}

func TestRelTime(t *testing.T) {
	now := time.Date(2026, 9, 21, 16, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		t    time.Time
		now  time.Time
		want string
	}{
		{"zero time", time.Time{}, now, ""},
		{"exactly now", now, now, "just now"},
		{"under one second", now.Add(-500 * time.Millisecond), now, "just now"},
		{"one second", now.Add(-1 * time.Second), now, "1s ago"},
		{"59 seconds (last second bucket)", now.Add(-59 * time.Second), now, "59s ago"},
		{"one minute (first minute bucket)", now.Add(-time.Minute), now, "1m ago"},
		{"59 minutes (last minute bucket)", now.Add(-59 * time.Minute), now, "59m ago"},
		{"one hour (first hour bucket)", now.Add(-time.Hour), now, "1h ago"},
		{"23 hours (last hour bucket)", now.Add(-23 * time.Hour), now, "23h ago"},
		{"one day (first day bucket)", now.Add(-24 * time.Hour), now, "1d ago"},
		{"two days", now.Add(-48 * time.Hour), now, "2d ago"},
		{"future clock skew", now.Add(5 * time.Second), now, "just now"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := relTime(tt.t, tt.now)
			if got != tt.want {
				t.Fatalf("relTime(%v, %v) = %q, want %q", tt.t, tt.now, got, tt.want)
			}
		})
	}
}

type timestampedPagesStore struct {
	emptyStore
	at time.Time
}

func (s timestampedPagesStore) GetStats(context.Context) (store.Stats, error) {
	return store.Stats{LastPollAt: s.at}, nil
}
func (s timestampedPagesStore) ListAlerts(context.Context, store.AlertFilter) ([]store.Alert, error) {
	return []store.Alert{{ID: 1, MonitorID: 1, RuleID: 2, EventID: "evt-1", CreatedAt: s.at}}, nil
}
func (s timestampedPagesStore) ListMonitors(context.Context, bool) ([]store.Monitor, error) {
	return []store.Monitor{{ID: 1, Name: "main", CreatedAt: s.at}}, nil
}
func (s timestampedPagesStore) ListChannels(context.Context, bool) ([]store.Channel, error) {
	return []store.Channel{{ID: 3, Name: "ops", Type: "webhook", CreatedAt: s.at}}, nil
}

// The monitors and channels pages read through the paginated calls since
// pagination landed; keep both in step so the pages are not rendered empty.
func (s timestampedPagesStore) ListMonitorsPage(ctx context.Context, _ store.ListFilter) ([]store.Monitor, error) {
	return s.ListMonitors(ctx, false)
}

func (s timestampedPagesStore) ListChannelsPage(ctx context.Context, _ store.ListFilter) ([]store.Channel, error) {
	return s.ListChannels(ctx, false)
}

func TestPagesShowRelativeTimesNextToAbsolute(t *testing.T) {
	at := time.Date(2026, 9, 21, 14, 32, 11, 0, time.UTC)
	s, err := New(timestampedPagesStore{at: at}, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	abs := at.Format("2006-01-02 15:04:05")
	for _, path := range []string{"/", "/alerts", "/monitors", "/channels"} {
		t.Run(path, func(t *testing.T) {
			res, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatal(err)
			}
			html := string(body)
			if !strings.Contains(html, abs) {
				t.Fatalf("%s: expected absolute timestamp %q, got:\n%s", path, abs, html)
			}
			if !strings.Contains(html, "ago") && !strings.Contains(html, "just now") {
				t.Fatalf("%s: expected a relative time next to the absolute timestamp, got:\n%s", path, html)
			}
		})
	}
}
