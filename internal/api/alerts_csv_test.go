package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// csvStore is a store.Store fake for the CSV export tests. It synthesizes
// alerts on demand so keyset paging and the row cap can be exercised without
// a database; when alerts is non-nil they are returned as a single page
// instead, which is what the escaping tests need.
type csvStore struct {
	store.Store
	alerts   []store.Alert
	total    int64
	monitors []store.Monitor
	got      store.AlertFilter
	calls    int
}

func (c *csvStore) ListMonitors(context.Context, bool) ([]store.Monitor, error) {
	return c.monitors, nil
}

func (c *csvStore) ListAlerts(_ context.Context, f store.AlertFilter) ([]store.Alert, error) {
	c.got = f
	c.calls++
	if c.alerts != nil {
		return c.alerts, nil
	}
	avail := c.total - f.AfterID
	if avail <= 0 || f.Limit <= 0 {
		return []store.Alert{}, nil
	}
	n := f.Limit
	if int64(n) > avail {
		n = int(avail)
	}
	out := make([]store.Alert, n)
	for i := range out {
		out[i] = store.Alert{ID: f.AfterID + 1 + int64(i)}
	}
	return out, nil
}

func readCSV(t *testing.T, r io.Reader) [][]string {
	t.Helper()
	rows, err := csv.NewReader(r).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	return rows
}

// exportCSV fetches /alerts.csv against a fresh server and returns the
// response plus the body. The caller owns res.Body.
func exportCSV(t *testing.T, st store.Store, query string) (*http.Response, []byte) {
	t.Helper()
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()
	res, err := http.Get(srv.URL + "/alerts.csv" + query)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return res, body
}

func TestExportAlertsCSV_HeadersAndFilename(t *testing.T) {
	res, body := exportCSV(t, &csvStore{total: 0}, "?from=2026-01-01T00:00:00Z&to=2026-02-01T00:00:00Z")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /alerts.csv = %d, want 200", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/csv") {
		t.Fatalf("Content-Type = %q, want text/csv", got)
	}
	if got := res.Header.Get("Content-Disposition"); got != `attachment; filename="alerts_20260101_20260201.csv"` {
		t.Fatalf("Content-Disposition = %q, want a filename carrying the date range", got)
	}
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	rows := readCSV(t, strings.NewReader(string(body)))
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want only the header row", len(rows))
	}
	if !reflect.DeepEqual(rows[0], alertCSVHeader) {
		t.Fatalf("header = %v, want %v", rows[0], alertCSVHeader)
	}
}

// TestExportAlertsCSV_EscapesAndGuardsInjection is the regression test for
// the spreadsheet-formula risk: every string column that begins with a
// formula character gets an apostrophe, while a value containing a comma or
// quote is still quoted and round-trips through a CSV reader.
func TestExportAlertsCSV_EscapesAndGuardsInjection(t *testing.T) {
	st := &csvStore{
		monitors: []store.Monitor{{ID: 1, Name: `-ops, "prod"`}},
		alerts: []store.Alert{{
			ID:        7,
			MonitorID: 1,
			RuleID:    3,
			EventID:   "@evt",
			Payload:   json.RawMessage(`{"contract_id":"=SUM(A1)","event_name":"+cmd","ledger":17}`),
			CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		}},
	}
	_, body := exportCSV(t, st, "")
	rows := readCSV(t, strings.NewReader(string(body)))
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want header + one alert: %q", len(rows), body)
	}
	want := []string{
		"7",
		`'-ops, "prod"`,
		"3",
		"warning",
		"'=SUM(A1)",
		"'+cmd",
		"'@evt",
		"17",
		"2026-01-02T03:04:05Z",
		`{"contract_id":"=SUM(A1)","event_name":"+cmd","ledger":17}`,
	}
	if !reflect.DeepEqual(rows[1], want) {
		t.Fatalf("row = %v\nwant %v", rows[1], want)
	}
}

// TestExportAlertsCSV_ReusesFilterParsing proves the export honors the same
// filters as the JSON listing by asserting they reach the store unchanged.
func TestExportAlertsCSV_ReusesFilterParsing(t *testing.T) {
	st := &csvStore{total: 0}
	exportCSV(t, st, "?monitor_id=3&rule_id=9&contract_id=CAAA&sort=created_at_asc&from=2026-01-01T00:00:00Z&to=2026-02-01T00:00:00Z")
	if st.got.MonitorID != 3 || st.got.RuleID != 9 || st.got.ContractID != "CAAA" || st.got.Sort != "created_at_asc" {
		t.Fatalf("filter = %+v", st.got)
	}
	if !st.got.From.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("From = %v", st.got.From)
	}
	if !st.got.To.Equal(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("To = %v", st.got.To)
	}
}

func TestExportAlertsCSV_InvalidFilterIsJSON400(t *testing.T) {
	res, body := exportCSV(t, &csvStore{total: 0}, "?sort=id")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET /alerts.csv?sort=id = %d, want 400", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("error body is not JSON: %v (%s)", err, body)
	}
	if env["error"] == nil || env["code"] == nil {
		t.Fatalf("400 envelope missing error/code: %v", env)
	}
}

func TestExportAlertsCSV_HonoursLimitAndCapsUnbounded(t *testing.T) {
	t.Run("explicit limit", func(t *testing.T) {
		_, body := exportCSV(t, &csvStore{total: 100}, "?limit=3")
		rows := readCSV(t, strings.NewReader(string(body)))
		if len(rows) != 4 { // header + 3 rows
			t.Fatalf("rows = %d, want header + 3", len(rows))
		}
	})

	t.Run("unbounded export is capped", func(t *testing.T) {
		st := &csvStore{total: maxAlertExportRows + 5000}
		_, body := exportCSV(t, st, "")
		rows := readCSV(t, strings.NewReader(string(body)))
		if len(rows) != maxAlertExportRows+1 { // header + capped rows
			t.Fatalf("rows = %d, want header + %d", len(rows), maxAlertExportRows)
		}
		if st.calls != (maxAlertExportRows+alertExportPageSize-1)/alertExportPageSize {
			t.Fatalf("store calls = %d, want the cap reached via full pages", st.calls)
		}
	})
}
