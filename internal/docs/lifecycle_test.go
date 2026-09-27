package docs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The lifecycle guide anchors operational claims to real files, functions and
// log messages. Rename one of those and the page silently rots, so this test
// fails first — the same idea as TestConfigurationReferenceCoversEveryEnvVar.

// Files the guide names: checked for existence.
var lifecycleFiles = []string{
	"docs/guides/alert-lifecycle.md",
	"internal/poller/poller.go",
	"internal/poller/source.go",
	"internal/poller/rpcsource.go",
	"internal/stellar/decoder.go",
	"internal/rules/rules.go",
	"internal/store/postgres.go",
	"internal/store/store.go",
	"internal/notify/dispatcher.go",
	"internal/notify/notify.go",
}

// Code the guide quotes: functions, identifiers, log messages and SQL,
// searched in non-test Go sources.
var lifecycleSymbols = []string{
	"func (p *Poller) Poll(",
	"func (p *Poller) handleEvent(",
	"func (p *Poller) fireAlert(",
	"func (DefaultDecoder) DecodeEvent(",
	"func (r *Registry) Evaluate(",
	"func (r *Registry) AlertEventID(",
	"func (p *Postgres) CreateAlert(",
	"func (p *Postgres) ListChannelsForMonitor(",
	"func (d *Dispatcher) Dispatch(",
	"func (d *Dispatcher) deliver(",
	"func (f *Factory) New(",
	"func GateRetry(",
	"DefaultRetryCooldown",
	"AlertSuppressed",
	"AlertDuplicate",
	"AND c.enabled",
	"\"skipping invalid contract id\"",
	"\"poll failed\"",
	"\"rule evaluation failed\"",
	"\"alert suppressed by cooldown\"",
	"\"create alert\"",
	"\"build notifier\"",
	"\"alert delivery failed\"",
	"ON CONFLICT (rule_id, event_id) DO NOTHING",
}

// TestAlertLifecycleGuideAnchorsExist keeps docs/guides/alert-lifecycle.md
// honest: every file and symbol it names must still exist in the repo.
func TestAlertLifecycleGuideAnchorsExist(t *testing.T) {
	root := configRepoRoot(t)
	doc := configRead(t, root, "docs/guides/alert-lifecycle.md")
	if !strings.Contains(doc, "The life of an alert") {
		t.Fatal("docs/guides/alert-lifecycle.md does not look like the lifecycle guide")
	}
	for _, f := range lifecycleFiles {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Errorf("alert-lifecycle guide names %q but it is gone", f)
		}
	}
	for _, needle := range lifecycleSymbols {
		if !goSourceContains(t, root, needle) {
			t.Errorf("alert-lifecycle guide quotes %q but it is gone from the code", needle)
		}
	}
}

// TestSummaryLinksAlertLifecycleGuide is the other "done" criterion: the page
// must be reachable from the docs table of contents.
func TestSummaryLinksAlertLifecycleGuide(t *testing.T) {
	root := configRepoRoot(t)
	summary := configRead(t, root, "docs/SUMMARY.md")
	if !strings.Contains(summary, "guides/alert-lifecycle.md") {
		t.Fatal("docs/SUMMARY.md does not link guides/alert-lifecycle.md")
	}
}

// goSourceContains reports whether any non-test Go file in the repo contains
// the literal anchor. Anchors live in implementation files, and a test fixture
// could outlive the code it faked.
func goSourceContains(t *testing.T, root, needle string) bool {
	t.Helper()
	found := false
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || found {
			return err
		}
		if d.IsDir() {
			// bin is build output; .git holds no Go.
			switch d.Name() {
			case ".git", "bin", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), needle) {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk for %q: %v", needle, err)
	}
	return found
}
