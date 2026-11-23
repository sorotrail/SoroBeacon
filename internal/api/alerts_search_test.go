package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// searchStore keeps the filter the handler built and answers with an empty
// page. The point is the parsing contract — what the API accepts and what it
// hands to the store — since the matching itself is covered backend-neutral by
// the store conformance suite.
type searchStore struct {
	store.Store
	got   store.AlertFilter
	calls int
}

func (s *searchStore) ListAlerts(_ context.Context, f store.AlertFilter) ([]store.Alert, error) {
	s.calls++
	s.got = f
	return nil, nil
}

// ListMonitors is what the CSV export looks up monitor names for before it
// writes anything; the embedded nil interface would panic instead.
func (s *searchStore) ListMonitors(context.Context, bool) ([]store.Monitor, error) {
	return nil, nil
}

// alertSearchGET issues one GET /alerts and returns the status together with
// the filter the handler produced.
func alertSearchGET(t *testing.T, st *searchStore, query string) int {
	t.Helper()
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/alerts" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	return res.StatusCode
}

func TestParseAlertFilterSearchTerm(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  int
		wantQ string
	}{
		{"term is passed through trimmed", "?q=%20transfer%20", http.StatusOK, "transfer"},
		{"an empty term is no filter", "?q=", http.StatusOK, ""},
		{"whitespace is no filter, not a match for nothing", "?q=%20%20", http.StatusOK, ""},
		{"the cap is inclusive", "?q=" + strings.Repeat("a", store.MaxAlertSearchLen), http.StatusOK, strings.Repeat("a", store.MaxAlertSearchLen)},
		{"one over the cap is rejected", "?q=" + strings.Repeat("a", store.MaxAlertSearchLen+1), http.StatusBadRequest, ""},
		// The cap bounds what a search may cost, so it is counted in characters:
		// measuring bytes would reject a 200-character term in any non-Latin
		// script while accepting a 256-byte Latin one.
		{"the cap counts characters, not bytes", "?q=" + strings.Repeat("\u00e9", store.MaxAlertSearchLen), http.StatusOK, strings.Repeat("\u00e9", store.MaxAlertSearchLen)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &searchStore{}
			if code := alertSearchGET(t, st, tc.query); code != tc.want {
				t.Fatalf("GET /alerts%s = %d, want %d", tc.query, code, tc.want)
			}
			if st.got.Query != tc.wantQ {
				t.Errorf("filter Query = %q, want %q", st.got.Query, tc.wantQ)
			}
			called := tc.want == http.StatusOK
			if (st.calls > 0) != called {
				t.Errorf("store called = %v, want %v", st.calls > 0, called)
			}
		})
	}
}

// TestParseAlertFilterSearchComposes is the paging guarantee: the search has to
// survive as part of the same filter as monitor_id and the cursor, or the
// second page of a searched list quietly becomes the second page of everything.
func TestParseAlertFilterSearchComposes(t *testing.T) {
	st := &searchStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/alerts?monitor_id=7&q=transfer&cursor=42&limit=5")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if st.got.MonitorID != 7 || st.got.AfterID != 42 || st.got.Query != "transfer" {
		t.Fatalf("filter = %+v, want monitor 7, cursor 42 and search transfer together", st.got)
	}
}

// The CSV export shares the filter parser, so a search the list honours has to
// bound the export too — an operator exporting "everything matching X" would
// otherwise get everything.
func TestExportAlertsCSVSearches(t *testing.T) {
	st := &searchStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/alerts.csv?q=transfer")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if st.got.Query != "transfer" {
		t.Fatalf("export filter Query = %q, want transfer", st.got.Query)
	}
}

type monitorStatsStore struct {
	store.Store
	stats store.MonitorStats
	err   error
	got   int64
}

func (s *monitorStatsStore) GetMonitorStats(_ context.Context, id int64) (store.MonitorStats, error) {
	s.got = id
	return s.stats, s.err
}

func getMonitorStats(t *testing.T, st *monitorStatsStore, path string) (int, map[string]any) {
	t.Helper()
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body map[string]any
	if res.StatusCode == http.StatusOK {
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return res.StatusCode, body
}

func TestMonitorStatsServesCounts(t *testing.T) {
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	st := &monitorStatsStore{stats: store.MonitorStats{
		MonitorID: 7, Alerts: 12, AlertsLast24h: 2, AlertsLast7d: 5, LastAlertAt: &at,
		DeliveriesOK: 9, DeliveriesFail: 3,
		Rules: []store.RuleMatchCount{{RuleID: 1, Type: "event_emitted", Alerts: 12}, {RuleID: 2, Type: "value_threshold", Alerts: 0}},
	}}
	code, body := getMonitorStats(t, st, "/monitors/7/stats")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if st.got != 7 {
		t.Fatalf("store got monitor %d, want 7", st.got)
	}
	for key, want := range map[string]any{
		"monitor_id": float64(7), "alerts": float64(12), "alerts_last_24h": float64(2),
		"alerts_last_7d": float64(5), "deliveries_succeeded": float64(9), "deliveries_failed": float64(3),
		"last_alert_at": "2026-09-01T10:00:00Z",
	} {
		if got := body[key]; got != want {
			t.Errorf("body[%q] = %v, want %v", key, got, want)
		}
	}
	rules, ok := body["rules"].([]any)
	if !ok || len(rules) != 2 {
		t.Fatalf("body[rules] = %v, want the two rules", body["rules"])
	}
	// A rule that never matched stays visible as a zero instead of dropping out.
	if second, ok := rules[1].(map[string]any); !ok || second["alerts"] != float64(0) {
		t.Fatalf("rules[1] = %v, want an explicit zero alert count", rules[1])
	}
}

// An unknown monitor is 404, not zeroes: the whole reason this endpoint exists
// is to tell a quiet monitor apart from an absent one.
func TestMonitorStatsUnknownMonitorIs404(t *testing.T) {
	code, _ := getMonitorStats(t, &monitorStatsStore{err: store.ErrNotFound}, "/monitors/999/stats")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

// A monitor with no rules must serialise rules as an empty array. Null would
// make a client distinguish "no rules" from "no data" by type rather than by
// value, which is the ambiguity this endpoint is meant to remove.
func TestMonitorStatsRulesSerialiseAsEmptyArray(t *testing.T) {
	code, body := getMonitorStats(t, &monitorStatsStore{stats: store.MonitorStats{MonitorID: 3}}, "/monitors/3/stats")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	rules, ok := body["rules"].([]any)
	if !ok || len(rules) != 0 {
		t.Fatalf("body[rules] = %#v, want an empty array", body["rules"])
	}
	if _, ok := body["last_alert_at"]; ok {
		t.Fatalf("body[last_alert_at] present for a monitor that has never alerted")
	}
}

func TestMonitorStatsRejectsMalformedID(t *testing.T) {
	code, _ := getMonitorStats(t, &monitorStatsStore{}, "/monitors/not-a-number/stats")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}
