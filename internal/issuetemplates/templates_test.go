package issuetemplates

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot walks up from the test working directory to the module root
// so this package can read .github files regardless of `go test` cwd.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from " + dir)
		}
		dir = parent
	}
}

func readTemplate(t *testing.T, name string) (front, body string) {
	t.Helper()
	path := filepath.Join(repoRoot(t), ".github", "ISSUE_TEMPLATE", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	text := string(raw)
	if !strings.HasPrefix(text, "---\n") {
		t.Fatalf("%s: missing YAML front matter", name)
	}
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		t.Fatalf("%s: front matter not closed", name)
	}
	return rest[:end], rest[end+len("\n---\n"):]
}

func hasLabel(front, want string) bool {
	for _, line := range strings.Split(front, "\n") {
		if !strings.HasPrefix(line, "labels:") {
			continue
		}
		return strings.Contains(line, want)
	}
	return false
}

func TestChannelRequestTemplate(t *testing.T) {
	front, body := readTemplate(t, "channel_request.md")
	if !strings.Contains(front, "name: Channel request") {
		t.Fatal("channel template: missing name")
	}
	if !hasLabel(front, "enhancement") || !hasLabel(front, "channel") {
		t.Fatalf("channel template labels want enhancement+channel, got:\n%s", front)
	}
	for _, want := range []string{
		"**The service**",
		"**API docs for sending a message**",
		"**Authentication**",
		"**Rate limits and message size**",
		"`notify.Notifier`",
		"`DefaultFactory`",
		"`internal/notify`",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("channel template missing %q", want)
		}
	}
}

func TestRuleTypeRequestTemplate(t *testing.T) {
	front, body := readTemplate(t, "rule_type_request.md")
	if !strings.Contains(front, "name: Rule type request") {
		t.Fatal("rule-type template: missing name")
	}
	if !hasLabel(front, "enhancement") || !hasLabel(front, "rule-type") {
		t.Fatalf("rule-type template labels want enhancement+rule-type, got:\n%s", front)
	}
	for _, want := range []string{
		"**The monitoring question**",
		"**An event it should match**",
		"**An event it should not match**",
		"**Parameters you would configure**",
		"`rules.RuleEvaluator`",
		"`NewRegistry`",
		"`internal/rules`",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rule-type template missing %q", want)
		}
	}
}

func TestExistingTemplatesStillPresent(t *testing.T) {
	dir := filepath.Join(repoRoot(t), ".github", "ISSUE_TEMPLATE")
	for _, name := range []string{"bug_report.md", "feature_request.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("expected existing template %s: %v", name, err)
		}
	}
}
