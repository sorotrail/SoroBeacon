package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// alertsStore returns a fixed number of alerts from ListAlerts regardless
// of the filter, so tests can drive listAlerts' page-size heuristic
// directly without a real database.
type alertsStore struct {
	store.Store
	n   int
	got store.AlertFilter
}

func (a *alertsStore) ListAlerts(ctx context.Context, f store.AlertFilter) ([]store.Alert, error) {
	a.got = f
	alerts := make([]store.Alert, a.n)
	for i := range alerts {
		alerts[i] = store.Alert{ID: int64(i + 1)}
	}
	return alerts, nil
}

func getAlerts(t *testing.T, n int, query string) map[string]any {
	t.Helper()
	srv := httptest.NewServer(newProbeServer(&alertsStore{n: n}, &fakeRPC{}))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/alerts" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /alerts%s = %d, want 200", query, res.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestListAlerts_NextCursorOmittedOnShortPage(t *testing.T) {
	// 3 rows against the default (unclamped) limit of 50: a short page,
	// there's nothing older to fetch.
	body := getAlerts(t, 3, "")
	if got := body["next_cursor"]; got != "" {
		t.Fatalf("next_cursor on a short page = %q, want empty", got)
	}
}

func TestListAlerts_NextCursorPresentOnFullPage(t *testing.T) {
	// Exactly the requested limit worth of rows: might be more, cursor
	// must be set so the client can page for them.
	body := getAlerts(t, 5, "?limit=5")
	if got, ok := body["next_cursor"].(string); !ok || got == "" {
		t.Fatalf("next_cursor on a full page = %v, want the last alert's id", body["next_cursor"])
	}
}

func TestListAlerts_NextCursorPresentOnFullDefaultPage(t *testing.T) {
	// No ?limit given: the effective limit is postgres.go's default of
	// 50, not the zero value of f.Limit. A full 50-row page must still
	// set next_cursor, not treat 50 != 0 as "short".
	body := getAlerts(t, 50, "")
	if got, ok := body["next_cursor"].(string); !ok || got == "" {
		t.Fatalf("next_cursor on a full default-limit page = %v, want the last alert's id", body["next_cursor"])
	}
}

func TestListAlerts_NextCursorOmittedOnEmptyPage(t *testing.T) {
	body := getAlerts(t, 0, "")
	if got := body["next_cursor"]; got != "" {
		t.Fatalf("next_cursor on an empty page = %q, want empty", got)
	}
}

func TestListAlerts_SortAndFiltersPassedThrough(t *testing.T) {
	st := &alertsStore{n: 1}
	code, _ := getJSON(t, st, "/alerts?rule_id=9&contract_id=CAAA&sort=created_at_asc&monitor_id=3&limit=10&cursor=42")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if st.got.RuleID != 9 || st.got.ContractID != "CAAA" || st.got.Sort != "created_at_asc" ||
		st.got.MonitorID != 3 || st.got.Limit != 10 || st.got.AfterID != 42 {
		t.Fatalf("filter = %+v", st.got)
	}
}

func TestListAlerts_InvalidSort(t *testing.T) {
	code, body := getJSON(t, &alertsStore{n: 1}, "/alerts?sort=id")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%v, want 400", code, body)
	}
}

func TestListAlerts_InvalidRuleID(t *testing.T) {
	code, body := getJSON(t, &alertsStore{n: 1}, "/alerts?rule_id=abc")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%v, want 400", code, body)
	}
}

type scriptedNotifier struct {
	err error
	n   int
}

func (s *scriptedNotifier) Send(context.Context, notify.Alert) error {
	s.n++
	return s.err
}

type retryStore struct {
	store.Store
	alert    *store.Alert
	channel  *store.Channel
	attempts []store.DeliveryAttempt
}

func (s *retryStore) GetAlert(context.Context, int64) (*store.Alert, error) {
	if s.alert == nil {
		return nil, store.ErrNotFound
	}
	return s.alert, nil
}
func (s *retryStore) GetChannel(_ context.Context, id int64) (*store.Channel, error) {
	if s.channel == nil || s.channel.ID != id {
		return nil, store.ErrNotFound
	}
	return s.channel, nil
}
func (s *retryStore) ListDeliveryAttempts(context.Context, int64, string) ([]store.DeliveryAttempt, error) {
	return s.attempts, nil
}
func (s *retryStore) RecordDeliveryAttempt(_ context.Context, d *store.DeliveryAttempt) error {
	d.ID = int64(len(s.attempts) + 1)
	d.AttemptedAt = time.Now()
	s.attempts = append(s.attempts, *d)
	return nil
}
func (s *retryStore) GetMonitor(context.Context, int64) (*store.Monitor, error) {
	return &store.Monitor{ID: 1, Name: "ops"}, nil
}
func (s *retryStore) GetRule(context.Context, int64) (*store.Rule, error) {
	return &store.Rule{ID: 2, Type: "event_emitted"}, nil
}

func retryServer(t *testing.T, st store.Store, n *scriptedNotifier) *httptest.Server {
	t.Helper()
	f := notify.DefaultFactory()
	f.Register("mock", func(json.RawMessage) (notify.Notifier, error) { return n, nil })
	s := New(st, rules.NewRegistry(), f, &fakeRPC{}, discardLogger())
	return httptest.NewServer(s.Routes())
}

func postRetry(t *testing.T, srv *httptest.Server, path string) (int, map[string]any, string) {
	t.Helper()
	res, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	return res.StatusCode, body, string(raw)
}

func TestRetryDelivery_Success(t *testing.T) {
	st := &retryStore{
		alert:   &store.Alert{ID: 10, MonitorID: 1, RuleID: 2, EventID: "ev-1", Payload: json.RawMessage(`{"contract_id":"C","event_name":"xfer"}`)},
		channel: &store.Channel{ID: 7, Name: "ops", Type: "mock", Config: json.RawMessage(`{}`), Enabled: true},
		attempts: []store.DeliveryAttempt{
			{ID: 1, AlertID: 10, ChannelID: 7, Status: "failed", ResponseSnippet: "timeout", AttemptedAt: time.Now().Add(-time.Hour)},
		},
	}
	n := &scriptedNotifier{}
	srv := retryServer(t, st, n)
	defer srv.Close()

	code, body, raw := postRetry(t, srv, "/alerts/10/deliveries/7/retry")
	if code != http.StatusOK {
		t.Fatalf("retry success = %d %s, want 200", code, raw)
	}
	if body["status"] != "success" {
		t.Fatalf("attempt status = %v, want success; body %s", body["status"], raw)
	}
	if n.n != 1 {
		t.Fatalf("notifier calls = %d, want 1", n.n)
	}
	if len(st.attempts) != 2 {
		t.Fatalf("attempts after retry = %d, want 2 (history preserved)", len(st.attempts))
	}
}

func TestRetryDelivery_AlreadySucceededConflict(t *testing.T) {
	st := &retryStore{
		alert:   &store.Alert{ID: 10, MonitorID: 1, RuleID: 2},
		channel: &store.Channel{ID: 7, Type: "mock", Config: json.RawMessage(`{}`), Enabled: true},
		attempts: []store.DeliveryAttempt{
			{ChannelID: 7, Status: "failed", AttemptedAt: time.Now().Add(-2 * time.Hour)},
			{ChannelID: 7, Status: "success", AttemptedAt: time.Now().Add(-time.Hour)},
		},
	}
	n := &scriptedNotifier{}
	srv := retryServer(t, st, n)
	defer srv.Close()

	code, body, raw := postRetry(t, srv, "/alerts/10/deliveries/7/retry")
	if code != http.StatusConflict {
		t.Fatalf("already succeeded = %d %s, want 409", code, raw)
	}
	if body["error"] != notify.ErrAlreadySucceeded.Error() {
		t.Fatalf("error = %v, want %q", body["error"], notify.ErrAlreadySucceeded.Error())
	}
	if n.n != 0 {
		t.Fatalf("notifier must not be called on 409, got %d", n.n)
	}
}

func TestRetryDelivery_MissingChannel(t *testing.T) {
	st := &retryStore{
		alert:    &store.Alert{ID: 10},
		channel:  &store.Channel{ID: 7, Type: "mock", Enabled: true},
		attempts: []store.DeliveryAttempt{{ChannelID: 7, Status: "failed", AttemptedAt: time.Now().Add(-time.Hour)}},
	}
	srv := retryServer(t, st, &scriptedNotifier{})
	defer srv.Close()

	code, _, raw := postRetry(t, srv, "/alerts/10/deliveries/99/retry")
	if code != http.StatusNotFound {
		t.Fatalf("missing channel = %d %s, want 404", code, raw)
	}
}

// deliveriesAPIStore records the status filter listDeliveries passed
// through. Filtering itself is the store's job; the handler's job is
// validating the query param and wiring it.
type deliveriesAPIStore struct {
	store.Store
	gotAlertID int64
	gotStatus  string
	called     bool
	attempts   []store.DeliveryAttempt
}

func (d *deliveriesAPIStore) ListDeliveryAttempts(_ context.Context, alertID int64, status string) ([]store.DeliveryAttempt, error) {
	d.called = true
	d.gotAlertID = alertID
	d.gotStatus = status
	return d.attempts, nil
}

func TestListDeliveries_StatusFilter(t *testing.T) {
	all := []store.DeliveryAttempt{
		{ID: 1, Status: store.DeliveryStatusFailed},
		{ID: 2, Status: store.DeliveryStatusSuccess},
	}
	tests := []struct {
		name       string
		query      string
		wantStatus int
		wantFilter string
		wantLen    int
		wantCalled bool
	}{
		{name: "omitted returns everything", query: "", wantStatus: http.StatusOK, wantFilter: "", wantLen: 2, wantCalled: true},
		{name: "status=failed", query: "?status=failed", wantStatus: http.StatusOK, wantFilter: store.DeliveryStatusFailed, wantLen: 2, wantCalled: true},
		{name: "status=success", query: "?status=success", wantStatus: http.StatusOK, wantFilter: store.DeliveryStatusSuccess, wantLen: 2, wantCalled: true},
		{name: "invalid status is 400", query: "?status=pending", wantStatus: http.StatusBadRequest, wantCalled: false},
		{name: "empty status query is omitted", query: "?status=", wantStatus: http.StatusOK, wantFilter: "", wantLen: 2, wantCalled: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &deliveriesAPIStore{attempts: all}
			srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
			defer srv.Close()

			res, err := http.Get(srv.URL + "/alerts/42/deliveries" + tt.query)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != tt.wantStatus {
				t.Fatalf("GET /alerts/42/deliveries%s = %d, want %d", tt.query, res.StatusCode, tt.wantStatus)
			}
			if st.called != tt.wantCalled {
				t.Fatalf("store called = %v, want %v", st.called, tt.wantCalled)
			}
			if !tt.wantCalled {
				var env map[string]any
				if err := json.NewDecoder(res.Body).Decode(&env); err != nil {
					t.Fatal(err)
				}
				if env["error"] == nil || env["code"] == nil {
					t.Fatalf("400 envelope missing error/code: %v", env)
				}
				return
			}
			if st.gotAlertID != 42 {
				t.Fatalf("alertID = %d, want 42", st.gotAlertID)
			}
			if st.gotStatus != tt.wantFilter {
				t.Fatalf("status filter = %q, want %q", st.gotStatus, tt.wantFilter)
			}
			var got []store.DeliveryAttempt
			if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if len(got) != tt.wantLen {
				t.Fatalf("got %d attempts, want %d", len(got), tt.wantLen)
			}
		})
	}
}
