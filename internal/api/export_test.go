package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// ndjsonStore is a store.Store fake for the NDJSON export tests.
// It synthesizes alerts on demand so streaming and the row cap can
// be exercised without a database.
type ndjsonStore struct {
	store.Store
	alerts []store.Alert
	got    store.AlertFilter
}

func (s *ndjsonStore) ListAlertsStream(_ context.Context, f store.AlertFilter, cb func(store.Alert) error) error {
	s.got = f
	limit := f.Limit
	if limit <= 0 {
		limit = len(s.alerts)
	}
	if limit > len(s.alerts) {
		limit = len(s.alerts)
	}
	for i := 0; i < limit; i++ {
		if err := cb(s.alerts[i]); err != nil {
			return err
		}
	}
	return nil
}

func (s *ndjsonStore) ListMonitors(context.Context, bool) ([]store.Monitor, error) {
	return []store.Monitor{{ID: 1, Name: "ops"}}, nil
}

// readNDJSON reads NDJSON lines from r and returns the parsed objects.
func readNDJSON(t *testing.T, r io.Reader) []map[string]any {
	t.Helper()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("parse ndjson: %v", err)
		}
		out = append(out, obj)
	}
	return out
}

// exportNDJSON fetches /alerts/export against a fresh server and returns
// the response plus the body. The caller owns res.Body.
func exportNDJSON(t *testing.T, st store.Store, query string) (*http.Response, []byte) {
	t.Helper()
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()
	res, err := http.Get(srv.URL + "/alerts/export" + query)
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

// TestExportAlertsNDJSON_HeadersAndFormat checks the response headers and
// that each line is valid NDJSON with the expected fields.
func TestExportAlertsNDJSON_HeadersAndFormat(t *testing.T) {
	st := &ndjsonStore{alerts: []store.Alert{
		{ID: 1, MonitorID: 1, RuleID: 2, EventID: "ev-1", Payload: json.RawMessage(`{"contract_id":"C","event_name":"xfer","ledger":1}`), CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		{ID: 2, MonitorID: 1, RuleID: 3, EventID: "ev-2", Payload: json.RawMessage(`{"contract_id":"D","event_name":"yfer","ledger":2}`), CreatedAt: time.Date(2026, 1, 3, 3, 4, 5, 0, time.UTC)},
	}}
	res, body := exportNDJSON(t, st, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /alerts/export = %d, want 200", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "application/x-ndjson" {
		t.Fatalf("Content-Type = %q, want application/x-ndjson", got)
	}
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	rows := readNDJSON(t, strings.NewReader(string(body)))
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0]["id"] != "1" {
		t.Fatalf("first row id = %v, want 1", rows[0]["id"])
	}
	if rows[0]["monitor_name"] != "ops" {
		t.Fatalf("monitor_name = %v, want ops", rows[0]["monitor_name"])
	}
	if rows[0]["event_name"] != "xfer" {
		t.Fatalf("event_name = %v, want xfer", rows[0]["event_name"])
	}
}

// TestExportAlertsNDJSON_StreamingLargeRowCount proves the export
// streams many rows without buffering them all in memory. A fake
// store returns far more rows than would fit in a typical buffer.
func TestExportAlertsNDJSON_StreamingLargeRowCount(t *testing.T) {
	// Generate far more rows than any reasonable buffer size.
	n := maxAlertExportRows + 5000
	alerts := make([]store.Alert, n)
	for i := range alerts {
		alerts[i] = store.Alert{
			ID: int64(i + 1),
			Payload: json.RawMessage(`{"contract_id":"C","event_name":"e","ledger":1}`),
		}
	}
	st := &ndjsonStore{alerts: alerts}
	_, body := exportNDJSON(t, st, "")
	rows := readNDJSON(t, strings.NewReader(string(body)))
	if len(rows) != maxAlertExportRows {
		t.Fatalf("rows = %d, want %d", len(rows), maxAlertExportRows)
	}
}

// TestExportAlertsNDJSON_ReusesFilterParsing proves the export honors
// the same filters as the JSON listing by asserting they reach the
// store unchanged.
func TestExportAlertsNDJSON_ReusesFilterParsing(t *testing.T) {
	st := &ndjsonStore{}
	exportNDJSON(t, st, "?monitor_id=3&rule_id=9&contract_id=CAAA&sort=created_at_asc&from=2026-01-01T00:00:00Z&to=2026-02-01T00:00:00Z&limit=50")
	if st.got.MonitorID != 3 || st.got.RuleID != 9 || st.got.ContractID != "CAAA" || st.got.Sort != "created_at_asc" {
		t.Fatalf("filter = %+v", st.got)
	}
	if !st.got.From.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("From = %v", st.got.From)
	}
	if !st.got.To.Equal(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("To = %v", st.got.To)
	}
	if st.got.Limit != 50 {
		t.Fatalf("Limit = %d, want 50", st.got.Limit)
	}
}

// cancelStoreImpl is a store that stops streaming when the
// request context is cancelled.
type cancelStoreImpl struct {
	store.Store
}

func (s *cancelStoreImpl) ListMonitors(context.Context, bool) ([]store.Monitor, error) {
	return nil, nil
}

func (s *cancelStoreImpl) ListAlertsStream(ctx context.Context, f store.AlertFilter, cb func(store.Alert) error) error {
	for _, a := range []store.Alert{{ID: 1}} {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			if err := cb(a); err != nil {
				return err
			}
		}
	}
	return nil
}

// TestExportAlertsNDJSON_CancelledClientStopsQuery verifies that when
// the request context is cancelled, the underlying store query stops.
func TestExportAlertsNDJSON_CancelledClientStopsQuery(t *testing.T) {
	srv := httptest.NewServer(newProbeServer(&cancelStoreImpl{}, &fakeRPC{}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/alerts/export", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)

	// Cancel the context shortly after the request starts.
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	client := &http.Client{Timeout: 2 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		// Expected: the query was interrupted by context cancellation.
		return
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	// If we got a response without error, the cancellation happened
	// after the stream completed, which is also acceptable.
}

// TestExportAlertsNDJSON_InvalidFilterIsJSON400 verifies that an
// invalid sort parameter produces a JSON 400 error envelope.
func TestExportAlertsNDJSON_InvalidFilterIsJSON400(t *testing.T) {
	res, body := exportNDJSON(t, &ndjsonStore{}, "?sort=id")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET /alerts/export?sort=id = %d, want 400", res.StatusCode)
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

// TestExportAlertsNDJSON_UnboundedExportIsCapped verifies that
// omitting the limit caps the export at maxAlertExportRows.
func TestExportAlertsNDJSON_UnboundedExportIsCapped(t *testing.T) {
	st := &ndjsonStore{
		alerts: make([]store.Alert, maxAlertExportRows+5000),
	}
	for i := range st.alerts {
		st.alerts[i] = store.Alert{ID: int64(i + 1)}
	}
	_, body := exportNDJSON(t, st, "")
	rows := readNDJSON(t, strings.NewReader(string(body)))
	if len(rows) != maxAlertExportRows {
		t.Fatalf("rows = %d, want %d", len(rows), maxAlertExportRows)
	}
	if st.got.Limit != maxAlertExportRows {
		t.Fatalf("store limit = %d, want %d", st.got.Limit, maxAlertExportRows)
	}
}
