package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// alertsStore returns a fixed number of alerts from ListAlerts regardless
// of the filter, so tests can drive listAlerts' page-size heuristic
// directly without a real database.
type alertsStore struct {
	store.Store
	n int
}

func (a *alertsStore) ListAlerts(ctx context.Context, f store.AlertFilter) ([]store.Alert, error) {
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
