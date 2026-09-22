package docs

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func systemdRepoRoot(t *testing.T) string {
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

func systemdRead(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// required phrases are the issue's "done" checklist — the test fails if a
// later edit drops a unit directive, the env-file mode, or the README link.
var required = []string{
	"Restart=on-failure",
	"After=network-online.target",
	"postgresql.service",
	"EnvironmentFile=",
	"0600",
	"NoNewPrivileges",
	"ProtectSystem",
	"PrivateTmp",
	"useradd",
	"make build",
	"journalctl",
	"journalctl -o cat",
	"JSON on stdout",
}

func TestSystemdGuideCoversRequiredChecks(t *testing.T) {
	root := systemdRepoRoot(t)
	doc := systemdRead(t, root, "docs/deployment-systemd.md")
	for _, want := range required {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/deployment-systemd.md missing %q", want)
		}
	}
}

func TestREADMELinksSystemdDeployment(t *testing.T) {
	root := systemdRepoRoot(t)
	readme := systemdRead(t, root, "README.md")
	if !strings.Contains(readme, "docs/deployment-systemd.md") {
		t.Fatal("README.md does not link docs/deployment-systemd.md")
	}
}
