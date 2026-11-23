package web

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// monitorPanelStore is the monitor page's whole dependency set: one monitor,
// the recent-alerts query it is allowed to run, and the statistics the panel
// reports. listFilter records what the page asked for, because a panel with no
// bound reads like the alerts page and a panel that forgets the monitor id
// reads another monitor's alerts as its own.
type monitorPanelStore struct {
	emptyStore
	monitor    store.Monitor
	alerts     []store.Alert
	stats      store.MonitorStats
	listFilter store.AlertFilter
	listCalls  int
	budget     *store.DeliveryBudget
}

func (s *monitorPanelStore) GetDeliveryBudget(context.Context, int64, time.Time) (*store.DeliveryBudget, error) {
	if s.budget == nil {
		return nil, store.ErrNotFound
	}
	return s.budget, nil
}

func (s *monitorPanelStore) GetMonitor(_ context.Context, id int64) (*store.Monitor, error) {
	m := s.monitor
	m.ID = id
	return &m, nil
}

func (s *monitorPanelStore) ListRules(context.Context, int64, bool) ([]store.Rule, error) {
	return []store.Rule{{ID: 3, Type: "event_emitted", Enabled: true,
		Params: json.RawMessage(`{"event_name":"transfer"}`)}}, nil
}

func (s *monitorPanelStore) ListAlerts(_ context.Context, f store.AlertFilter) ([]store.Alert, error) {
	s.listFilter = f
	s.listCalls++
	return s.alerts, nil
}

func (s *monitorPanelStore) GetMonitorStats(context.Context, int64) (store.MonitorStats, error) {
	return s.stats, nil
}

func testAlert(id int64, at time.Time) store.Alert {
	return store.Alert{
		ID: id, MonitorID: 7, RuleID: 3, EventID: "00000012-00000001",
		Payload:   json.RawMessage(`{"contract_id":"CAAAAA","event_name":"transfer"}`),
		CreatedAt: at,
		Severity:  store.SeverityWarning,
	}
}

func TestMonitorPageAsksForABoundedRecentAlertsPanel(t *testing.T) {
	at := time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC)
	st := &monitorPanelStore{
		monitor: store.Monitor{ID: 7, Name: "vault", Enabled: true},
		alerts:  []store.Alert{testAlert(41, at), testAlert(40, at.Add(-time.Hour))},
	}
	html := getHTML(t, st, "/monitors/7")

	if st.listCalls != 1 {
		t.Fatalf("recent alerts queried %d times, want 1", st.listCalls)
	}
	if st.listFilter.MonitorID != 7 {
		t.Fatalf("panel listed monitor %d, want the monitor on screen", st.listFilter.MonitorID)
	}
	if st.listFilter.Limit != recentAlertsPanelSize {
		t.Fatalf("panel limit = %d, want the small summary limit %d", st.listFilter.Limit, recentAlertsPanelSize)
	}

	for _, want := range []string{
		`href="/alerts/41"`, "00000012-00000001",
		// The panel is a summary, so it needs a way to see everything.
		`href="/alerts?monitor_id=7"`,
		// Timestamps go through formatTime, which is what makes the timezone
		// preference and the no-JavaScript UTC label work.
		`<time datetime="` + at.Format(time.RFC3339) + `"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("monitor page missing %q, got:\n%s", want, html)
		}
	}
}

// TestMonitorPagePanelAndCueAgree covers both halves of #326's acceptance
// criteria: a monitor that has never fired says so in useful copy rather than
// an empty table, and the panel cannot report a different history from the
// last-matched cue rendered above it.
func TestMonitorPagePanelAndCueAgree(t *testing.T) {
	at := time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC)
	fired := &at
	cases := []struct {
		name        string
		lastMatched *time.Time
		stats       store.MonitorStats
		alerts      []store.Alert
		wantNever   bool
		wantRows    bool
		wantPruned  bool
	}{{
		name:      "never matched",
		stats:     store.MonitorStats{MonitorID: 7},
		wantNever: true,
	}, {
		name:        "matched and alerting",
		lastMatched: fired,
		stats:       store.MonitorStats{MonitorID: 7, Alerts: 2},
		alerts:      []store.Alert{testAlert(41, at)},
		wantRows:    true,
	}, {
		name:        "matched but nothing retained",
		lastMatched: fired,
		stats:       store.MonitorStats{MonitorID: 7},
		wantPruned:  true,
	}}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			st := &monitorPanelStore{
				monitor: store.Monitor{ID: 7, Name: "vault", Enabled: true, LastMatchedAt: tt.lastMatched},
				alerts:  tt.alerts,
				stats:   tt.stats,
			}
			html := getHTML(t, st, "/monitors/7")

			// The pill and the panel are two renderings of one question, so
			// whichever answer the cue gives the page has to agree with.
			cueNever := strings.Contains(html, `class="pill never">never`)
			if cueNever != tt.wantNever {
				t.Fatalf("last-matched cue never=%v but panel never=%v: the page disagrees with itself",
					cueNever, tt.wantNever)
			}
			if got := strings.Contains(html, "has never fired"); got != tt.wantNever {
				t.Fatalf("never-fired copy = %v, want %v, got:\n%s", got, tt.wantNever, html)
			}
			if got := strings.Contains(html, `href="/alerts/41"`); got != tt.wantRows {
				t.Fatalf("recent alert rows = %v, want %v", got, tt.wantRows)
			}
			if got := strings.Contains(html, "older than the alerts kept"); got != tt.wantPruned {
				t.Fatalf("pruned-history line = %v, want %v", got, tt.wantPruned)
			}
		})
	}
}

func TestMonitorPageReportsPerRuleStatistics(t *testing.T) {
	at := time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC)
	st := &monitorPanelStore{
		monitor: store.Monitor{ID: 7, Name: "vault", Enabled: true, LastMatchedAt: &at},
		alerts:  []store.Alert{testAlert(41, at)},
		stats: store.MonitorStats{
			MonitorID: 7, Alerts: 128, AlertsLast24h: 3, AlertsLast7d: 21,
			LastAlertAt:  &at,
			DeliveriesOK: 120, DeliveriesFail: 4,
			Rules: []store.RuleMatchCount{
				{RuleID: 3, Type: "event_emitted", Alerts: 128},
				{RuleID: 4, Type: "value_threshold", Alerts: 0},
			},
		},
	}
	html := getHTML(t, st, "/monitors/7")
	for _, want := range []string{
		"<b>128</b> alerts", "<b>3</b> in 24h", "<b>21</b> in 7d",
		"<b>120</b> delivered", "<b>4</b> delivery failed",
		"Most recent alert:", `<time datetime="` + at.Format(time.RFC3339) + `"`,
		"#3", "event_emitted", "#4", "value_threshold",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("statistics panel missing %q, got:\n%s", want, html)
		}
	}
	// A rule that has never matched is reported as a zero, not left off: the
	// operator asking "which of these rules is dead" needs the zeroes.
	if !strings.Contains(html, "<td>#4</td><td>value_threshold</td><td>0</td>") {
		t.Fatalf("a never-matching rule is not shown as zero, got:\n%s", html)
	}
}

// TestMonitorPageQuietStatisticsStayQuiet is #104's explicit requirement: a
// monitor with no alerts shows a stated empty state rather than a wall of
// zeroes, which an operator cannot distinguish from a broken counter.
func TestMonitorPageQuietStatisticsStayQuiet(t *testing.T) {
	st := &monitorPanelStore{
		monitor: store.Monitor{ID: 7, Name: "vault", Enabled: true},
		stats:   store.MonitorStats{MonitorID: 7},
	}
	html := getHTML(t, st, "/monitors/7")
	if strings.Contains(html, `<div class="stats">`) {
		t.Fatalf("quiet monitor rendered stat cards, got:\n%s", html)
	}
	if !strings.Contains(html, "No alerts yet") {
		t.Fatalf("quiet monitor did not state its empty state, got:\n%s", html)
	}
}

func TestMonitorPageShowsRemainingDeliveryBudget(t *testing.T) {
	limit := 50
	start := time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC)
	st := &monitorPanelStore{
		monitor: store.Monitor{ID: 7, Name: "vault", Enabled: true},
		budget:  &store.DeliveryBudget{MonitorID: 7, Limit: &limit, Used: 20, WindowStart: start},
	}
	html := getHTML(t, st, "/monitors/7")
	for _, want := range []string{"Delivery budget", "<b>30</b> of 50 deliveries left today", "active"} {
		if !strings.Contains(html, want) {
			t.Fatalf("monitor page missing %q, got:\n%s", want, html)
		}
	}
	if strings.Contains(html, "Delivery stopped") {
		t.Fatal("a budget with units left must not claim delivery stopped")
	}
}

func TestMonitorPageSaysWhenDeliveryBudgetIsExhausted(t *testing.T) {
	limit := 5
	at := time.Date(2026, 3, 5, 9, 0, 0, 0, time.UTC)
	st := &monitorPanelStore{
		monitor: store.Monitor{ID: 7, Name: "vault", Enabled: true},
		budget: &store.DeliveryBudget{MonitorID: 7, Limit: &limit, Used: 5,
			WindowStart: time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC), ExhaustedAt: &at},
	}
	html := getHTML(t, st, "/monitors/7")
	for _, want := range []string{"exhausted", "<b>0</b> of 5 deliveries left today", "Delivery stopped"} {
		if !strings.Contains(html, want) {
			t.Fatalf("monitor page missing %q, got:\n%s", want, html)
		}
	}
}

func TestMonitorPageHidesBudgetPanelWhenUnlimited(t *testing.T) {
	st := &monitorPanelStore{
		monitor: store.Monitor{ID: 7, Name: "vault", Enabled: true},
		budget:  &store.DeliveryBudget{MonitorID: 7},
	}
	if html := getHTML(t, st, "/monitors/7"); strings.Contains(html, "Delivery budget") {
		t.Fatal("an unlimited monitor must not render a budget panel")
	}
}
