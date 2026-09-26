package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// auditCaptureStore captures the entries the dashboard writes.
type auditCaptureStore struct {
	emptyStore
	entries []*store.AuditEntry
}

func (s *auditCaptureStore) CreateAuditEntry(_ context.Context, e *store.AuditEntry) error {
	s.entries = append(s.entries, e)
	return nil
}

func (s *auditCaptureStore) CreateChannel(_ context.Context, c *store.Channel) error {
	c.ID = 12
	return nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestDashboardChannelCreateIsAuditedWithoutSecrets(t *testing.T) {
	st := &auditCaptureStore{}
	srv, err := New(st, rules.NewRegistry(), notify.DefaultFactory(), testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	form := url.Values{
		"name":   {"ops"},
		"type":   {"slack"},
		"config": {`{"webhook_url":"https://hooks.example/SUPER-SECRET-TOKEN"}`},
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	res, err := client.PostForm(ts.URL+"/channels", form)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("create channel = %d, want 303", res.StatusCode)
	}
	if len(st.entries) != 1 {
		t.Fatalf("recorded %d entries, want exactly 1", len(st.entries))
	}
	e := st.entries[0]
	if e.Action != store.AuditActionCreate || e.TargetType != "channel" || e.TargetID != 12 {
		t.Fatalf("entry = %+v", e)
	}
	if e.Actor == "" {
		t.Error("entry has no actor (request id)")
	}
	if strings.Contains(string(e.Diff), "SUPER-SECRET-TOKEN") {
		t.Fatalf("audit diff leaked the channel secret: %s", e.Diff)
	}
	if !strings.Contains(string(e.Diff), "config") {
		t.Errorf("audit diff = %s, want the config field name recorded", e.Diff)
	}
}
