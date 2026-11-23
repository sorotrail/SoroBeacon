package ci_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The security gates are configuration, so nothing else fails when one is
// quietly weakened: a dropped trigger, a rule removed from the include list,
// or a write permission that leaks to the whole workflow. These tests pin the
// properties SECURITY.md promises.

func readSecurityFile(t *testing.T, rel ...string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(append([]string{repoRoot(t)}, rel...)...))
	require.NoError(t, err)
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

func TestCodeQLWorkflow(t *testing.T) {
	wf := readSecurityFile(t, ".github", "workflows", "codeql.yml")

	// Pull requests and pushes to main are both required: without the
	// pull_request trigger findings never reach the pull request.
	require.Regexp(t, `(?m)^  pull_request:\s*$`, wf)
	require.Regexp(t, `(?m)^  push:\n    branches: \[main\]`, wf)

	// The workflow-level token stays read-only; only the analysis job gets
	// security-events: write, which is what uploading SARIF needs.
	require.Regexp(t, `(?m)^permissions:\n  contents: read\n`, wf)
	require.Contains(t, wf, "security-events: write")
	require.NotContains(t, wf, "secrets.", "the scan must not depend on secrets, or it breaks on fork pull requests")
	require.NotContains(t, wf, "pull_request_target", "pull_request_target would run fork code with a write token")

	// Every CodeQL action reference is pinned to a version, never a branch.
	refs := regexp.MustCompile(`github/codeql-action/[a-z-]+@(\S+)`).FindAllStringSubmatch(wf, -1)
	require.NotEmpty(t, refs)
	for _, ref := range refs {
		require.NotContains(t, []string{"main", "master", "latest"}, ref[1], "codeql-action must be pinned")
	}
	require.Contains(t, wf, "languages: go")
	require.Contains(t, wf, "config-file: ./.github/codeql/codeql-config.yml")
}

func TestCodeQLConfigUsesSecurityExtended(t *testing.T) {
	cfg := readSecurityFile(t, ".github", "codeql", "codeql-config.yml")
	// security-extended carries go/insecure-randomness on top of the default
	// SQL injection, request forgery and path injection queries.
	require.Contains(t, cfg, "uses: security-extended")
}

// requiredGosecRules are the rules issue #570 names: SQL built by formatting
// or concatenation, SSRF, path traversal, weak randomness, and unsafe.
var requiredGosecRules = []string{"G103", "G107", "G201", "G202", "G304", "G305", "G404"}

func TestGosecRuleSet(t *testing.T) {
	cfg := readSecurityFile(t, ".golangci-security.yml")
	require.Regexp(t, `(?m)^  default: none\s*$`, cfg, "the security run must be tuned, not every default rule")
	require.Regexp(t, `(?m)^    - gosec\s*$`, cfg)
	for _, rule := range requiredGosecRules {
		require.Regexp(t, `(?m)^\s+- `+rule+`\b`, cfg, "gosec rule %s must stay enabled", rule)
	}
	// Suppressions live next to the code they excuse, not in a central list.
	require.NotRegexp(t, `(?m)^\s+excludes:`, cfg)
	require.NotContains(t, cfg, "text: \"G", "rule-level exclusions belong inline, with a reason")
}

func TestGosecRuleSetFailsIfRuleDropped(t *testing.T) {
	cfg := readSecurityFile(t, ".golangci-security.yml")
	broken := regexp.MustCompile(`(?m)^\s+- G404.*\n`).ReplaceAllString(cfg, "")
	require.NotRegexp(t, `(?m)^\s+- G404\b`, broken, "removing a rule must be detectable by this check")
}

func TestSecurityJobRunsInCI(t *testing.T) {
	ci := readSecurityFile(t, ".github", "workflows", "ci.yml")
	job := requireJobBlock(t, ci, "security")
	require.Contains(t, job, "--config=.golangci-security.yml")
	require.Contains(t, job, "--config=../.golangci-security.yml", "the Terraform provider module must be scanned too")
	require.NotContains(t, job, "only-new-issues", "existing findings are triaged inline, so every finding fails")
}

// TestGosecSuppressionsCarryAReason keeps the baseline honest: a bare
// #nosec directive is a blanket ignore, which issue #570 rules out.
func TestGosecSuppressionsCarryAReason(t *testing.T) {
	root := repoRoot(t)
	withReason := regexp.MustCompile(`//\s*(#nosec\s+G\d{3}\s+(--|//)\s+\S|nolint:gosec\s+//\s+\S)`)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "bin":
				return filepath.SkipDir
			}
			return nil
		}
		// Test files are not scanned (tests: false), so a suppression there
		// would excuse nothing.
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if !strings.Contains(line, "nolint:gosec") && !strings.Contains(line, "#nosec") {
				continue
			}
			trimmed := strings.TrimRight(line, "\r")
			if !withReason.MatchString(trimmed) {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s:%d: gosec suppression must carry an inline reason", rel, i+1)
			}
		}
		return nil
	})
	require.NoError(t, err)
}
