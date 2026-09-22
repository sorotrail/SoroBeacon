package docs

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func configRepoRoot(t *testing.T) string {
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

func configRead(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// Every variable config.Load / ParseNetwork reads, plus the issue's
// "done" checklist (grouped sections, secret marking, SOURCE_MODE note).
var configRequired = []string{
	"DATABASE_URL",
	"SOURCE_MODE",
	"SOROTRAIL_URL",
	"NETWORK",
	"RPC_URL",
	"NETWORK_PASSPHRASE",
	"POLL_INTERVAL",
	"HTTP_ADDR",
	"CORS_ALLOWED_ORIGINS",
	"LOG_LEVEL",
	"## Database",
	"## RPC / event source",
	"## HTTP",
	"## Polling",
	"## Logging",
	"**required when `SOURCE_MODE=sorotrail`**",
	"**yes**",
}

func TestConfigurationReferenceCoversEveryEnvVar(t *testing.T) {
	root := configRepoRoot(t)
	doc := configRead(t, root, "docs/configuration.md")
	for _, want := range configRequired {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/configuration.md missing %q", want)
		}
	}
}

func TestREADMELinksConfigurationReference(t *testing.T) {
	root := configRepoRoot(t)
	readme := configRead(t, root, "README.md")
	if !strings.Contains(readme, "docs/configuration.md") {
		t.Fatal("README.md does not link docs/configuration.md")
	}
}
