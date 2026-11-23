package docs

import (
	"strings"
	"testing"
)

// TestRetentionPageDocumentsOperatorRunbook pins the parts of the retention
// contract an operator cannot safely discover from the source: that deletion is
// permanent, that the default partition prevents an insert outage, how the
// partition window is maintained, what to monitor, and how the composite key
// changes future schema work. The page is the runbook an operator reaches for
// before shortening a retention window, so losing any of these sections is a
// regression even though no Go code changed.
func TestRetentionPageDocumentsOperatorRunbook(t *testing.T) {
	root := configRepoRoot(t)
	page := configRead(t, root, "docs/operations/retention.md")

	for _, want := range []string{
		"permanently deletes alert history",
		"ALERT_RETENTION",
		"ARCHIVE_URL",
		"alerts_default",
		"does not cause an alert-insert outage",
		"ensure alert partitions failed",
		"drop expired alert partitions failed",
		"no dedicated Prometheus metric",
		"current month plus the next two months",
		"(id, created_at)",
		"composite foreign key",
		"pg_dump",
		"no built-in import command",
		"1000",
		"once an hour",
		"ACCESS EXCLUSIVE",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("retention.md does not document %q", want)
		}
	}

	// The combined reorg page is the page operators already know; it must point
	// at the detailed runbook rather than leaving the new page undiscoverable.
	combined := configRead(t, root, "docs/operations/retention-and-reorg.md")
	if !strings.Contains(combined, "[Alert retention and partitioning](retention.md)") {
		t.Error("retention-and-reorg.md does not link the retention runbook")
	}
}
