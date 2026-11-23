package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// captureAlertStore records every filter the alerts page sends so a test can
// assert on the query the handler asked the store to run, not only on the rows
// it printed. A filter silently dropped on the way to the database renders
// exactly like a filter that matched everything.
type captureAlertStore struct {
	emptyStore
	filters []store.AlertFilter
	alerts  []store.Alert
}

func (c *captureAlertStore) ListAlerts(_ context.Context, f store.AlertFilter) ([]store.Alert, error) {
	c.filters = append(c.filters, f)
	return c.alerts, nil
}

func (c *captureAlertStore) onlyFilter(t *testing.T) store.AlertFilter {
	t.Helper()
	if len(c.filters) != 1 {
		t.Fatalf("ListAlerts called %d times, want 1", len(c.filters))
	}
	return c.filters[0]
}

// fullAlertPage returns n alerts with sequential ids, which is how the page
// decides it is worth an Older link.
func fullAlertPage(n int) []store.Alert {
	out := make([]store.Alert, n)
	for i := range out {
		out[i] = store.Alert{ID: int64(i + 1)}
	}
	return out
}

// olderHref pulls the paging link out of a rendered alerts page. The value is
// a template.URL, but the HTML escaper may write its separators as entities,
// so both spellings are accepted.
func olderHref(t *testing.T, html string) string {
	t.Helper()
	const attr = `href="`
	i := strings.Index(html, attr+`/alerts?`)
	if i < 0 {
		t.Fatalf("no Older link in:\n%s", html)
	}
	rest := html[i+len(attr):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatal("unterminated Older href")
	}
	return strings.ReplaceAll(rest[:end], "&amp;", "&")
}

func TestAlertsPageSendsSearchAndRangeToTheStore(t *testing.T) {
	st := &captureAlertStore{}
	getHTML(t, st, "/alerts?q=transfer%20mint&from=2026-03-01&to=2026-03-05")

	f := st.onlyFilter(t)
	if f.Query != "transfer mint" {
		t.Fatalf("search term = %q, want the decoded q", f.Query)
	}
	// A picked day is a whole UTC day, so "to" becomes the next midnight: the
	// store compares with < and the end day has to be inside the range.
	for _, tt := range []struct{ name, got, want string }{
		{"from", f.From.UTC().Format(time.RFC3339), "2026-03-01T00:00:00Z"},
		{"to", f.To.UTC().Format(time.RFC3339), "2026-03-06T00:00:00Z"},
	} {
		if tt.got != tt.want {
			t.Fatalf("%s bound = %s, want %s", tt.name, tt.got, tt.want)
		}
	}
}

func TestAlertsPageKeepsSearchAndRangeInTheForm(t *testing.T) {
	st := &captureAlertStore{}
	html := getHTML(t, st, "/alerts?q=mint&from=2026-03-01&to=2026-03-05")
	for _, want := range []string{
		`name="q"`, `value="mint"`,
		`name="from"`, `value="2026-03-01"`,
		`name="to"`, `value="2026-03-05"`,
		// The zone the days are read in is stated on the page: the dashboard
		// can render timestamps in a preferred zone, and a filter labelled
		// with a zone it does not use is worse than one labelled UTC.
		"From (UTC)", "To (UTC)",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("alerts page missing %q, got:\n%s", want, html)
		}
	}
}

// TestAlertsPageOlderLinkRoundTripsSearchAndRange is the regression the issues
// call out: if the paging link drops the search term, page two is a different
// query than the one the operator ran, and if it re-sends the picker's bare
// date the window walks a day further on every click.
func TestAlertsPageOlderLinkRoundTripsSearchAndRange(t *testing.T) {
	st := &captureAlertStore{alerts: fullAlertPage(50)}
	html := getHTML(t, st, "/alerts?q=mint&from=2026-03-01&to=2026-03-05")
	first := st.onlyFilter(t)

	link := olderHref(t, html)
	if !strings.Contains(link, "cursor=50") {
		t.Fatalf("Older link %q has no cursor", link)
	}
	getHTML(t, st, link)
	if len(st.filters) != 2 {
		t.Fatalf("page two sent %d queries, want 2", len(st.filters))
	}
	second := st.filters[1]
	if second.Query != first.Query {
		t.Fatalf("search lost while paging: %q then %q", first.Query, second.Query)
	}
	if !second.From.Equal(first.From) || !second.To.Equal(first.To) {
		t.Fatalf("range moved while paging: %v..%v then %v..%v",
			first.From, first.To, second.From, second.To)
	}
	if second.AfterID != 50 {
		t.Fatalf("cursor = %d, want 50", second.AfterID)
	}
}

func TestAlertsPageReportsAnInvertedRangeInsteadOfAnEmptyTable(t *testing.T) {
	st := &captureAlertStore{alerts: fullAlertPage(3)}
	html := getHTML(t, st, "/alerts?from=2026-03-05&to=2026-03-01")
	if len(st.filters) != 0 {
		t.Fatalf("an empty range still queried the store: %+v", st.filters)
	}
	if !strings.Contains(html, "the end date is before the start date") {
		t.Fatalf("no message for the inverted range, got:\n%s", html)
	}
}

func TestAlertsPageReportsAnUnreadableDate(t *testing.T) {
	st := &captureAlertStore{}
	html := getHTML(t, st, "/alerts?from=tomorrow")
	if len(st.filters) != 0 {
		t.Fatalf("an unreadable bound still queried the store: %+v", st.filters)
	}
	if !strings.Contains(html, "could not be read") {
		t.Fatalf("no message for the unreadable date, got:\n%s", html)
	}
	// The bad value stays in the box so it can be fixed in place.
	if !strings.Contains(html, `value="tomorrow"`) {
		t.Fatalf("unreadable date not echoed back, got:\n%s", html)
	}
}

func TestAlertsPageReportsAnOverlongSearchInsteadOfShowingUnsearchedRows(t *testing.T) {
	st := &captureAlertStore{alerts: fullAlertPage(3)}
	long := strings.Repeat("a", store.MaxAlertSearchLen+1)
	html := getHTML(t, st, "/alerts?q="+long)

	// A full table under a search box the operator just typed into reads like
	// results for that term, so an unusable term lists nothing and says why —
	// the same handling an unreadable date gets.
	if len(st.filters) != 0 {
		t.Fatalf("overlong term still queried the store: %q", st.filters[0].Query)
	}
	if !strings.Contains(html, "Search is limited to") {
		t.Fatalf("no note about the overlong term, got:\n%s", html)
	}
	if !strings.Contains(html, "could not be applied") {
		t.Fatalf("the page did not explain its own empty list, got:\n%s", html)
	}
	// The term stays in the box so it can be trimmed in place.
	if !strings.Contains(html, `value="`+long+`"`) {
		t.Fatal("overlong term was dropped from the form")
	}
}

// TestAlertsPageSearchCapIsInclusive pins the boundary the handler checks: a
// term exactly at the cap is searched, one character more is not.
func TestAlertsPageSearchCapIsInclusive(t *testing.T) {
	atLimit := strings.Repeat("b", store.MaxAlertSearchLen)
	st := &captureAlertStore{}
	getHTML(t, st, "/alerts?q="+atLimit)
	if f := st.onlyFilter(t); f.Query != atLimit {
		t.Fatalf("term at the cap = %q, want it searched as typed", f.Query)
	}
}

// TestAlertsPageRendersASeverityPill guards a failure that stays invisible
// until real rows arrive: store.Severity is not a string, and html/template
// checks argument types only when the action runs, so a template that hands a
// Severity to a func taking one aborts the page partway through the table.
func TestAlertsPageRendersASeverityPill(t *testing.T) {
	st := &captureAlertStore{alerts: []store.Alert{testAlert(9, time.Now().UTC())}}
	html := getHTML(t, st, "/alerts")
	if !strings.Contains(html, `<span class="pill warning">warning</span>`) {
		t.Fatalf("severity did not render as a pill, got:\n%s", html)
	}
}

// TestAlertsPageEmptyStateFollowsTheFilterSet checks that a filter of any kind
// — including a date range, which the older check could not see — turns an
// empty list into an answer about the filter instead of an onboarding tour.
func TestAlertsPageEmptyStateFollowsTheFilterSet(t *testing.T) {
	st := &captureAlertStore{}
	html := getHTML(t, st, "/alerts?from=2026-03-01&to=2026-03-05")
	if !strings.Contains(html, "No alerts match these filters") {
		t.Fatalf("filtered empty list did not say so, got:\n%s", html)
	}
	if strings.Contains(html, "Nothing is being watched yet") {
		t.Fatalf("filtered empty list walked the operator through setup, got:\n%s", html)
	}

	// With nothing narrowing the list, the setup guidance is still the right
	// message for a brand new instance.
	bare := getHTML(t, &captureAlertStore{}, "/alerts")
	if !strings.Contains(bare, "Nothing is being watched yet") {
		t.Fatalf("unfiltered empty list should explain the instance, got:\n%s", bare)
	}
}
