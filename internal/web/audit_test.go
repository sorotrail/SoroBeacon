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
	"time"

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

// auditPageStore records every filter the audit page sends and serves canned
// entries, so a test can assert the query the handler asked the store to run
// rather than only the rows it printed — a filter silently dropped on the way
// down renders exactly like one that matched everything.
type auditPageStore struct {
	emptyStore
	filters []store.AuditFilter
	entries []store.AuditEntry
}

func (s *auditPageStore) ListAuditEntries(_ context.Context, f store.AuditFilter) ([]store.AuditEntry, error) {
	s.filters = append(s.filters, f)
	return s.entries, nil
}

func (s *auditPageStore) onlyFilter(t *testing.T) store.AuditFilter {
	t.Helper()
	if len(s.filters) != 1 {
		t.Fatalf("ListAuditEntries called %d times, want 1", len(s.filters))
	}
	return s.filters[0]
}

// auditEntry builds an entry with the fields the page renders.
func auditEntry(id int64, actor, action, targetType string, targetID int64, diff string) store.AuditEntry {
	return store.AuditEntry{
		ID:         id,
		Actor:      actor,
		Action:     action,
		TargetType: targetType,
		TargetID:   targetID,
		Diff:       []byte(diff),
		CreatedAt:  time.Date(2026, 10, 3, 14, 5, 0, 0, time.UTC),
	}
}

// TestAuditPageSendsItsFiltersToTheStore pins the three filters the issue
// asks for: entity type, entity id and actor all have to reach the store query.
func TestAuditPageSendsItsFiltersToTheStore(t *testing.T) {
	st := &auditPageStore{}
	html := getHTML(t, st, "/audit?target_type=monitor&target_id=5&actor=alice")

	f := st.onlyFilter(t)
	if f.TargetType != "monitor" || f.TargetID != 5 || f.Actor != "alice" {
		t.Fatalf("filter = %+v, want monitor/5/alice", f)
	}
	if f.Limit != auditPageSize {
		t.Errorf("limit = %d, want the page size %d", f.Limit, auditPageSize)
	}
	// The applied values stay in the form so the operator can narrow further.
	for _, want := range []string{`value="5"`, `value="alice"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("audit page dropped %q from the form, got:\n%s", want, html)
		}
	}
}

// TestAuditPageDropsAnUnknownEntityTypeInsteadOfFailing follows the alerts
// page's rule: a value this build does not know lists everything rather than
// erroring, so a link written before a new entity type existed still works.
func TestAuditPageDropsAnUnknownEntityTypeInsteadOfFailing(t *testing.T) {
	st := &auditPageStore{}
	html := getHTML(t, st, "/audit?target_type=spaceship")

	if f := st.onlyFilter(t); f.TargetType != "" {
		t.Fatalf("unknown type was sent as %q, want dropped", f.TargetType)
	}
	if !strings.Contains(html, "All entities") {
		t.Fatalf("the form does not show the filter was dropped, got:\n%s", html)
	}
}

// TestAuditPageStopsOnAnUnreadableTargetID: a filter that cannot be read would
// silently widen to the whole log, which reads as "nothing happened" — so it
// stops the listing, says why, and keeps the typed value to fix in place.
func TestAuditPageStopsOnAnUnreadableTargetID(t *testing.T) {
	st := &auditPageStore{}
	html := getHTML(t, st, "/audit?target_id=abc")

	if len(st.filters) != 0 {
		t.Fatalf("an unreadable id still queried the store: %+v", st.filters)
	}
	if !strings.Contains(html, "could not be read") {
		t.Fatalf("no message for the unreadable id, got:\n%s", html)
	}
	if !strings.Contains(html, `value="abc"`) {
		t.Fatalf("unreadable id not echoed back, got:\n%s", html)
	}
}

// TestAuditPagePaginatesWithACursorAndKeepsItsFilters is the API's keyset
// paging applied to the page: a full page yields an Older link whose cursor
// is the last entry's id, and following it asks for the entries below that id
// with the same filters still applied.
func TestAuditPagePaginatesWithACursorAndKeepsItsFilters(t *testing.T) {
	st := &auditPageStore{}
	for i := 0; i < auditPageSize; i++ {
		st.entries = append(st.entries, auditEntry(int64(100-i), "alice", "update", "monitor", 5, `{"fields":["enabled"]}`))
	}
	html := getHTML(t, st, "/audit?target_type=monitor&actor=alice")
	first := st.onlyFilter(t)
	const attr = `href="`
	i := strings.Index(html, attr+`/audit?`)
	if i < 0 {
		t.Fatalf("no Older link on a full page, got:\n%s", html)
	}
	rest := html[i+len(attr):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatal("unterminated Older href")
	}
	link := strings.ReplaceAll(rest[:end], "&amp;", "&")
	if !strings.Contains(link, "cursor=51") {
		t.Fatalf("Older link %q has no cursor for the last entry", link)
	}
	if !strings.Contains(link, "target_type=monitor") || !strings.Contains(link, "actor=alice") {
		t.Fatalf("Older link %q dropped the filters", link)
	}

	getHTML(t, st, link)
	second := st.filters[1]
	if second.AfterID != 51 {
		t.Errorf("cursor = %d, want 51", second.AfterID)
	}
	if second.TargetType != first.TargetType || second.Actor != first.Actor {
		t.Errorf("filters moved between pages: %+v then %+v", first, second)
	}

	// A short page is the last one, so it offers no further link.
	st2 := &auditPageStore{entries: []store.AuditEntry{auditEntry(1, "alice", "create", "channel", 7, `{}`)}}
	short := getHTML(t, st2, "/audit")
	if strings.Contains(short, `href="/audit?`) {
		t.Fatalf("a short page still offers an Older link, got:\n%s", short)
	}
}

// TestAuditPageRendersTheEntryButNeverItsValues guards the secret rule where
// it is easiest to break: the page shows the actor, the action, the entity and
// the names of the fields that changed — while a diff that carries values is
// rendered as nothing at all, so a webhook URL can never reach the browser.
func TestAuditPageRendersTheEntryButNeverItsValues(t *testing.T) {
	st := &auditPageStore{entries: []store.AuditEntry{
		auditEntry(9, "alice", "update", "channel", 7, `{"fields":["name","config"]}`),
		auditEntry(8, "alice", "update", "channel", 7, `{"config":{"webhook_url":"https://hooks.example/SUPER-SECRET-TOKEN"}}`),
	}}
	html := getHTML(t, st, "/audit")

	for _, want := range []string{
		"<code>alice</code>",
		"update",
		"name, config",
		`href="/channels/7/edit"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("audit page missing %q, got:\n%s", want, html)
		}
	}
	if strings.Contains(html, "SUPER-SECRET-TOKEN") || strings.Contains(html, "webhook_url") {
		t.Fatalf("audit page leaked a channel config value:\n%s", html)
	}
}

// TestAuditPageDistinguishesAnEmptyLogFromAnEmptyResult: with no filters an
// empty table means nothing has ever changed, which is worth saying differently
// from "your filter matched nothing".
func TestAuditPageDistinguishesAnEmptyLogFromAnEmptyResult(t *testing.T) {
	empty := getHTML(t, &auditPageStore{}, "/audit")
	if !strings.Contains(empty, "No changes recorded yet") {
		t.Fatalf("empty log does not explain itself, got:\n%s", empty)
	}
	filtered := getHTML(t, &auditPageStore{}, "/audit?actor=ghost")
	if !strings.Contains(filtered, "No audit entries match these filters") {
		t.Fatalf("filtered empty list does not say so, got:\n%s", filtered)
	}
	if strings.Contains(filtered, "No changes recorded yet") {
		t.Fatalf("a filtered empty list claimed the log was never written, got:\n%s", filtered)
	}
}

// TestAuditWebRouteTableCoversEveryMutatingDashboardRoute walks the dashboard
// router and fails the build if a mutating endpoint (POST/PUT/PATCH/DELETE) is
// registered without an entry in AuditWebRouteTable or AuditExemptWebRoutes.
func TestAuditWebRouteTableCoversEveryMutatingDashboardRoute(t *testing.T) {
	routes := walkedDashboardRoutes(t)
	if len(routes) < 20 {
		t.Fatalf("expected at least 20 dashboard routes, got %d", len(routes))
	}

	tablePatterns := make(map[string]bool)
	for _, k := range AuditWebRouteTable.Patterns() {
		tablePatterns[k] = true
	}
	registeredMutating := make(map[string]bool)
	for _, rt := range routes {
		switch rt.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			continue
		}
		key := rt.Method + " " + rt.Path
		registeredMutating[key] = true

		inTable := tablePatterns[key]
		reason, inExempt := AuditExemptWebRoutes[key]
		if inTable && inExempt {
			t.Errorf("dashboard route %s appears in both AuditWebRouteTable and AuditExemptWebRoutes", key)
		}
		if !inTable && !inExempt {
			t.Errorf("mutating dashboard route %s is missing from AuditWebRouteTable and AuditExemptWebRoutes", key)
		}
		if inTable {
			spec, ok := AuditWebRouteTable.Lookup(rt.Method, rt.Path)
			if !ok || spec.Action == "" || !store.ValidAuditTargetType(spec.TargetType) {
				t.Errorf("dashboard route %s has invalid AuditWebSpec: %+v", key, spec)
			}
		}
		if inExempt && strings.TrimSpace(reason) == "" {
			t.Errorf("exempt dashboard route %s must state a non-empty reason", key)
		}
	}

	for _, key := range AuditWebRouteTable.Patterns() {
		if !registeredMutating[key] {
			t.Errorf("AuditWebRouteTable names stale or non-mutating route %q", key)
		}
	}
	for key := range AuditExemptWebRoutes {
		if !registeredMutating[key] {
			t.Errorf("AuditExemptWebRoutes names stale or non-mutating route %q", key)
		}
	}
}
