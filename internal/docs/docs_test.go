package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot is the repository root relative to this package's directory, so
// the tests read the real documentation files on disk wherever they run.
const repoRoot = "../.."

// pagesAddedHere are the four documentation pages this branch introduces.
// Each must exist and be linked from docs/SUMMARY.md — the "done" condition
// every one of the issues specifies — and they are the pages whose internal
// links the link checker below walks.
var pagesAddedHere = []string{
	"docs/operations/security.md",
	"docs/operations/upgrading.md",
	"docs/guides/writing-rules.md",
	"docs/contributing/testing.md",
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
		base := filepath.Base(page)
		if !strings.Contains(summary, "("+strings.TrimPrefix(filepath.Dir(page), "docs/")+"/"+base+")") {
			t.Errorf("docs/SUMMARY.md does not link %s", page)
		}
	}
}

// TestNewPagesLinksResolve walks every relative link in the four new pages
// and fails for a target that does not exist, or a #fragment that matches no
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
