package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/reqid"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// ackCall is one acknowledge POST the dashboard forwarded to the store.
type ackCall struct {
	id    int64
	actor string
}

// ackStore is the alert-detail store plus a mutable acknowledgement: every
// acknowledge POST is recorded with the actor it carried, so a test can assert
// what the dashboard actually sent rather than only what it rendered
// afterwards. It honours the store contract — an unknown id is ErrNotFound,
// a repeat keeps the first actor — so handler regressions surface here
// rather than only against Postgres.
type ackStore struct {
	alertDetailStore
	acks    []ackCall
	entries []*store.AuditEntry
}

func (s *ackStore) GetAlert(_ context.Context, id int64) (*store.Alert, error) {
	if id != s.alert.ID {
		return nil, store.ErrNotFound
	}
	return &s.alert, nil
}

// ListAlerts returns the one alert, honouring the acknowledged filter the
// same way the store does, so the list's per-row action and pill can be
// checked before and after a press.
func (s *ackStore) ListAlerts(_ context.Context, f store.AlertFilter) ([]store.Alert, error) {
	ackd := s.alert.AcknowledgedAt != nil
	switch f.Acknowledged {
	case "acknowledged":
		if !ackd {
			return nil, nil
		}
	case "unacknowledged":
		if ackd {
			return nil, nil
		}
	}
	return []store.Alert{s.alert}, nil
}

func (s *ackStore) AcknowledgeAlert(_ context.Context, id int64, actor string) error {
	if id != s.alert.ID {
		return store.ErrNotFound
	}
	s.acks = append(s.acks, ackCall{id: id, actor: actor})
	if s.alert.AcknowledgedAt == nil {
		now := time.Date(2026, 10, 24, 9, 30, 0, 0, time.UTC)
		s.alert.AcknowledgedAt = &now
	}
	if s.alert.AcknowledgedBy == "" {
		s.alert.AcknowledgedBy = actor
	}
	return nil
}

func (s *ackStore) CreateAuditEntry(_ context.Context, e *store.AuditEntry) error {
	s.entries = append(s.entries, e)
	return nil
}

// newAckServer serves the dashboard over the given store with redirects left
// in place, which is what a browser following the acknowledge form does. The
// router is wrapped in reqid.Middleware exactly as cmd/sorobeacon mounts it,
// because the acknowledge actor and the audit row's actor are only the same
// request id when a request id exists at all.
func newAckServer(t *testing.T, st store.Store) (*httptest.Server, *http.Client) {
	t.Helper()
	srv, err := New(st, rules.NewRegistry(), notify.DefaultFactory(), testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(reqid.Middleware(srv.Routes()))
	t.Cleanup(ts.Close)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	return ts, client
}

// postAck presses the acknowledge form the way the page does: a POST from the
// alert's own page, so the Referer decides where the redirect lands.
func postAck(t *testing.T, client *http.Client, base, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/alerts/42/acknowledge", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Referer", base+path)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

// TestDashboardAcknowledgeGoesThroughTheStorePins the whole action the issue
// asks for: one store call carrying the actor, one audit row for the
// transition, and a redirect back to the page that was pressed — with the
// acknowledged state readable afterwards.
func TestDashboardAcknowledgeGoesThroughTheStore(t *testing.T) {
	st := &ackStore{alertDetailStore: sampleAlertDetail()}
	ts, client := newAckServer(t, st)

	res := postAck(t, client, ts.URL, "/alerts/42")
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("acknowledge = %d, want 303", res.StatusCode)
	}
	if loc := res.Header.Get("Location"); loc != "/alerts/42" {
		t.Fatalf("redirect = %q, want the alert page", loc)
	}
	if len(st.acks) != 1 {
		t.Fatalf("store saw %d acknowledge calls, want 1", len(st.acks))
	}
	if st.acks[0].id != 42 {
		t.Errorf("acknowledged id = %d, want 42", st.acks[0].id)
	}
	if st.acks[0].actor == "" {
		t.Error("acknowledge carried no actor (request id)")
	}
	if st.alert.AcknowledgedAt == nil || st.alert.AcknowledgedBy != st.acks[0].actor {
		t.Fatalf("alert not stamped by the caller: %+v", st.alert)
	}
	if len(st.entries) != 1 {
		t.Fatalf("recorded %d audit entries, want exactly 1", len(st.entries))
	}
	e := st.entries[0]
	if e.Action != store.AuditActionUpdate || e.TargetType != "alert" || e.TargetID != 42 {
		t.Errorf("audit entry = %+v, want update of alert 42", e)
	}
	if e.Actor != st.acks[0].actor {
		t.Errorf("audit actor = %q, want the acknowledge actor %q", e.Actor, st.acks[0].actor)
	}
}

// TestDashboardAcknowledgeIsIdempotentAndSaysSo covers the second press from a
// stale page: the store is called again (it is the escalation-stopping path),
// but the operator is told nothing changed and the log records the transition
// once — not once per press.
func TestDashboardAcknowledgeIsIdempotentAndSaysSo(t *testing.T) {
	st := &ackStore{alertDetailStore: sampleAlertDetail()}
	ts, client := newAckServer(t, st)

	first := postAck(t, client, ts.URL, "/alerts/42")
	first.Body.Close()
	second := postAck(t, client, ts.URL, "/alerts/42")
	if second.StatusCode != http.StatusSeeOther {
		t.Fatalf("second acknowledge = %d, want 303", second.StatusCode)
	}
	if loc := second.Header.Get("Location"); !strings.Contains(loc, "ack=already") {
		t.Fatalf("repeat redirect = %q, want ack=already so the page can say so", loc)
	}
	if len(st.acks) != 2 {
		t.Fatalf("store saw %d calls, want both presses forwarded", len(st.acks))
	}
	if st.alert.AcknowledgedBy != st.acks[0].actor {
		t.Fatalf("the first actor was overwritten: %q then %q", st.acks[0].actor, st.alert.AcknowledgedBy)
	}
	if len(st.entries) != 1 {
		t.Fatalf("recorded %d audit entries, want the transition logged once", len(st.entries))
	}

	// The page shows the result: who and when, plus the sentence about the
	// repeat press once the redirect's flag arrives.
	res, err := client.Get(ts.URL + "/alerts/42?ack=already")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	html := string(body)
	if !strings.Contains(html, "acknowledged") || !strings.Contains(html, st.acks[0].actor) {
		t.Fatalf("alert page does not show who acknowledged it, got:\n%s", html)
	}
	if !strings.Contains(html, "already acknowledged") {
		t.Fatalf("alert page does not explain the repeated press, got:\n%s", html)
	}
	if strings.Contains(html, `action="/alerts/42/acknowledge"`) {
		t.Fatal("an acknowledged alert still offers the acknowledge button")
	}
}

// TestDashboardAcknowledgeUnknownAlertIs404 keeps a mistyped or stale id from
// being answered with a redirect that looks like success.
func TestDashboardAcknowledgeUnknownAlertIs404(t *testing.T) {
	st := &ackStore{alertDetailStore: sampleAlertDetail()}
	ts, client := newAckServer(t, st)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/alerts/99/acknowledge", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("acknowledge unknown alert = %d, want 404", res.StatusCode)
	}
	if len(st.acks) != 0 {
		t.Fatalf("store was called for an alert that does not exist: %+v", st.acks)
	}
}

// TestAlertDetailOffersAcknowledgeOnlyWhenItIsNeeded checks the page's half:
// an untouched alert carries the form, and the list offers the same action
// per row so escalation can be stopped without leaving the list.
func TestAlertDetailOffersAcknowledgeOnlyWhenItIsNeeded(t *testing.T) {
	st := &ackStore{alertDetailStore: sampleAlertDetail()}
	ts, client := newAckServer(t, st)

	res, err := client.Get(ts.URL + "/alerts/42")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(body), `action="/alerts/42/acknowledge"`) {
		t.Fatalf("unacknowledged alert has no acknowledge form, got:\n%s", body)
	}

	list, err := client.Get(ts.URL + "/alerts")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(list.Body)
	list.Body.Close()
	if !strings.Contains(string(body), `action="/alerts/42/acknowledge"`) {
		t.Fatalf("alerts list has no per-row acknowledge action, got:\n%s", body)
	}
}

// TestAlertsPageSendsTheAcknowledgedFilter pins the filter the issue asks
// for: the value has to reach the store, and a value the page does not know
// is dropped rather than sent — the same rule the severity control follows.
func TestAlertsPageSendsTheAcknowledgedFilter(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  string
	}{
		{"acknowledged", "acknowledged", "acknowledged"},
		{"unacknowledged", "unacknowledged", "unacknowledged"},
		{"unknown value dropped", "bogus", ""},
		{"absent", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &captureAlertStore{}
			getHTML(t, st, "/alerts?acknowledged="+tc.query)
			if got := st.onlyFilter(t).Acknowledged; got != tc.want {
				t.Fatalf("Acknowledged filter = %q, want %q", got, tc.want)
			}
		})
	}
}
