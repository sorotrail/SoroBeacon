package docs

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for range 8 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not find repo root from test file")
	return ""
}

func read(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// required phrases are the issue's "done" checklist — the test fails if a
// later edit drops a triage path or the README link.
var required = []string{
	"GET /api/v1/health",
	"GET /api/v1/readyz",
	"GET /api/v1/stats",
	"GET /api/v1/alerts/{id}/deliveries",
	"after it was created",
	"LOG_LEVEL",
	"lower_snake_case",
	"bug_report.md",
	"No alerts appearing",
	"Alerts appearing but not delivered",
	"A channel test fails",
	"The service does not start",
	"Database connection failures",
	"RPC errors and rate limiting",
}

func TestTroubleshootingGuideCoversRequiredChecks(t *testing.T) {
	root := repoRoot(t)
	doc := read(t, root, "docs/troubleshooting.md")
	for _, want := range required {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/troubleshooting.md missing %q", want)
		}
	}
}

func TestREADMELinksTroubleshooting(t *testing.T) {
	root := repoRoot(t)
	readme := read(t, root, "README.md")
	if !strings.Contains(readme, "docs/troubleshooting.md") {
		t.Fatal("README.md does not link docs/troubleshooting.md")
	}
}
