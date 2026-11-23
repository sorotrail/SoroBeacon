package ci_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// These checks guard the wiring that keeps terraform-provider-sorobeacon/ a
// first-class citizen of the build. The provider is a nested module: the root
// module's `./...` stops at a subdirectory that carries its own go.mod, by
// design, so nothing in the server build — no compile, no vet, no lint, no
// dependabot — reaches it. Every assertion here is about the root repo
// noticing that. Without them the provider would be seven Go files shipping
// unchecked, which is the state this was written to end.

const providerDir = "terraform-provider-sorobeacon"

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(rel)))
	require.NoError(t, err, "read %s", rel)
	return string(raw)
}

// yamlJobBlock returns the text of one top-level job in a GitHub Actions
// workflow: the line "  <name>:" through to the next line that starts a
// sibling key. Working on the text rather than a decoded YAML value keeps the
// assertion readable and keeps a workflow this repository does not fully
// model from failing to unmarshal for unrelated reasons.
func yamlJobBlock(workflow, job string) (string, bool) {
	lines := strings.Split(workflow, "\n")
	start := -1
	for i, line := range lines {
		if line == "  "+job+":" {
			start = i
			break
		}
	}
	if start < 0 {
		return "", false
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		// A sibling job or a top-level key ("permissions:", "jobs:") ends the
		// block; both are written at two-space indentation or less.
		if line := lines[i]; line != "" && !strings.HasPrefix(line, "    ") {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n"), true
}

// requireJobBlock is yamlJobBlock with a failure instead of a false, so the
// assertions below read as one line each.
func requireJobBlock(t *testing.T, workflow, job string) string {
	t.Helper()
	block, ok := yamlJobBlock(workflow, job)
	require.True(t, ok, "workflow has no %q job", job)
	require.Contains(t, block, job, "extracted block must not be empty")
	return block
}

// makeRecipe returns the recipe lines under a Makefile target: every line
// after "<target>:" that starts with a tab.
func makeRecipe(t *testing.T, makefile, target string) string {
	t.Helper()
	lines := strings.Split(makefile, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != target+":" {
			continue
		}
		var b strings.Builder
		for _, next := range lines[i+1:] {
			if !strings.HasPrefix(next, "\t") {
				break
			}
			b.WriteString(next)
			b.WriteString("\n")
		}
		return b.String()
	}
	t.Fatalf("Makefile has no %q target", target)
	return ""
}

// goDirective returns the version on the `go` line of a go.mod file.
func goDirective(t *testing.T, path string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^go\s+(\S+)\s*$`)
	m := re.FindStringSubmatch(readRepoFile(t, path))
	require.Len(t, m, 2, "%s has no go directive", path)
	return m[1]
}

// TestTerraformProviderJobInCI is the guard that matters most: a pull request
// that breaks the provider must go red. A job that ran in the root directory,
// or that forgot a working-directory, would pass while checking nothing — so
// every assertion is on a specific input rather than on the job's name.
func TestTerraformProviderJobInCI(t *testing.T) {
	job := requireJobBlock(t, readRepoFile(t, ".github/workflows/ci.yml"), "terraform-provider")

	require.Contains(t, job, "working-directory: "+providerDir,
		"the provider is a nested module; commands run from the root check nothing")
	require.Contains(t, job, "cache-dependency-path: "+providerDir+"/go.sum",
		"cache on the provider's go.sum, or every run downloads its dependency tree")
	require.Contains(t, job, "go-version-file: "+providerDir+"/go.mod")
	for _, cmd := range []string{"go build ./...", "go vet ./...", "go test ./...", "go mod tidy"} {
		require.Contains(t, job, cmd)
	}
	require.Contains(t, job, "git diff --exit-code",
		"tidy must be verified, not merely run — the root module's build cannot see this go.mod")
	require.Contains(t, job, "golangci-lint-action",
		"lint the provider too; it has different idioms from the server")
	require.Contains(t, job, "working-directory: "+providerDir+"\n",
		"the lint action needs the module directory too")
}

// TestTerraformProviderJobIsWiredAndFailsOnRegressions proves the extraction
// above has teeth: drop the job and the checks must stop finding it. A guard
// that passes on an empty string is worse than no guard.
func TestTerraformProviderJobIsWiredAndFailsOnRegressions(t *testing.T) {
	workflow := readRepoFile(t, ".github/workflows/ci.yml")
	require.Contains(t, requireJobBlock(t, workflow, "terraform-provider"), "go build ./...")

	broken := strings.ReplaceAll(workflow, "  terraform-provider:\n", "  removed-provider:\n")
	require.NotContains(t, broken, "  terraform-provider:",
		"the fixture must actually have removed the job")
	_, found := yamlJobBlock(broken, "terraform-provider")
	require.False(t, found, "a removed job must not extract, or every assertion above is vacuous")
}

// TestTerraformProviderMakeTargets keeps the local reproduction of the CI job
// honest: each target exists and runs inside the module, since a recipe that
// stayed in the root directory would silently build nothing while exiting 0.
func TestTerraformProviderMakeTargets(t *testing.T) {
	makefile := readRepoFile(t, "Makefile")
	for target, want := range map[string]string{
		"build-tf": "go build",
		"test-tf":  "go test",
		"lint-tf":  "golangci-lint run",
		"tidy-tf":  "go mod tidy",
	} {
		recipe := makeRecipe(t, makefile, target)
		require.Contains(t, recipe, want, "make %s must run %s", target, want)
		require.Contains(t, recipe, "$(TF_DIR)", "make %s must cd into the module", target)
	}
}

// TestGoDirectivesMatch pins the two modules to the same language version.
// They were 1.25 and 1.26 for a while, which is harmless until it is not: the
// provider then compiles against a language the server does not, and a fix
// that needs a newer directive is harder to reason about than the drift.
func TestGoDirectivesMatch(t *testing.T) {
	require.Equal(t, goDirective(t, "go.mod"), goDirective(t, providerDir+"/go.mod"),
		"the two modules must declare the same go directive")
}

// TestTerraformProviderHasOwnLintConfig checks the decision this change made
// explicit: the provider pins its own .golangci.yml rather than relying on
// golangci-lint walking up into the repository root's. The file is currently a
// copy of the root's linter set; the point is that it exists, so the rules
// gating this module can diverge from the server's without touching either.
func TestTerraformProviderHasOwnLintConfig(t *testing.T) {
	path := filepath.Join(repoRoot(t), providerDir, ".golangci.yml")
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "the provider must pin its own golangci-lint config")
	require.Contains(t, string(raw), `version: "2"`)
}

// TestDependabotCoversTerraformProvider stops the dependency tree going quiet.
// A gomod entry for "/" does not cover a nested module, so without this entry
// the plugin framework and its transitive deps never receive a patch.
func TestDependabotCoversTerraformProvider(t *testing.T) {
	updates := splitUpdates(readRepoFile(t, ".github/dependabot.yml"))

	var provider []string
	for _, u := range updates {
		if strings.Contains(u, "package-ecosystem: gomod") &&
			strings.Contains(u, "directory: /"+providerDir) {
			provider = append(provider, u)
		}
	}
	require.Len(t, provider, 1, "exactly one gomod entry must cover the provider module")
	block := provider[0]
	require.Regexp(t, `(?m)^\s*interval:\s*weekly\s*$`, block)
	require.Regexp(t, `(?m)^\s*open-pull-requests-limit:\s*5\s*$`, block)
	require.Regexp(t, `(?m)^\s*prefix:\s*"chore\(deps\)"\s*$`, block)
	require.Contains(t, block, "go-minor-and-patch", "routine bumps stay grouped, as at the root")
	require.NotContains(t, block, "major", "major updates stay ungrouped, as at the root")
}