package docs

import (
	"strings"
	"testing"
)

// TestDeliveryFailuresDocumentsDeadLetters pins the recovery path operators
// need: the dead-letter queue, redrive on both surfaces, and the metrics
// reference. delivery-failures.md is the page an operator reads when a
// delivery dies, so losing the queue section would make the feature
// undiscoverable again.
func TestDeliveryFailuresDocumentsDeadLetters(t *testing.T) {
	root := configRepoRoot(t)
	page := configRead(t, root, "docs/operations/delivery-failures.md")

	for _, want := range []string{
		"## Dead-Letter Queue and Redrive",
		"GET /api/v1/dead-letters",
		"POST /api/v1/dead-letters/{id}/redrive",
		"../reference/metrics.md#the-metrics",
		"Known difference between the API and dashboard redrive paths",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("delivery-failures.md is missing %q", want)
		}
	}

	metrics := configRead(t, root, "docs/reference/metrics.md")
	if !strings.Contains(metrics, "sorobeacon_dead_letters_total") {
		t.Error("metrics.md does not document sorobeacon_dead_letters_total")
	}

	api := configRead(t, root, "docs/reference/api.md")
	for _, want := range []string{
		"## Dead letters",
		"GET /dead-letters",
		"POST /dead-letters/{id}/redrive",
	} {
		if !strings.Contains(api, want) {
			t.Errorf("api.md is missing %q", want)
		}
	}

	// The prose links point at anchors, not just files; keep them honest.
	anchors := []struct {
		file   string
		anchor string
	}{
		{"docs/reference/metrics.md", "the-metrics"},
		{"docs/operations/delivery-failures.md", "known-difference-between-the-api-and-dashboard-redrive-paths"},
	}
	for _, a := range anchors {
		if !headingSlugs(configRead(t, root, a.file))[a.anchor] {
			t.Errorf("%s is missing heading anchor #%s", a.file, a.anchor)
		}
	}
}
