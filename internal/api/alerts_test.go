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
