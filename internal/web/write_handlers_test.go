package web

// Tests for the dashboard's write handlers — the paths a person clicks in
// the dashboard, each of which writes to the store and most of which write an
// audit row. Every test drives the handler through the dashboard router with
// a signed-in session, then asserts the store state, the redirect or fragment
// the browser would follow, and the audit row where one is written.
//
// The harness is a recording stub store (writeStore) rather than the empty
// one the page-rendering tests use: these tests exist to pin what the writes
// do, so the stub remembers monitors, rules, channels, windows, searches,
// attempts, dead letters and audit entries, and can inject a failure into any
// individual store call to cover the error paths.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// Obviously fake contract IDs of the right shape: valid Stellar strkeys
// (correct version byte and checksum, so the importer's validation accepts
// them) built from all-zero and all-ones payloads so they are recognisably
// placeholders rather than a real network address.
const (
	fakeContractA = "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABSC4"
	fakeContractB = "CD7777777777777777777777777777777777777777777777777767GY"
)

// writeStore is the recording stub behind the write-handler tests. It embeds
// emptyStore for everything the handlers under test never call; a handler
// that reaches a method it does not implement panics, which is the signal to
// implement it rather than to quietly return nothing.
type writeStore struct {
	emptyStore

	monitors    map[int64]store.Monitor
	rules       map[int64]store.Rule
	channels    map[int64]store.Channel
	windows     map[int64]store.MaintenanceWindow
	searches    map[int64]store.SavedSearch
	alerts      map[int64]store.Alert
	attempts    []store.DeliveryAttempt
	deadLetters map[int64]store.DeadLetter
	audit       []store.AuditEntry

	// lastSetEnabled remembers the ids the last bulk enable/disable asked
	// for, so a test can pin that unknown ids ride along instead of failing
	// the request.
	lastSetEnabled []int64

	// errAt injects a failure into the store method of that name; nil
	// (the default) means the method behaves normally.
	errAt map[string]error

	nextID int64
}

func newWriteStore() *writeStore {
	return &writeStore{
		monitors:    map[int64]store.Monitor{},
		rules:       map[int64]store.Rule{},
		channels:    map[int64]store.Channel{},
		windows:     map[int64]store.MaintenanceWindow{},
		searches:    map[int64]store.SavedSearch{},
		alerts:      map[int64]store.Alert{},
		deadLetters: map[int64]store.DeadLetter{},
		errAt:       map[string]error{},
	}
}

func (s *writeStore) boom(method string) error { return s.errAt[method] }

func (s *writeStore) GetMonitor(_ context.Context, id int64) (*store.Monitor, error) {
	if err := s.boom("GetMonitor"); err != nil {
		return nil, err
	}
	m, ok := s.monitors[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &m, nil
}

func (s *writeStore) UpdateMonitor(_ context.Context, m *store.Monitor) error {
	if err := s.boom("UpdateMonitor"); err != nil {
		return err
	}
	if _, ok := s.monitors[m.ID]; !ok {
		return store.ErrNotFound
	}
	s.monitors[m.ID] = *m
	return nil
}

func (s *writeStore) SoftDeleteMonitor(_ context.Context, id int64) error {
	if err := s.boom("SoftDeleteMonitor"); err != nil {
		return err
	}
	m, ok := s.monitors[id]
	if !ok {
		return store.ErrNotFound
	}
	now := time.Now()
	m.DeletedAt = &now
	s.monitors[id] = m
	return nil
}

func (s *writeStore) SetMonitorsEnabled(_ context.Context, ids []int64, enabled bool) (int, []int64, error) {
	if err := s.boom("SetMonitorsEnabled"); err != nil {
		return 0, nil, err
	}
	s.lastSetEnabled = append([]int64(nil), ids...)
	updated := 0
	var unknown []int64
	for _, id := range ids {
		m, ok := s.monitors[id]
		if !ok {
			unknown = append(unknown, id)
			continue
		}
		m.Enabled = enabled
		s.monitors[id] = m
		updated++
	}
	return updated, unknown, nil
}

func (s *writeStore) SetMonitorChannels(_ context.Context, monitorID int64, channelIDs []int64) error {
	if err := s.boom("SetMonitorChannels"); err != nil {
		return err
	}
	m, ok := s.monitors[monitorID]
	if !ok {
		return store.ErrNotFound
	}
	m.ChannelIDs = append([]int64(nil), channelIDs...)
	s.monitors[monitorID] = m
	return nil
}

func (s *writeStore) GetRule(_ context.Context, id int64) (*store.Rule, error) {
	if err := s.boom("GetRule"); err != nil {
		return nil, err
	}
	r, ok := s.rules[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &r, nil
}

func (s *writeStore) DeleteRule(_ context.Context, id int64) error {
	if err := s.boom("DeleteRule"); err != nil {
		return err
	}
	if _, ok := s.rules[id]; !ok {
		return store.ErrNotFound
	}
	delete(s.rules, id)
	return nil
}

func (s *writeStore) SetChannelsEnabled(_ context.Context, ids []int64, enabled bool) (int, []int64, error) {
	if err := s.boom("SetChannelsEnabled"); err != nil {
		return 0, nil, err
	}
	s.lastSetEnabled = append([]int64(nil), ids...)
	updated := 0
	var unknown []int64
	for _, id := range ids {
		c, ok := s.channels[id]
		if !ok {
			unknown = append(unknown, id)
			continue
		}
		c.Enabled = enabled
		s.channels[id] = c
		updated++
	}
	return updated, unknown, nil
}

func (s *writeStore) GetChannel(_ context.Context, id int64) (*store.Channel, error) {
	if err := s.boom("GetChannel"); err != nil {
		return nil, err
	}
	c, ok := s.channels[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &c, nil
}

func (s *writeStore) ListChannels(_ context.Context, enabledOnly bool) ([]store.Channel, error) {
	if err := s.boom("ListChannels"); err != nil {
		return nil, err
	}
	out := make([]store.Channel, 0, len(s.channels))
	for i := int64(1); i <= int64(len(s.channels)); i++ {
		if c, ok := s.channels[i]; ok {
			if enabledOnly && !c.Enabled {
				continue
			}
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *writeStore) CreateChannel(_ context.Context, c *store.Channel) error {
	if err := s.boom("CreateChannel"); err != nil {
		return err
	}
	// New ids come from the map itself so a manually seeded fixture can
	// never be overwritten by an auto-assigned id.
	c.ID = 1
	for id := range s.channels {
		if id >= c.ID {
			c.ID = id + 1
		}
	}
	c.CreatedAt = time.Now()
	s.channels[c.ID] = *c
	return nil
}

func (s *writeStore) DeleteMaintenanceWindow(_ context.Context, id int64) error {
	if err := s.boom("DeleteMaintenanceWindow"); err != nil {
		return err
	}
	if _, ok := s.windows[id]; !ok {
		return store.ErrNotFound
	}
	delete(s.windows, id)
	return nil
}

func (s *writeStore) GetAlert(_ context.Context, id int64) (*store.Alert, error) {
	if err := s.boom("GetAlert"); err != nil {
		return nil, err
	}
	a, ok := s.alerts[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &a, nil
}

func (s *writeStore) ListDeliveryAttempts(_ context.Context, alertID int64, status string) ([]store.DeliveryAttempt, error) {
	if err := s.boom("ListDeliveryAttempts"); err != nil {
		return nil, err
	}
	out := []store.DeliveryAttempt{}
	for _, a := range s.attempts {
		if a.AlertID != alertID {
			continue
		}
		if status != "" && a.Status != status {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

func (s *writeStore) RecordDeliveryAttempt(_ context.Context, d *store.DeliveryAttempt) error {
	if err := s.boom("RecordDeliveryAttempt"); err != nil {
		return err
	}
	s.nextID++
	d.ID = s.nextID
	if d.AttemptedAt.IsZero() {
		d.AttemptedAt = time.Now()
	}
	s.attempts = append(s.attempts, *d)
	return nil
}

func (s *writeStore) RecordChannelHealth(context.Context, int64, store.ChannelHealthUpdate) error {
	return s.boom("RecordChannelHealth")
}

func (s *writeStore) ListDeadLetters(_ context.Context, f store.DeadLetterFilter) ([]store.DeadLetter, error) {
	if err := s.boom("ListDeadLetters"); err != nil {
		return nil, err
	}
	out := []store.DeadLetter{}
	for i := int64(1); i <= int64(len(s.deadLetters)); i++ {
		dl, ok := s.deadLetters[i]
		if !ok {
			continue
		}
		if f.ChannelID != 0 && dl.ChannelID != f.ChannelID {
			continue
		}
		if f.AlertID != 0 && dl.AlertID != f.AlertID {
			continue
		}
		out = append(out, dl)
	}
	return out, nil
}

func (s *writeStore) GetDeadLetter(_ context.Context, id int64) (*store.DeadLetter, error) {
	if err := s.boom("GetDeadLetter"); err != nil {
		return nil, err
	}
	dl, ok := s.deadLetters[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &dl, nil
}

func (s *writeStore) DeleteDeadLetter(_ context.Context, id int64) error {
	if err := s.boom("DeleteDeadLetter"); err != nil {
		return err
	}
	delete(s.deadLetters, id)
	return nil
}

func (s *writeStore) CreateMonitor(_ context.Context, m *store.Monitor) error {
	if err := s.boom("CreateMonitor"); err != nil {
		return err
	}
	m.ID = 1
	for id := range s.monitors {
		if id >= m.ID {
			m.ID = id + 1
		}
	}
	m.CreatedAt = time.Now()
	s.monitors[m.ID] = *m
	return nil
}

func (s *writeStore) ListMonitors(_ context.Context, _ bool) ([]store.Monitor, error) {
	if err := s.boom("ListMonitors"); err != nil {
		return nil, err
	}
	out := make([]store.Monitor, 0, len(s.monitors))
	for i := int64(1); i <= int64(len(s.monitors)); i++ {
		if m, ok := s.monitors[i]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *writeStore) SetDefaultSearch(_ context.Context, id int64) error {
	if err := s.boom("SetDefaultSearch"); err != nil {
		return err
	}
	if _, ok := s.searches[id]; !ok {
		return store.ErrNotFound
	}
	for i, ss := range s.searches {
		ss.IsDefault = i == id
		s.searches[i] = ss
	}
	return nil
}

func (s *writeStore) ClearDefaultSearch(_ context.Context, id int64) error {
	if err := s.boom("ClearDefaultSearch"); err != nil {
		return err
	}
	ss, ok := s.searches[id]
	if !ok {
		return store.ErrNotFound
	}
	ss.IsDefault = false
	s.searches[id] = ss
	return nil
}

func (s *writeStore) CreateAuditEntry(_ context.Context, e *store.AuditEntry) error {
	if err := s.boom("CreateAuditEntry"); err != nil {
		return err
	}
	s.audit = append(s.audit, *e)
	return nil
}

// recordingNotifier counts the sends it was asked to make and returns the
// scripted outcome, so a delivery test can assert that a retry or a redrive
// really called the notifier instead of only touching the store.
type recordingNotifier struct {
	mu     sync.Mutex
	sends  int
	err    error
	last   notify.Alert
}

func (n *recordingNotifier) Send(_ context.Context, a notify.Alert) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sends++
	n.last = a
	return n.err
}

func (n *recordingNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.sends
}

// newWriteDashboard builds a dashboard over writeStore with authentication
// configured, and signs in through the router so every request below carries
// a real session cookie — the same credential a browser would hold.
func newWriteDashboard(t *testing.T, notifier notify.Notifier) (*writeStore, http.Handler, *http.Cookie) {
	t.Helper()
	st := newWriteStore()
	f := notify.DefaultFactory()
	f.Register("mock", func(json.RawMessage) (notify.Notifier, error) { return notifier, nil })
	s, err := New(st, rules.NewRegistry(), f, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.WithAuth(auth.New([]string{testToken}, 0))
	h := s.Routes()
	c := sessionCookieOf(t, signIn(t, h, testToken, "/"))
	if c == nil {
		t.Fatal("sign-in set no session cookie")
	}
	return st, h, c
}

// postForm posts an application/x-www-form-urlencoded body through the
// dashboard router with the session attached, exactly as a form submit does.
func postWithSession(t *testing.T, h http.Handler, sess *http.Cookie, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sess)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// postMultipartUpload posts a file upload (the import form's encoding).
func postMultipartUpload(t *testing.T, h http.Handler, sess *http.Cookie, path, field, filename string, content []byte, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile(field, filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatalf("write file: %v", err)
	}
	for k, vs := range form {
		for _, v := range vs {
			if err := w.WriteField(k, v); err != nil {
				t.Fatalf("WriteField: %v", err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.AddCookie(sess)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func wantRedirect(t *testing.T, rec *httptest.ResponseRecorder, location string) {
	t.Helper()
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != location {
		t.Fatalf("Location = %q, want %q", got, location)
	}
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, want, rec.Body.String())
	}
}

// auditFor returns the audit entries for one target, so a test can assert
// both that a handler wrote its row and that it wrote exactly one.
func auditFor(entries []store.AuditEntry, targetType string, targetID int64) []store.AuditEntry {
	var out []store.AuditEntry
	for _, e := range entries {
		if e.TargetType == targetType && e.TargetID == targetID {
			out = append(out, e)
		}
	}
	return out
}

// --- monitors ---

// Bulk enable/disable is partial by design: unknown ids must not stop the
// known ones, and the request still redirects.
func TestBulkMonitorsAppliesKnownIDsAndRedirects(t *testing.T) {
	st, h, sess := newWriteDashboard(t, scriptedNotifier{})
	st.monitors[1] = store.Monitor{ID: 1, Name: "one", Enabled: true}
	st.monitors[2] = store.Monitor{ID: 2, Name: "two", Enabled: true}

	rec := postWithSession(t, h, sess, "/monitors/bulk", url.Values{
		"ids":     {"1", "99"},
		"enabled": {"false"},
	})
	wantRedirect(t, rec, "/monitors")

	if st.monitors[1].Enabled {
		t.Error("monitor 1 was not disabled")
	}
	if !st.monitors[2].Enabled {
		t.Error("monitor 2 must be untouched")
	}
	if len(st.lastSetEnabled) != 2 || st.lastSetEnabled[1] != 99 {
		t.Errorf("store received ids %v, want the unknown id passed through", st.lastSetEnabled)
	}
	if len(st.audit) != 0 {
		t.Errorf("bulk enable/disable writes no audit row, got %d", len(st.audit))
	}
}

func TestBulkMonitorsRejectsBadForms(t *testing.T) {
	st, h, sess := newWriteDashboard(t, scriptedNotifier{})
	cases := []struct {
		name string
		form url.Values
		want int
	}{
		{"missing enabled", url.Values{"ids": {"1"}}, http.StatusBadRequest},
		{"enabled not a bool", url.Values{"ids": {"1"}, "enabled": {"maybe"}}, http.StatusBadRequest},
		{"missing ids", url.Values{"enabled": {"true"}}, http.StatusBadRequest},
		{"non-numeric id", url.Values{"ids": {"abc"}, "enabled": {"true"}}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postWithSession(t, h, sess, "/monitors/bulk", tc.form)
			wantStatus(t, rec, tc.want)
		})
	}

	t.Run("store error", func(t *testing.T) {
		st.errAt["SetMonitorsEnabled"] = errors.New("db down")
		defer delete(st.errAt, "SetMonitorsEnabled")
		rec := postWithSession(t, h, sess, "/monitors/bulk", url.Values{"ids": {"1"}, "enabled": {"true"}})
		wantStatus(t, rec, http.StatusInternalServerError)
		if !strings.Contains(rec.Body.String(), "Something went wrong") {
			t.Errorf("store error must render the error page, got: %s", rec.Body.String())
		}
	})
}

func TestToggleMonitorFlipsEnabledAndAudits(t *testing.T) {
	st, h, sess := newWriteDashboard(t, scriptedNotifier{})
	st.monitors[1] = store.Monitor{ID: 1, Name: "one", Enabled: false}

	rec := postWithSession(t, h, sess, "/monitors/1/toggle", nil)
	wantRedirect(t, rec, "/monitors")
	if !st.monitors[1].Enabled {
		t.Fatal("toggle did not enable the monitor")
	}
	rows := auditFor(st.audit, "monitor", 1)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want exactly 1", len(rows))
	}
	if rows[0].Action != store.AuditActionUpdate || !strings.Contains(string(rows[0].Diff), "enabled") {
		t.Errorf("audit row = %+v, want update naming the enabled field", rows[0])
	}

	// Toggling again flips it back and appends one more row.
	rec = postWithSession(t, h, sess, "/monitors/1/toggle", nil)
	wantRedirect(t, rec, "/monitors")
	if st.monitors[1].Enabled {
		t.Error("second toggle did not disable the monitor")
	}
	if len(st.audit) != 2 {
		t.Errorf("audit rows = %d, want 2", len(st.audit))
	}
}

func TestToggleMonitorErrorPaths(t *testing.T) {
	st, h, sess := newWriteDashboard(t, scriptedNotifier{})

	t.Run("malformed id", func(t *testing.T) {
		wantStatus(t, postWithSession(t, h, sess, "/monitors/abc/toggle", nil), http.StatusNotFound)
	})
	t.Run("unknown id", func(t *testing.T) {
		wantStatus(t, postWithSession(t, h, sess, "/monitors/42/toggle", nil), http.StatusNotFound)
	})
	t.Run("store error", func(t *testing.T) {
		st.monitors[1] = store.Monitor{ID: 1, Name: "one"}
		st.errAt["UpdateMonitor"] = errors.New("db down")
		defer delete(st.errAt, "UpdateMonitor")
		wantStatus(t, postWithSession(t, h, sess, "/monitors/1/toggle", nil), http.StatusInternalServerError)
	})
	t.Run("audit store error does not block the request", func(t *testing.T) {
		st.errAt["CreateAuditEntry"] = errors.New("db down")
		defer delete(st.errAt, "CreateAuditEntry")
		rec := postWithSession(t, h, sess, "/monitors/1/toggle", nil)
		wantRedirect(t, rec, "/monitors")
	})
}

func TestDeleteMonitorSoftDeletesAndAudits(t *testing.T) {
	st, h, sess := newWriteDashboard(t, scriptedNotifier{})
	st.monitors[1] = store.Monitor{ID: 1, Name: "one"}

	rec := postWithSession(t, h, sess, "/monitors/1/delete", nil)
	wantRedirect(t, rec, "/monitors")
	if st.monitors[1].DeletedAt == nil {
		t.Fatal("delete did not soft-delete the monitor")
	}
	rows := auditFor(st.audit, "monitor", 1)
	if len(rows) != 1 || rows[0].Action != store.AuditActionDelete {
		t.Fatalf("audit rows = %+v, want one delete row", rows)
	}

	t.Run("unknown id is a server error page", func(t *testing.T) {
		// The store answers ErrNotFound and the handler renders the
		// generic failure page; the point pinned here is that it does
		// not redirect as if the delete happened.
		rec := postWithSession(t, h, sess, "/monitors/77/delete", nil)
		wantStatus(t, rec, http.StatusInternalServerError)
	})
	t.Run("malformed id", func(t *testing.T) {
		wantStatus(t, postWithSession(t, h, sess, "/monitors/abc/delete", nil), http.StatusNotFound)
	})
	t.Run("store error", func(t *testing.T) {
		st.errAt["SoftDeleteMonitor"] = errors.New("db down")
		defer delete(st.errAt, "SoftDeleteMonitor")
		wantStatus(t, postWithSession(t, h, sess, "/monitors/1/delete", nil), http.StatusInternalServerError)
	})
}

func TestSetMonitorChannelsReplacesAttachmentsAndAudits(t *testing.T) {
	st, h, sess := newWriteDashboard(t, scriptedNotifier{})
	st.monitors[1] = store.Monitor{ID: 1, Name: "one", ChannelIDs: []int64{9}}

	rec := postWithSession(t, h, sess, "/monitors/1/channels", url.Values{"channel_ids": {"3", "5"}})
	wantRedirect(t, rec, "/monitors/1")
	got := st.monitors[1].ChannelIDs
	if len(got) != 2 || got[0] != 3 || got[1] != 5 {
		t.Fatalf("channel_ids = %v, want [3 5] (attachments are replaced, not appended)", got)
	}
	rows := auditFor(st.audit, "monitor", 1)
	if len(rows) != 1 || !strings.Contains(string(rows[0].Diff), "channel_ids") {
		t.Fatalf("audit rows = %+v, want one update naming channel_ids", rows)
	}

	t.Run("empty selection detaches every channel", func(t *testing.T) {
		rec := postWithSession(t, h, sess, "/monitors/1/channels", nil)
		wantRedirect(t, rec, "/monitors/1")
		if len(st.monitors[1].ChannelIDs) != 0 {
			t.Errorf("channel_ids = %v, want empty", st.monitors[1].ChannelIDs)
		}
	})
	t.Run("non-numeric channel id", func(t *testing.T) {
		rec := postWithSession(t, h, sess, "/monitors/1/channels", url.Values{"channel_ids": {"abc"}})
		wantStatus(t, rec, http.StatusBadRequest)
		if !strings.Contains(rec.Body.String(), "invalid channel id") {
			t.Errorf("body = %s, want the id named", rec.Body.String())
		}
	})
	t.Run("unknown monitor", func(t *testing.T) {
		wantStatus(t, postWithSession(t, h, sess, "/monitors/77/channels", url.Values{"channel_ids": {"1"}}), http.StatusInternalServerError)
	})
	t.Run("malformed monitor id", func(t *testing.T) {
		wantStatus(t, postWithSession(t, h, sess, "/monitors/abc/channels", url.Values{"channel_ids": {"1"}}), http.StatusNotFound)
	})
	t.Run("store error", func(t *testing.T) {
		st.errAt["SetMonitorChannels"] = errors.New("db down")
		defer delete(st.errAt, "SetMonitorChannels")
		wantStatus(t, postWithSession(t, h, sess, "/monitors/1/channels", url.Values{"channel_ids": {"1"}}), http.StatusInternalServerError)
	})
}

// --- rules ---

func TestDeleteRuleRemovesRuleAndAudits(t *testing.T) {
	st, h, sess := newWriteDashboard(t, scriptedNotifier{})
	st.monitors[1] = store.Monitor{ID: 1, Name: "one"}
	st.monitors[2] = store.Monitor{ID: 2, Name: "two"}
	st.rules[7] = store.Rule{ID: 7, MonitorID: 1, Type: "event_emitted"}
	st.rules[8] = store.Rule{ID: 8, MonitorID: 2, Type: "event_emitted"}

	rec := postWithSession(t, h, sess, "/monitors/1/rules/7/delete", nil)
	wantRedirect(t, rec, "/monitors/1")
	if _, ok := st.rules[7]; ok {
		t.Fatal("rule 7 was not deleted")
	}
	rows := auditFor(st.audit, "rule", 7)
	if len(rows) != 1 || rows[0].Action != store.AuditActionDelete {
		t.Fatalf("audit rows = %+v, want one delete row", rows)
	}

	t.Run("a rule of another monitor is not deletable through this path", func(t *testing.T) {
		wantStatus(t, postWithSession(t, h, sess, "/monitors/1/rules/8/delete", nil), http.StatusNotFound)
		if _, ok := st.rules[8]; !ok {
			t.Error("rule 8 must survive the rejected delete")
		}
	})
	t.Run("unknown rule", func(t *testing.T) {
		wantStatus(t, postWithSession(t, h, sess, "/monitors/1/rules/404/delete", nil), http.StatusNotFound)
	})
	t.Run("malformed ids", func(t *testing.T) {
		wantStatus(t, postWithSession(t, h, sess, "/monitors/abc/rules/7/delete", nil), http.StatusNotFound)
		wantStatus(t, postWithSession(t, h, sess, "/monitors/1/rules/abc/delete", nil), http.StatusNotFound)
	})
	t.Run("store error", func(t *testing.T) {
		st.rules[9] = store.Rule{ID: 9, MonitorID: 1}
		st.errAt["DeleteRule"] = errors.New("db down")
		defer delete(st.errAt, "DeleteRule")
		wantStatus(t, postWithSession(t, h, sess, "/monitors/1/rules/9/delete", nil), http.StatusInternalServerError)
	})
}

// --- channels ---

func TestBulkChannelsAppliesKnownIDsAndCapsRequestSize(t *testing.T) {
	st, h, sess := newWriteDashboard(t, scriptedNotifier{})
	st.channels[1] = store.Channel{ID: 1, Name: "one", Enabled: false}
	st.channels[2] = store.Channel{ID: 2, Name: "two", Enabled: false}

	rec := postWithSession(t, h, sess, "/channels/bulk", url.Values{
		"ids":     {"1", "99"},
		"enabled": {"true"},
	})
	wantRedirect(t, rec, "/channels")
	if !st.channels[1].Enabled {
		t.Error("channel 1 was not enabled")
	}
	if st.channels[2].Enabled {
		t.Error("channel 2 must be untouched")
	}
	if len(st.lastSetEnabled) != 2 || st.lastSetEnabled[1] != 99 {
		t.Errorf("store received ids %v, want the unknown id passed through", st.lastSetEnabled)
	}

	t.Run("more than 100 ids is rejected", func(t *testing.T) {
		ids := make([]string, 101)
		for i := range ids {
			ids[i] = "1"
		}
		rec := postWithSession(t, h, sess, "/channels/bulk", url.Values{"ids": ids, "enabled": {"true"}})
		wantStatus(t, rec, http.StatusBadRequest)
		if !strings.Contains(rec.Body.String(), "at most 100") {
			t.Errorf("body = %s, want the cap named", rec.Body.String())
		}
	})
	t.Run("missing enabled", func(t *testing.T) {
		wantStatus(t, postWithSession(t, h, sess, "/channels/bulk", url.Values{"ids": {"1"}}), http.StatusBadRequest)
	})
	t.Run("non-numeric id", func(t *testing.T) {
		wantStatus(t, postWithSession(t, h, sess, "/channels/bulk", url.Values{"ids": {"x"}, "enabled": {"true"}}), http.StatusBadRequest)
	})
	t.Run("store error", func(t *testing.T) {
		st.errAt["SetChannelsEnabled"] = errors.New("db down")
		defer delete(st.errAt, "SetChannelsEnabled")
		wantStatus(t, postWithSession(t, h, sess, "/channels/bulk", url.Values{"ids": {"1"}, "enabled": {"true"}}), http.StatusInternalServerError)
	})
}

func TestDuplicateChannelCreatesDisabledUnattachedCopy(t *testing.T) {
	st, h, sess := newWriteDashboard(t, scriptedNotifier{})
	st.channels[1] = store.Channel{
		ID: 1, Name: "ops", Type: "mock", Enabled: true,
		Config:              json.RawMessage(`{"url":"http://example.invalid/hook"}`),
		ConsecutiveFailures: 3, LastError: "boom",
	}

	rec := postWithSession(t, h, sess, "/channels/1/duplicate", nil)
	wantRedirect(t, rec, "/channels")
	if len(st.channels) != 2 {
		t.Fatalf("channels = %d, want 2", len(st.channels))
	}
	dup, ok := st.channels[2]
	if !ok {
		t.Fatalf("copy not stored under a new id: %+v", st.channels)
	}
	if dup.Name != "ops (copy)" {
		t.Errorf("name = %q, want %q", dup.Name, "ops (copy)")
	}
	if dup.Enabled {
		t.Error("a duplicate must be created disabled")
	}
	if !bytes.Equal(dup.Config, st.channels[1].Config) {
		t.Errorf("config = %s, want it carried over", dup.Config)
	}
	if dup.ConsecutiveFailures != 0 || dup.LastError != "" {
		t.Errorf("health state carried over: %+v", dup)
	}

	t.Run("unknown id", func(t *testing.T) {
		wantStatus(t, postWithSession(t, h, sess, "/channels/42/duplicate", nil), http.StatusNotFound)
	})
	t.Run("malformed id", func(t *testing.T) {
		wantStatus(t, postWithSession(t, h, sess, "/channels/abc/duplicate", nil), http.StatusNotFound)
	})
	t.Run("store error", func(t *testing.T) {
		st.errAt["CreateChannel"] = errors.New("db down")
		defer delete(st.errAt, "CreateChannel")
		wantStatus(t, postWithSession(t, h, sess, "/channels/1/duplicate", nil), http.StatusInternalServerError)
	})
}

// --- maintenance ---

func TestDeleteMaintenanceRemovesWindow(t *testing.T) {
	st, h, sess := newWriteDashboard(t, scriptedNotifier{})
	st.windows[1] = store.MaintenanceWindow{ID: 1, Reason: "upgrade", Scope: "global"}

	rec := postWithSession(t, h, sess, "/maintenance/1/delete", nil)
	wantRedirect(t, rec, "/maintenance")
	if _, ok := st.windows[1]; ok {
		t.Fatal("window was not deleted")
	}

	t.Run("malformed id", func(t *testing.T) {
		wantStatus(t, postWithSession(t, h, sess, "/maintenance/abc/delete", nil), http.StatusNotFound)
	})
	t.Run("unknown id", func(t *testing.T) {
		wantStatus(t, postWithSession(t, h, sess, "/maintenance/55/delete", nil), http.StatusInternalServerError)
	})
	t.Run("store error", func(t *testing.T) {
		st.windows[2] = store.MaintenanceWindow{ID: 2}
		st.errAt["DeleteMaintenanceWindow"] = errors.New("db down")
		defer delete(st.errAt, "DeleteMaintenanceWindow")
		wantStatus(t, postWithSession(t, h, sess, "/maintenance/2/delete", nil), http.StatusInternalServerError)
	})
}

// --- delivery retry ---

func retryFixture(t *testing.T, n notify.Notifier) (*writeStore, http.Handler, *http.Cookie) {
	t.Helper()
	st, h, sess := newWriteDashboard(t, n)
	st.alerts[1] = store.Alert{ID: 1, MonitorID: 1, RuleID: 1, EventID: "ev-1", Severity: store.SeverityWarning}
	st.channels[1] = store.Channel{ID: 1, Name: "ops", Type: "mock", Enabled: true}
	return st, h, sess
}

func TestRetryDeliveryRecordsNewAttemptAndCallsNotifier(t *testing.T) {
	n := &recordingNotifier{}
	st, h, sess := retryFixture(t, n)
	// A failed attempt, older than the retry cooldown, is what makes a
	// manual retry legal.
	st.attempts = append(st.attempts, store.DeliveryAttempt{
		ID: 1, AlertID: 1, ChannelID: 1, Status: "failed",
		AttemptedAt: time.Now().Add(-time.Minute),
	})

	rec := postWithSession(t, h, sess, "/alerts/1/deliveries/1/retry", nil)
	wantStatus(t, rec, http.StatusOK)
	body := rec.Body.String()
	if !strings.Contains(body, "<table") || !strings.Contains(body, "success") {
		t.Fatalf("fragment must render the attempt table with the new outcome, got: %s", body)
	}
	if len(st.attempts) != 2 {
		t.Fatalf("attempts = %d, want 2 (a new attempt is recorded, the old one untouched)", len(st.attempts))
	}
	if st.attempts[1].Status != "success" {
		t.Errorf("new attempt status = %q, want success", st.attempts[1].Status)
	}
	if n.count() != 1 {
		t.Errorf("notifier sends = %d, want 1", n.count())
	}
	if len(st.audit) != 0 {
		t.Errorf("retry writes no audit row, got %d", len(st.audit))
	}
}

func TestRetryDeliveryGateFailuresStayInTheFragment(t *testing.T) {
	cases := []struct {
		name    string
		seed    func(*writeStore)
		want    string
	}{
		{
			name: "no attempt for this channel",
			seed: func(st *writeStore) {},
			want: "no delivery attempt for this channel",
		},
		{
			name: "already succeeded",
			seed: func(st *writeStore) {
				st.attempts = append(st.attempts, store.DeliveryAttempt{
					AlertID: 1, ChannelID: 1, Status: "success",
					AttemptedAt: time.Now().Add(-time.Minute),
				})
			},
			want: "delivery already succeeded",
		},
		{
			name: "retry cooldown",
			seed: func(st *writeStore) {
				st.attempts = append(st.attempts, store.DeliveryAttempt{
					AlertID: 1, ChannelID: 1, Status: "failed",
					AttemptedAt: time.Now(),
				})
			},
			want: "retried too recently",
		},
		{
			name: "disabled channel",
			seed: func(st *writeStore) {
				st.channels[1] = store.Channel{ID: 1, Name: "ops", Type: "mock", Enabled: false}
				st.attempts = append(st.attempts, store.DeliveryAttempt{
					AlertID: 1, ChannelID: 1, Status: "failed",
					AttemptedAt: time.Now().Add(-time.Minute),
				})
			},
			want: "channel is disabled",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := &recordingNotifier{}
			st, h, sess := retryFixture(t, n)
			tc.seed(st)

			rec := postWithSession(t, h, sess, "/alerts/1/deliveries/1/retry", nil)
			wantStatus(t, rec, http.StatusOK)
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("fragment = %s, want it to say %q", rec.Body.String(), tc.want)
			}
			if n.count() != 0 {
				t.Errorf("notifier sends = %d, want 0 — a gated retry must not deliver", n.count())
			}
		})
	}
}

func TestRetryDeliveryErrorPaths(t *testing.T) {
	_, h, sess := newWriteDashboard(t, scriptedNotifier{})

	t.Run("malformed alert id", func(t *testing.T) {
		wantStatus(t, postWithSession(t, h, sess, "/alerts/abc/deliveries/1/retry", nil), http.StatusNotFound)
	})
	t.Run("malformed channel id", func(t *testing.T) {
		wantStatus(t, postWithSession(t, h, sess, "/alerts/1/deliveries/abc/retry", nil), http.StatusNotFound)
	})
	t.Run("unknown alert", func(t *testing.T) {
		wantStatus(t, postWithSession(t, h, sess, "/alerts/42/deliveries/1/retry", nil), http.StatusInternalServerError)
	})
}

// --- dead letters ---

func TestListDeadLettersRendersQueue(t *testing.T) {
	st, h, sess := newWriteDashboard(t, scriptedNotifier{})
	st.channels[1] = store.Channel{ID: 1, Name: "ops"}
	st.deadLetters[1] = store.DeadLetter{
		ID: 1, AlertID: 1, ChannelID: 1,
		LastError: "status 500: upstream exploded", AttemptCount: 5, LastStatus: 500,
		CreatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
	}
	// A dead letter whose channel is gone falls back to the raw id.
	st.deadLetters[2] = store.DeadLetter{ID: 2, AlertID: 1, ChannelID: 77}

	req := httptest.NewRequest(http.MethodGet, "/dead-letters", nil)
	req.AddCookie(sess)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	wantStatus(t, rec, http.StatusOK)

	body := rec.Body.String()
	for _, want := range []string{"ops", "status 500: upstream exploded", ">77<"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}

	t.Run("store error renders the error page", func(t *testing.T) {
		st.errAt["ListDeadLetters"] = errors.New("db down")
		defer delete(st.errAt, "ListDeadLetters")
		req := httptest.NewRequest(http.MethodGet, "/dead-letters", nil)
		req.AddCookie(sess)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		wantStatus(t, rec, http.StatusInternalServerError)
	})
}

func TestRedriveDeadLetterDeliversAndClearsQueue(t *testing.T) {
	n := &recordingNotifier{}
	st, h, sess := retryFixture(t, n)
	st.deadLetters[1] = store.DeadLetter{
		ID: 1, AlertID: 1, ChannelID: 1,
		LastError: "status 500: upstream exploded", AttemptCount: 5, LastStatus: 500,
	}

	rec := postWithSession(t, h, sess, "/dead-letters/1/redrive", nil)
	wantRedirect(t, rec, "/dead-letters")
	if _, ok := st.deadLetters[1]; ok {
		t.Fatal("a successful redrive must delete the dead letter")
	}
	if n.count() != 1 {
		t.Errorf("notifier sends = %d, want 1", n.count())
	}
	if len(st.attempts) != 1 || st.attempts[0].Status != "success" {
		t.Errorf("attempts = %+v, want one new success row", st.attempts)
	}
}

func TestRedriveDeadLetterErrorPages(t *testing.T) {
	t.Run("malformed id", func(t *testing.T) {
		_, h, sess := newWriteDashboard(t, scriptedNotifier{})
		rec := postWithSession(t, h, sess, "/dead-letters/abc/redrive", nil)
		wantStatus(t, rec, http.StatusBadRequest)
		if !strings.Contains(rec.Body.String(), "malformed dead letter id") {
			t.Errorf("body = %s", rec.Body.String())
		}
	})
	t.Run("unknown id", func(t *testing.T) {
		_, h, sess := newWriteDashboard(t, scriptedNotifier{})
		rec := postWithSession(t, h, sess, "/dead-letters/9/redrive", nil)
		wantStatus(t, rec, http.StatusNotFound)
	})
	t.Run("dead letter whose alert is gone", func(t *testing.T) {
		st, h, sess := newWriteDashboard(t, scriptedNotifier{})
		st.deadLetters[1] = store.DeadLetter{ID: 1, AlertID: 404, ChannelID: 1}
		rec := postWithSession(t, h, sess, "/dead-letters/1/redrive", nil)
		wantStatus(t, rec, http.StatusNotFound)
		if _, ok := st.deadLetters[1]; !ok {
			t.Error("a rejected redrive must keep the row")
		}
	})
	t.Run("disabled channel", func(t *testing.T) {
		st, h, sess := newWriteDashboard(t, scriptedNotifier{})
		st.alerts[1] = store.Alert{ID: 1, EventID: "ev-1"}
		st.channels[1] = store.Channel{ID: 1, Name: "ops", Type: "mock", Enabled: false}
		st.deadLetters[1] = store.DeadLetter{ID: 1, AlertID: 1, ChannelID: 1}
		rec := postWithSession(t, h, sess, "/dead-letters/1/redrive", nil)
		wantStatus(t, rec, http.StatusBadGateway)
		if !strings.Contains(rec.Body.String(), "Redrive failed") {
			t.Errorf("body = %s", rec.Body.String())
		}
		if _, ok := st.deadLetters[1]; !ok {
			t.Error("a failed redrive must keep the row")
		}
	})
	t.Run("delivery failure keeps the row", func(t *testing.T) {
		st, h, sess := newWriteDashboard(t, &recordingNotifier{err: errors.New("upstream exploded")})
		st.alerts[1] = store.Alert{ID: 1, EventID: "ev-1"}
		st.channels[1] = store.Channel{ID: 1, Name: "ops", Type: "mock", Enabled: true}
		st.deadLetters[1] = store.DeadLetter{ID: 1, AlertID: 1, ChannelID: 1}
		rec := postWithSession(t, h, sess, "/dead-letters/1/redrive", nil)
		wantStatus(t, rec, http.StatusBadGateway)
		if _, ok := st.deadLetters[1]; !ok {
			t.Error("a failed redrive must keep the row")
		}
		if len(st.attempts) != 1 || st.attempts[0].Status != "failed" {
			t.Errorf("attempts = %+v, want one failed row", st.attempts)
		}
	})
}

// --- import ---

func TestImportContractsWebFromCSV(t *testing.T) {
	st, h, sess := newWriteDashboard(t, scriptedNotifier{})
	csvBody := fakeContractA + "\n\n" + fakeContractB + "\n"

	rec := postMultipartUpload(t, h, sess, "/monitors/import", "file", "contracts.csv",
		[]byte(csvBody), url.Values{"name": {"treasury"}})
	wantRedirect(t, rec, "/monitors")
	if len(st.monitors) != 2 {
		t.Fatalf("monitors = %d, want 2", len(st.monitors))
	}
	for _, m := range st.monitors {
		if !strings.HasPrefix(m.Name, "treasury-") {
			t.Errorf("monitor name %q must carry the import prefix", m.Name)
		}
		if !m.Enabled {
			t.Errorf("imported monitor %q must be enabled", m.Name)
		}
	}

	t.Run("re-importing the same contracts creates nothing new", func(t *testing.T) {
		rec := postMultipartUpload(t, h, sess, "/monitors/import", "file", "contracts.csv",
			[]byte(csvBody), url.Values{"name": {"treasury"}})
		wantRedirect(t, rec, "/monitors")
		if len(st.monitors) != 2 {
			t.Errorf("monitors = %d, want 2 — already-monitored contracts are skipped", len(st.monitors))
		}
	})

	t.Run("an invalid contract id anywhere rejects the whole file", func(t *testing.T) {
		rec := postMultipartUpload(t, h, sess, "/monitors/import", "file", "contracts.csv",
			[]byte(fakeContractA+"\nnot-a-contract\n"), url.Values{"name": {"bad"}})
		wantStatus(t, rec, http.StatusBadRequest)
		if !strings.Contains(rec.Body.String(), "invalid contract ID on row 2") {
			t.Errorf("body = %s, want the offending row named", rec.Body.String())
		}
	})

	t.Run("missing file", func(t *testing.T) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		_ = w.WriteField("name", "treasury")
		_ = w.Close()
		req := httptest.NewRequest(http.MethodPost, "/monitors/import", &buf)
		req.Header.Set("Content-Type", w.FormDataContentType())
		req.AddCookie(sess)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		wantStatus(t, rec, http.StatusBadRequest)
		if !strings.Contains(rec.Body.String(), "file is required") {
			t.Errorf("body = %s", rec.Body.String())
		}
	})

	t.Run("a JSON payload works too", func(t *testing.T) {
		payload, err := json.Marshal(map[string]any{"contract_ids": []string{fakeContractA}})
		if err != nil {
			t.Fatal(err)
		}
		before := len(st.monitors)
		rec := postMultipartUpload(t, h, sess, "/monitors/import", "file", "contracts.json",
			payload, url.Values{"name": {"fromjson"}})
		// fakeContractA is already monitored, so the JSON import skips it
		// and creates nothing — but the request still succeeds.
		wantRedirect(t, rec, "/monitors")
		if len(st.monitors) != before {
			t.Errorf("monitors = %d, want %d", len(st.monitors), before)
		}
	})

	t.Run("a spreadsheet BOM and header row do not break the import", func(t *testing.T) {
		var b bytes.Buffer
		b.WriteString("\xEF\xBB\xBF")
		b.WriteString("contract_id\n")
		b.WriteString(fakeContractB + "\n")
		before := len(st.monitors)
		// fakeContractB is already monitored from the CSV case, so this
		// pins that the BOM and header parse to the same id and skip.
		rec := postMultipartUpload(t, h, sess, "/monitors/import", "file", "bom.csv",
			b.Bytes(), url.Values{"name": {"bom"}})
		wantRedirect(t, rec, "/monitors")
		if len(st.monitors) != before {
			t.Errorf("monitors = %d, want %d", len(st.monitors), before)
		}
	})

	t.Run("a store error on one create does not fail the request", func(t *testing.T) {
		st.errAt["CreateMonitor"] = errors.New("db down")
		defer delete(st.errAt, "CreateMonitor")
		before := len(st.monitors)
		rec := postMultipartUpload(t, h, sess, "/monitors/import", "file", "contracts.csv",
			[]byte(fakeContractA+"\n"), url.Values{"name": {"x"}})
		wantRedirect(t, rec, "/monitors")
		if len(st.monitors) != before {
			t.Errorf("monitors = %d, want %d", len(st.monitors), before)
		}
	})
}

// parseWebCSV is the parser behind the upload form; the table pins the
// quirks real files have: a header row, blank lines, a BOM, quoted fields
// and — the moment the importer rejects the file — garbage.
func TestParseWebCSV(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    []string
		wantErr bool
	}{
		{
			name: "one id per line",
			in:   "AAA\nBBB\n",
			want: []string{"AAA", "BBB"},
		},
		{
			name: "header row is skipped",
			in:   "contract_id\nAAA\nBBB\n",
			want: []string{"AAA", "BBB"},
		},
		{
			name: "blank and whitespace-only lines are skipped",
			in:   "\nAAA\n\n   \nBBB\n",
			want: []string{"AAA", "BBB"},
		},
		{
			name: "utf-8 BOM is stripped from the first id",
			in:   "\xEF\xBB\xBFAAA\nBBB\n",
			want: []string{"AAA", "BBB"},
		},
		{
			name: "quoted fields keep their content",
			in:   "\"AAA\"\n\"BB, BBB\"\n",
			want: []string{"AAA", "BB, BBB"},
		},
		{
			name:    "a bare quote is a parse error",
			in:      "AA\"BB\n",
			wantErr: true,
		},
		{
			name:    "more than 500 rows is refused",
			in:      strings.Repeat("AAA\n", 501),
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseWebCSV(strings.NewReader(tc.in))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseWebCSV(%q) = %v, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseWebCSV(%q): %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ids = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("ids[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// --- saved searches ---

func TestSetAndClearDefaultSearch(t *testing.T) {
	st, h, sess := newWriteDashboard(t, scriptedNotifier{})
	st.searches[1] = store.SavedSearch{ID: 1, Name: "one"}
	st.searches[2] = store.SavedSearch{ID: 2, Name: "two"}

	rec := postWithSession(t, h, sess, "/searches/1/default", nil)
	wantRedirect(t, rec, "/searches")
	if !st.searches[1].IsDefault {
		t.Fatal("search 1 was not made the default")
	}
	if st.searches[2].IsDefault {
		t.Error("only one search can be the default")
	}

	t.Run("making another default moves it", func(t *testing.T) {
		rec := postWithSession(t, h, sess, "/searches/2/default", nil)
		wantRedirect(t, rec, "/searches")
		if st.searches[2].IsDefault == false || st.searches[1].IsDefault {
			t.Errorf("defaults: 1=%v 2=%v, want only 2", st.searches[1].IsDefault, st.searches[2].IsDefault)
		}
	})

	t.Run("clearing", func(t *testing.T) {
		rec := postWithSession(t, h, sess, "/searches/2/undefault", nil)
		wantRedirect(t, rec, "/searches")
		if st.searches[2].IsDefault {
			t.Error("default was not cleared")
		}
	})

	t.Run("malformed id", func(t *testing.T) {
		rec := postWithSession(t, h, sess, "/searches/abc/default", nil)
		wantStatus(t, rec, http.StatusBadRequest)
		if !strings.Contains(rec.Body.String(), "invalid search id") {
			t.Errorf("body = %s", rec.Body.String())
		}
		rec = postWithSession(t, h, sess, "/searches/abc/undefault", nil)
		wantStatus(t, rec, http.StatusBadRequest)
	})

	t.Run("unknown id", func(t *testing.T) {
		wantStatus(t, postWithSession(t, h, sess, "/searches/404/default", nil), http.StatusNotFound)
		wantStatus(t, postWithSession(t, h, sess, "/searches/404/undefault", nil), http.StatusNotFound)
	})

	t.Run("store error", func(t *testing.T) {
		st.errAt["SetDefaultSearch"] = errors.New("db down")
		defer delete(st.errAt, "SetDefaultSearch")
		wantStatus(t, postWithSession(t, h, sess, "/searches/1/default", nil), http.StatusInternalServerError)

		st.errAt["ClearDefaultSearch"] = errors.New("db down")
		defer delete(st.errAt, "ClearDefaultSearch")
		wantStatus(t, postWithSession(t, h, sess, "/searches/1/undefault", nil), http.StatusInternalServerError)
	})
}
