package docs

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"unicode"

	"github.com/go-chi/chi/v5"

	"github.com/sorotrail/sorobeacon/internal/api"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
)

// docMentions reports whether the reference documents "METHOD /path" as a
// complete route mention. A bare substring check is not enough: deleting the
// `POST /monitors` row would still leave "POST /monitors/bulk" containing the
// needle, and the guard would pass with an entry missing. So every occurrence
// must be checked: the character right after the needle may not be a path
// continuation (a slash, a placeholder, a query, a filename suffix). A
// terminator — backtick, table pipe, space, end of file — means the mention
// really is this route.
func docMentions(doc, needle string) bool {
	for {
		i := strings.Index(doc, needle)
		if i < 0 {
			return false
		}
		next := i + len(needle)
		if next >= len(doc) || isRouteTerminator(rune(doc[next])) {
			return true
		}
		// This occurrence is the prefix of a longer path or query; keep
		// searching from after it.
		doc = doc[next:]
	}
}

// isRouteTerminator reports whether c ends a route mention: the route is
// complete here rather than the prefix of a longer path ("/monitors/bulk"),
// a placeholder ("/monitors/{id}"), a query or a file name ("/alerts.csv").
func isRouteTerminator(c rune) bool {
	if unicode.IsSpace(c) {
		return true
	}
	switch c {
	case '`', '|', ',', ')', ';', ':', '"', '\'', '<', '>', '[', ']', '*', '!', '@', '#', '$', '%', '^', '(', '\\', '+':
		return true
	}
	return false
}

// walkRoutes collects every method/path pair a client can actually call on
// the API router, normalised the way the reference writes them: no /api/v1
// mount prefix (the page states the base path once), no trailing slash
// (chi walks subroute roots as "/monitors/").
func walkRoutes(t *testing.T, r chi.Router) []string {
	t.Helper()
	var out []string
	walk := func(method, path string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if len(path) > 1 {
			path = strings.TrimRight(path, "/")
		}
		// Only the five verbs the reference documents; chi's automatic
		// HEAD/OPTIONS are not endpoints anyone scripts against.
		switch method {
		case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			return nil
		}
		// chi reports the NotFound catch-all as "/*"; the reference
		// documents the error envelope once, not 404 as an endpoint.
		if path == "/*" || path == "/" {
			return nil
		}
		out = append(out, method+" "+path)
		return nil
	}
	if err := chi.Walk(r, walk); err != nil {
		t.Fatalf("walk API routes: %v", err)
	}
	return out
}

// docsTestServer builds an API server just enough to walk its router. Routes
// only needs the route table, never a request served, so nil dependencies are
// safe: no handler runs.
func docsTestServer() *api.Server {
	return api.New(nil, rules.NewRegistry(), notify.DefaultFactory(), nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestAPIReferenceCoversEveryRoute walks the live API router and fails when a
// registered method/path pair has no entry in docs/reference/api.md. It is the
// drift check for issue #528: someone scripting against SoroBeacon should not
// have to read internal/api to discover an endpoint, and nothing else ties the
// reference to the router — deleting one entry from the doc must fail here.
func TestAPIReferenceCoversEveryRoute(t *testing.T) {
	doc := readRepoFile(t, "docs/reference/api.md")

	var missing []string
	for _, rt := range walkRoutes(t, docsTestServer().Routes()) {
		if !docMentions(doc, rt) {
			missing = append(missing, rt)
		}
	}
	for _, rt := range missing {
		t.Errorf("docs/reference/api.md does not document route %s — add a table row or prose entry for it", rt)
	}
}

// TestAPIReferenceGuardFailsWhenAnEntryIsDeleted pins that the guard has
// teeth: the issue requires it to fail when a documented entry is removed, so
// the same mention check runs against a copy of the reference with the
// `POST /monitors` row deleted and must then report the route missing — even
// though "POST /monitors/bulk" and friends are still in the document.
func TestAPIReferenceGuardFailsWhenAnEntryIsDeleted(t *testing.T) {
	doc := readRepoFile(t, "docs/reference/api.md")
	stripped := strings.ReplaceAll(doc, "`POST /monitors`", "`POST /missing`")
	if stripped == doc {
		t.Fatal("could not strip the POST /monitors entry from the reference")
	}
	if docMentions(stripped, "POST /monitors") {
		t.Fatal("the guard still considers POST /monitors documented after its entry was deleted")
	}
}
