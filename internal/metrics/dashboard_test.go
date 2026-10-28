package metrics

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type grafanaDashboard struct {
	UID         string              `json:"uid"`
	Title       string              `json:"title"`
	Description string              `json:"description"`
	Links       []grafanaLink       `json:"links"`
	Templating  grafanaTemplating   `json:"templating"`
	Panels      []grafanaPanel      `json:"panels"`
}

type grafanaLink struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

type grafanaTemplating struct {
	List []grafanaTemplateVar `json:"list"`
}

type grafanaTemplateVar struct {
	Name       string             `json:"name"`
	Query      string             `json:"query"`
	IncludeAll bool               `json:"includeAll"`
	Current    grafanaVarSelected `json:"current"`
}

type grafanaVarSelected struct {
	Value string `json:"value"`
}

type grafanaPanel struct {
	ID          int             `json:"id"`
	Title       string          `json:"title"`
	Type        string          `json:"type"`
	Description string          `json:"description"`
	Targets     []grafanaTarget `json:"targets"`
}

type grafanaTarget struct {
	RefID string `json:"refId"`
	Expr  string `json:"expr"`
}

var promMetricRefPattern = regexp.MustCompile(`\bsorobeacon_[a-z0-9_]+\b`)

// TestExportedMetricNamesMatchesRegistry verifies that ExportedMetricNames()
// stays in exact 1-to-1 sync with the metrics actually registered on Metrics.
func TestExportedMetricNamesMatchesRegistry(t *testing.T) {
	m := New()
	m.RegisterStreamDropped(func() uint64 { return 0 })

	// Touch each vector once so Gather() emits every registered family.
	m.RecordPoll(true, 10*time.Millisecond)
	m.SetPollLag(1)
	m.SetPriorityContracts("high", 1)
	m.SetPollLagByPriority("high", 1)
	m.RecordReorg(1)
	m.RecordEvents(1, 1)
	m.RecordRuleEvaluations(1)
	m.RecordRuleEval("threshold", true, nil, 5*time.Millisecond)
	m.RecordRuleEval("threshold", false, errors.New("eval failed"), 5*time.Millisecond)
	m.RecordAlert()
	m.TickPollAge(1)
	m.RecordPanic()
	m.RecordDelivery("slack", true)
	m.RecordThrottle("slack")
	m.SetBreakerState("ch-1", "slack", "closed")
	m.RecordStoreRead("primary")
	m.RecordReplicaFallback()
	m.SetReplicaEnabled(true)
	m.SetDeadLetters(0)
	m.httpDuration.WithLabelValues("/health", "GET", "200").Observe(0.01)

	families, err := m.registry.Gather()
	require.NoError(t, err)

	gathered := make(map[string]bool, len(families))
	for _, f := range families {
		gathered[f.GetName()] = true
	}

	declared := make(map[string]bool, len(ExportedMetricNames()))
	for _, name := range ExportedMetricNames() {
		declared[name] = true
		assert.True(t, gathered[name], "ExportedMetricNames() lists %q which is not registered in Metrics", name)
	}

	for name := range gathered {
		assert.True(t, declared[name], "Metrics registers %q which is missing from ExportedMetricNames()", name)
	}
}

// TestDashboardsReferenceOnlyExportedMetrics parses every shipped Grafana
// dashboard JSON under deploy/grafana/dashboards and verifies that:
// 1. Every referenced sorobeacon_* metric exists in ExportedMetricNames().
// 2. Every panel is annotated with a description explaining what good looks like.
// 3. Template variables default to All ($__all) rather than a single slice.
func TestDashboardsReferenceOnlyExportedMetrics(t *testing.T) {
	allowed := allowedPromMetricNames()

	dashboardDir := filepath.Join("..", "..", "deploy", "grafana", "dashboards")
	entries, err := os.ReadDir(dashboardDir)
	require.NoError(t, err, "deploy/grafana/dashboards directory must exist")

	var jsonFiles []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			jsonFiles = append(jsonFiles, filepath.Join(dashboardDir, entry.Name()))
		}
	}
	require.GreaterOrEqual(t, len(jsonFiles), 5, "expected overview, evaluation, delivery, channels, and store dashboards")

	for _, path := range jsonFiles {
		raw, readErr := os.ReadFile(path)
		require.NoError(t, readErr)

		var dash grafanaDashboard
		require.NoError(t, json.Unmarshal(raw, &dash), "invalid JSON in %s", path)

		assert.NotEmpty(t, dash.UID, "%s must define a stable uid", path)
		assert.NotEmpty(t, dash.Title, "%s must define a title", path)
		assert.NotEmpty(t, dash.Description, "%s must define a description", path)
		assert.NotEmpty(t, dash.Links, "%s must link back to runbooks or sibling views", path)
		assert.NotEmpty(t, dash.Panels, "%s must contain panels", path)
		assert.LessOrEqual(t, len(dash.Panels), 12, "%s should fit on one screen without being a 40-panel data dump", path)

		for _, tv := range dash.Templating.List {
			assert.True(t, tv.IncludeAll, "%s variable %q must allow All", path, tv.Name)
			assert.Equal(t, "$__all", tv.Current.Value, "%s variable %q must default to $__all", path, tv.Name)
			for _, ref := range promMetricRefPattern.FindAllString(tv.Query, -1) {
				assert.True(t, allowed[ref], "%s variable %q references unknown metric %q", path, tv.Name, ref)
			}
		}

		for _, panel := range dash.Panels {
			assert.NotEmpty(t, panel.Title, "%s panel %d missing title", path, panel.ID)
			assert.NotEmpty(t, panel.Description, "%s panel %q must be annotated with what good looks like", path, panel.Title)
			assert.NotEmpty(t, panel.Targets, "%s panel %q has no query targets", path, panel.Title)

			for _, target := range panel.Targets {
				assert.NotEmpty(t, target.Expr, "%s panel %q target %s has empty expr", path, panel.Title, target.RefID)
				refs := promMetricRefPattern.FindAllString(target.Expr, -1)
				for _, ref := range refs {
					assert.True(t, allowed[ref], "%s panel %q references non-existent metric %q", path, panel.Title, ref)
				}
			}
		}
	}
}

func allowedPromMetricNames() map[string]bool {
	allowed := make(map[string]bool)
	for _, base := range ExportedMetricNames() {
		allowed[base] = true
		if strings.HasSuffix(base, "_seconds") {
			allowed[base+"_bucket"] = true
			allowed[base+"_sum"] = true
			allowed[base+"_count"] = true
		}
	}
	return allowed
}
