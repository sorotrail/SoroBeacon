package ci

import (
	"os"
	"path/filepath"
	"regexp"
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

func makefileRecipe(t *testing.T, makefile, target string) []string {
	t.Helper()
	lines := strings.Split(makefile, "\n")
	var recipe []string
	prefix := target + ":"
	in := false
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) && (len(line) == len(prefix) || line[len(prefix)] == ' ' || line[len(prefix)] == '\t') {
			in = true
			continue
		}
		if in {
			if strings.HasPrefix(line, "\t") {
				cmd := strings.TrimSpace(line)
				if cmd != "" {
					recipe = append(recipe, cmd)
				}
				continue
			}
			if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			break
		}
	}
	if len(recipe) == 0 {
		t.Fatalf("Makefile has no %s recipe", target)
	}
	return recipe
}

func TestLintFixRunsGolangciLintFix(t *testing.T) {
	root := repoRoot(t)
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	got := makefileRecipe(t, string(makefile), "lint-fix")
	if len(got) != 1 || got[0] != "golangci-lint run --fix" {
		t.Fatalf("lint-fix recipe = %v, want [golangci-lint run --fix]", got)
	}
}

func TestLintIsUnchanged(t *testing.T) {
	root := repoRoot(t)
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	got := makefileRecipe(t, string(makefile), "lint")
	if len(got) != 1 || got[0] != "golangci-lint run" {
		t.Fatalf("lint recipe = %v, want [golangci-lint run] (CI must not pick up --fix)", got)
	}
}

func TestLintFixIsPhony(t *testing.T) {
	root := repoRoot(t)
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	if !regexp.MustCompile(`(?m)^\.PHONY:.*\blint-fix\b`).Match(makefile) {
		t.Fatal("lint-fix is not listed in .PHONY")
	}
	if !regexp.MustCompile(`(?m)^\.PHONY:.*\blint\b`).Match(makefile) {
		t.Fatal("lint is not listed in .PHONY")
	}
}

func TestContributingDocumentsLintFix(t *testing.T) {
	root := repoRoot(t)
	doc, err := os.ReadFile(filepath.Join(root, "CONTRIBUTING.md"))
	if err != nil {
		t.Fatalf("read CONTRIBUTING.md: %v", err)
	}
	text := string(doc)
	if !strings.Contains(text, "make lint-fix") {
		t.Fatal("CONTRIBUTING.md missing make lint-fix")
	}
	if !strings.Contains(text, "make lint") {
		t.Fatal("CONTRIBUTING.md missing make lint")
	}
}
