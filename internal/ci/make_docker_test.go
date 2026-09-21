package ci

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func dockerRepoRoot(t *testing.T) string {
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

func dockerMakefile(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dockerRepoRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	return string(b)
}

func dockerRecipe(t *testing.T, makefile, target string) string {
	t.Helper()
	lines := strings.Split(makefile, "\n")
	var body []string
	in := false
	prefix := target + ":"
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			in = true
			continue
		}
		if in {
			if strings.HasPrefix(line, "\t") {
				body = append(body, strings.TrimSpace(line))
				continue
			}
			if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			break
		}
	}
	if len(body) == 0 {
		t.Fatalf("Makefile has no %s recipe", target)
	}
	return strings.Join(body, " ")
}

func TestDockerImageDefault(t *testing.T) {
	mk := dockerMakefile(t)
	if !regexp.MustCompile(`(?m)^IMAGE\s*\?=\s*sorobeacon\s*$`).MatchString(mk) {
		t.Fatal(`Makefile missing "IMAGE ?= sorobeacon"`)
	}
}

func TestDockerTargetsArePhony(t *testing.T) {
	mk := dockerMakefile(t)
	if !regexp.MustCompile(`(?m)^\.PHONY:.*\bdocker-build\b`).MatchString(mk) {
		t.Fatal("docker-build is not listed in .PHONY")
	}
	if !regexp.MustCompile(`(?m)^\.PHONY:.*\bdocker-run\b`).MatchString(mk) {
		t.Fatal("docker-run is not listed in .PHONY")
	}
}

func TestDockerBuildPassesBuildArgsAndTags(t *testing.T) {
	body := dockerRecipe(t, dockerMakefile(t), "docker-build")
	for _, want := range []string{
		"--build-arg VERSION=$(VERSION)",
		"--build-arg COMMIT=$(COMMIT)",
		"--build-arg DATE=$(DATE)",
		"-t $(IMAGE):$(VERSION)",
		"-t $(IMAGE):latest",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("docker-build recipe missing %q\nrecipe: %s", want, body)
		}
	}
}

func TestDockerRunPublishesAndForwardsEnv(t *testing.T) {
	body := dockerRecipe(t, dockerMakefile(t), "docker-run")
	for _, want := range []string{
		"-p 8080:8080",
		"-e DATABASE_URL",
		"-e RPC_URL",
		"$(IMAGE):$(VERSION)",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("docker-run recipe missing %q\nrecipe: %s", want, body)
		}
	}
}
