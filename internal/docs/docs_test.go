package docs

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/auth"
)

// repoRoot is the repository root relative to this package's directory, so
// the tests read the real documentation files on disk wherever they run.
const repoRoot = "../.."

// pagesAddedHere are the documentation pages pinned by this guard: the set
// introduced by the docs branches, each of which must exist and be linked from
// docs/SUMMARY.md — the "done" condition every one of the issues specifies —
// and they are the pages whose internal links the link checker below walks.
var pagesAddedHere = []string{
	"docs/operations/security.md",
	"docs/operations/upgrading.md",
	"docs/guides/writing-rules.md",
	"docs/contributing/testing.md",
	"docs/guides/escalation.md",
	"docs/reference/authorization.md",
	"docs/rules/numeric-range.md",
	"docs/rules/contract-allowlist.md",
	"docs/rules/event-name-glob.md",
	"docs/rules/token-supply-change.md",
	"docs/operations/backup-restore.md",
	"docs/operations/retention.md",
	"docs/channels/email.md",
	"docs/channels/federation.md",
}

// markdownLink matches [text](target) links in prose. Code fences and inline
// code can contain square brackets too, but the four pages under test keep
// links in prose, so a simple pattern is enough and keeps the test readable.
var markdownLink = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)

// headingLine matches an ATX heading, capturing its text after the # marks.
var headingLine = regexp.MustCompile(`^#{1,6}\s+(.+?)\s*$`)

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(raw)
}

// headingSlugs returns the GitHub-style anchor slugs of every heading in a
// markdown document: lower-cased, punctuation dropped, underscores and
// hyphens kept, spaces mapped to hyphens.
func headingSlugs(markdown string) map[string]bool {
	slugs := map[string]bool{}
	for _, line := range strings.Split(markdown, "\n") {
		if m := headingLine.FindStringSubmatch(line); m != nil {
			slugs[headingSlug(m[1])] = true
		}
	}
	return slugs
}

func headingSlug(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return b.String()
}

// TestSummaryLinksEveryNewPage pins the summary contract: docs/SUMMARY.md
// links each page added by this branch, and each page exists. Fails if a
// page is deleted or its summary entry is dropped.
func TestSummaryLinksEveryNewPage(t *testing.T) {
	summary := readRepoFile(t, "docs/SUMMARY.md")
	for _, page := range pagesAddedHere {
		if _, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(page))); err != nil {
			t.Errorf("%s must exist: %v", page, err)
			continue
		}
		// SUMMARY.md links are slash-separated and relative to docs/, so the
		// entry is built with path, not filepath. filepath.Dir returns
		// "docs\operations" on Windows, which no longer carries the "docs/"
		// prefix being trimmed, and every page then looked unlinked.
		base := path.Base(page)
		if !strings.Contains(summary, "("+strings.TrimPrefix(path.Dir(page), "docs/")+"/"+base+")") {
			t.Errorf("docs/SUMMARY.md does not link %s", page)
		}
	}
}

// TestNewPagesLinksResolve walks every relative link in the new pages and
// fails for a target that does not exist, or a #fragment that matches no
// heading in the target document. Dead cross-references between the guides
// and the reference pages they deliberately link out to would otherwise go
// unnoticed until a reader hit them.
func TestNewPagesLinksResolve(t *testing.T) {
	for _, page := range pagesAddedHere {
		page := page
		t.Run(page, func(t *testing.T) {
			doc := readRepoFile(t, page)
			slugs := headingSlugs(doc)
			for _, link := range markdownLink.FindAllStringSubmatch(doc, -1) {
				text, target := link[1], link[2]
				if target == "" || strings.HasPrefix(target, "http://") ||
					strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "mailto:") {
					continue
				}
				fragment := ""
				if i := strings.IndexByte(target, '#'); i >= 0 {
					fragment = target[i+1:]
					target = target[:i]
				}
				if target == "" {
					// Same-page anchor: the fragment must match one of this
					// document's own headings.
					if !slugs[fragment] {
						t.Errorf("link %q points at missing anchor #%q in %s", text, fragment, page)
					}
					continue
				}
				// page is repo-root relative ("docs/guides/writing-rules.md"),
				// so resolving the link against its directory is enough.
				resolved := filepath.Join(repoRoot, filepath.FromSlash(filepath.Dir(page)), filepath.FromSlash(target))
				if _, err := os.Stat(resolved); err != nil {
					t.Errorf("link %q targets missing file %s", text, target)
					continue
				}
				if fragment != "" && strings.HasSuffix(target, ".md") {
					raw, err := os.ReadFile(resolved)
					if err != nil {
						t.Errorf("link %q: read %s: %v", text, target, err)
						continue
					}
					if !headingSlugs(string(raw))[fragment] {
						t.Errorf("link %q targets missing anchor #%q in %s", text, fragment, target)
					}
				}
			}
		})
	}
}

// TestAuthorizationReferenceCoversVocabulary keeps the reference honest in the
// direction that matters when the code grows: a scope or role added to the
// vocabulary without a line in the page fails here instead of shipping
// undocumented. The reverse direction (a name in the page that no longer
// exists in code) is prose review; route patterns and shorthands make a simple
// text-based check of every backticked value too noisy.
func TestAuthorizationReferenceCoversVocabulary(t *testing.T) {
	doc := readRepoFile(t, "docs/reference/authorization.md")
	for _, scope := range auth.AllScopes {
		if !strings.Contains(doc, string(scope)) {
			t.Errorf("docs/reference/authorization.md does not mention scope %q", scope)
		}
	}
	for _, role := range []string{"viewer", "editor", "admin"} {
		if _, ok := auth.ParseRole(role); !ok {
			t.Fatalf("ParseRole(%q) rejected a documented role", role)
		}
		if !strings.Contains(doc, role) {
			t.Errorf("docs/reference/authorization.md does not mention role %q", role)
		}
	}
	if !strings.Contains(doc, string(auth.ScopeNone)) {
		t.Errorf("docs/reference/authorization.md does not mention scope %q", auth.ScopeNone)
	}
}
