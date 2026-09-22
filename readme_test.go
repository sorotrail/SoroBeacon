package sorobeacon_test

import (
	"os"
	"strings"
	"testing"
)

// Guard the README badges and the verified quickstart so a later edit cannot
// drop the CI badge URL, the real clone path, or the health/dashboard checks
// without failing the suite.
func TestREADMEBadgesAndQuickstart(t *testing.T) {
	b, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := string(b)
	for _, s := range []string{
		"https://github.com/sorotrail/SoroBeacon/actions/workflows/ci.yml/badge.svg",
		"https://img.shields.io/github/license/sorotrail/SoroBeacon",
		"https://img.shields.io/github/go-mod/go-version/sorotrail/SoroBeacon",
		"git clone https://github.com/sorotrail/SoroBeacon.git",
		"cd SoroBeacon",
		"make up",
		"curl -sS http://localhost:8080/api/v1/health",
		"open http://localhost:8080",
	} {
		if !strings.Contains(readme, s) {
			t.Errorf("README.md missing %q", s)
		}
	}
	if strings.Contains(readme, "cd sorobeacon") {
		t.Error("clone directory is SoroBeacon, not sorobeacon")
	}
}
