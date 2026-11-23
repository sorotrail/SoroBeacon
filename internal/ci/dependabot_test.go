package ci_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
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
	t.Fatal("could not find repo root (go.mod)")
	return ""
}

func TestDependabotConfig(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "dependabot.yml"))
	require.NoError(t, err)
	text := string(raw)

	require.Regexp(t, `(?m)^version:\s*2\s*$`, text)

	updates := splitUpdates(text)
	// Root gomod, terraform-provider gomod, github-actions, and docker: the Terraform provider is a
	// nested module, and a gomod entry for "/" does not reach it.
	require.Len(t, updates, 4, "want gomod (root), gomod (terraform provider), github-actions and docker")

	gomodRoot := updates[0]
	gomodTF := updates[1]
	actions := findUpdate(t, updates, "github-actions")
	docker := findUpdate(t, updates, "docker")

	assertWeeklyRoot(t, gomodRoot)
	assertWeeklyRoot(t, gomodTF)
	assertWeeklyRoot(t, actions)
	assertWeeklyRoot(t, docker)

	require.Contains(t, gomodRoot, "go-minor-and-patch", "minor/patch Go bumps should be grouped")
	require.Contains(t, gomodRoot, "minor")
	require.Contains(t, gomodRoot, "patch")
	require.NotContains(t, gomodRoot, "major", "major Go updates must stay ungrouped")
	require.NotContains(t, actions, "go-minor-and-patch")

}

func TestDependabotConfigFailsIfEcosystemDropped(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "dependabot.yml"))
	require.NoError(t, err)
	// Every gomod entry: the provider module has one of its own, so replacing
	// only the first would leave a survivor and the check would pass on a file
	// that still watches the provider.
	broken := strings.ReplaceAll(string(raw), "package-ecosystem: gomod", "package-ecosystem: removed")
	updates := splitUpdates(broken)
	_, found := lookupUpdate(updates, "gomod")
	require.False(t, found, "dropping the gomod ecosystem must fail this check")
}

func assertWeeklyRoot(t *testing.T, block string) {
	t.Helper()
	require.Regexp(t, `(?m)^\s*directory:\s*/(\s*$|terraform-provider-sorobeacon)`, block)
	require.Regexp(t, `(?m)^\s*interval:\s*weekly\s*$`, block)
	require.Regexp(t, `(?m)^\s*open-pull-requests-limit:\s*5\s*$`, block)
	require.Regexp(t, `(?m)^\s*prefix:\s*"chore\(deps\)"\s*$`, block)
}

func splitUpdates(text string) []string {
	re := regexp.MustCompile(`(?m)^  - package-ecosystem:`)
	idxs := re.FindAllStringIndex(text, -1)
	if len(idxs) == 0 {
		return nil
	}
	out := make([]string, 0, len(idxs))
	for i, idx := range idxs {
		end := len(text)
		if i+1 < len(idxs) {
			end = idxs[i+1][0]
		}
		out = append(out, text[idx[0]:end])
	}
	return out
}

func findUpdate(t *testing.T, updates []string, ecosystem string) string {
	t.Helper()
	block, ok := lookupUpdate(updates, ecosystem)
	require.True(t, ok, "missing package-ecosystem %s", ecosystem)
	return block
}

func lookupUpdate(updates []string, ecosystem string) (string, bool) {
	want := "package-ecosystem: " + ecosystem
	for _, u := range updates {
		if strings.Contains(u, want) {
			return u, true
		}
	}
	return "", false
}
