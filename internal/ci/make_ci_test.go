package ci

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// Commands make ci must run, in order, matching the local-reproducible
// checks in .github/workflows/ci.yml. The lint job uses the
// golangci-lint action; locally that is `golangci-lint run`.
var wantCI = []string{
	"go build ./...",
	"go vet ./...",
	"go test ./...",
	"golangci-lint run",
}

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

func makefileCIRecipe(t *testing.T, makefile string) []string {
	t.Helper()
	lines := strings.Split(makefile, "\n")
	var recipe []string
	inCI := false
	for _, line := range lines {
		if strings.HasPrefix(line, "ci:") {
			inCI = true
			continue
		}
		if inCI {
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
		t.Fatal("Makefile has no ci recipe")
	}
	return recipe
}

func TestMakeCIMatchesWorkflow(t *testing.T) {
	root := repoRoot(t)
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	workflow, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}

	if !strings.Contains(string(makefile), ".github/workflows/ci.yml") {
		t.Fatal("Makefile ci target must note that it stays in sync with .github/workflows/ci.yml")
	}

	got := makefileCIRecipe(t, string(makefile))
	if len(got) != len(wantCI) {
		t.Fatalf("make ci recipe = %v, want %v", got, wantCI)
	}
	for i, cmd := range wantCI {
		if got[i] != cmd {
			t.Fatalf("make ci step %d = %q, want %q", i, got[i], cmd)
		}
	}

	yml := string(workflow)
	for _, cmd := range []string{"go build ./...", "go vet ./...", "go test ./..."} {
		if !strings.Contains(yml, cmd) {
			t.Fatalf("ci.yml missing %q", cmd)
		}
	}
	if !regexp.MustCompile(`golangci-lint`).MatchString(yml) {
		t.Fatal("ci.yml missing golangci-lint")
	}
}

func TestMakeCIIsPhony(t *testing.T) {
	root := repoRoot(t)
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	if !regexp.MustCompile(`(?m)^\.PHONY:.*\bci\b`).Match(makefile) {
		t.Fatal("ci is not listed in .PHONY")
	}
}
