package docs

import (
	"strings"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/rules"
)

// TestScalingDocCoversEveryRuleTypeAndCapacityModel ensures that
// docs/operations/scaling.md documents the evaluation capacity model, the
// reproducible benchmark command, steady-state vs catch-up throughput, stage
// breakdown, and per-type cost for every rule type registered in NewRegistry().
func TestScalingDocCoversEveryRuleTypeAndCapacityModel(t *testing.T) {
	doc := readRepoFile(t, "docs/operations/scaling.md")

	for _, requiredSection := range []string{
		"## Evaluation capacity model",
		"### Per-rule-type evaluation cost",
		"### Pipeline stage breakdown and bottleneck analysis",
		"### Scaling across monitors, rules, and events",
		"### Steady-state vs catch-up throughput",
		"### Reproducing the measurements",
		"make bench",
	} {
		if !strings.Contains(doc, requiredSection) {
			t.Errorf("docs/operations/scaling.md is missing required capacity model section or command %q", requiredSection)
		}
	}

	reg := rules.NewRegistry()
	for _, rt := range reg.Types() {
		if !strings.Contains(doc, "`"+rt+"`") {
			t.Errorf("docs/operations/scaling.md does not document evaluation cost for registered rule type %q", rt)
		}
	}
}
