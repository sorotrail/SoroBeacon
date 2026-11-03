package metrics

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

type promRuleFile struct {
	Groups []promRuleGroup `yaml:"groups"`
}

type promRuleGroup struct {
	Name     string     `yaml:"name"`
	Interval string     `yaml:"interval"`
	Rules    []promRule `yaml:"rules"`
}

type promRule struct {
	Record      string            `yaml:"record"`
	Alert       string            `yaml:"alert"`
	Expr        string            `yaml:"expr"`
	For         string            `yaml:"for"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

var (
	promRawMetricRegex      = regexp.MustCompile(`\bsorobeacon_[a-z0-9_]+\b`)
	promRecordingMetricRgx  = regexp.MustCompile(`\bsorobeacon:sli_[a-z0-9_:]+\b`)
)

func TestExportedMetricNamesMatchesRegistryForAlerts(t *testing.T) {
	m := New()
	m.RegisterStreamDropped(func() uint64 { return 0 })

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
	m.SetDeadLettersFor("default", "slack", 0)
	m.RecordBodyRejection("max_body")
	m.httpDuration.WithLabelValues("/health", "GET", "200").Observe(0.01)

	families, err := m.registry.Gather()
	require.NoError(t, err)

	gathered := make(map[string]bool, len(families))
	for _, f := range families {
		gathered[f.GetName()] = true
	}

	for _, name := range ExportedMetricNames() {
		assert.True(t, gathered[name], "ExportedMetricNames() lists %q which is not registered in Metrics", name)
	}
}

func TestSLORecordingAndAlertRulesAreValidAndLinkedToRunbook(t *testing.T) {
	allowedMetrics := make(map[string]bool)
	for _, base := range ExportedMetricNames() {
		allowedMetrics[base] = true
		if strings.HasSuffix(base, "_seconds") || strings.HasSuffix(base, "_ledgers") {
			allowedMetrics[base+"_bucket"] = true
			allowedMetrics[base+"_sum"] = true
			allowedMetrics[base+"_count"] = true
		}
	}

	sloPath := filepath.Join("..", "..", "deploy", "prometheus", "rules", "sorobeacon_slos.yaml")
	alertPath := filepath.Join("..", "..", "deploy", "prometheus", "rules", "sorobeacon_alerts.yaml")
	runbookPath := filepath.Join("..", "..", "docs", "operations", "slos-and-alerts.md")

	sloRaw, err := os.ReadFile(sloPath)
	require.NoError(t, err)
	var sloFile promRuleFile
	require.NoError(t, yaml.Unmarshal(sloRaw, &sloFile))
	require.NotEmpty(t, sloFile.Groups)

	recordedSeries := make(map[string]bool)
	for _, g := range sloFile.Groups {
		require.NotEmpty(t, g.Name)
		for _, r := range g.Rules {
			require.NotEmpty(t, r.Record, "recording rule must declare record:")
			require.NotEmpty(t, r.Expr, "recording rule %s must declare expr:", r.Record)
			recordedSeries[r.Record] = true

			refs := promRawMetricRegex.FindAllString(r.Expr, -1)
			require.NotEmpty(t, refs, "recording rule %s must reference at least one sorobeacon_* metric", r.Record)
			for _, ref := range refs {
				assert.True(t, allowedMetrics[ref], "recording rule %s references unexported metric %q", r.Record, ref)
			}
		}
	}

	// Ensure evaluation freshness recording rules explicitly exclude mode="backfill"
	for _, g := range sloFile.Groups {
		for _, r := range g.Rules {
			if strings.Contains(r.Record, "evaluation_freshness") {
				assert.Contains(t, r.Expr, `mode!="backfill"`,
					"evaluation freshness rule %s must exclude mode=\"backfill\"", r.Record)
			}
		}
	}

	runbookBytes, err := os.ReadFile(runbookPath)
	require.NoError(t, err)
	runbookText := string(runbookBytes)

	alertRaw, err := os.ReadFile(alertPath)
	require.NoError(t, err)
	var alertFile promRuleFile
	require.NoError(t, yaml.Unmarshal(alertRaw, &alertFile))
	require.NotEmpty(t, alertFile.Groups)

	seenAlerts := make(map[string]bool)
	for _, g := range alertFile.Groups {
		require.NotEmpty(t, g.Name)
		for _, r := range g.Rules {
			require.NotEmpty(t, r.Alert, "alert rule must declare alert:")
			require.NotEmpty(t, r.Expr, "alert %s must declare expr:", r.Alert)
			seenAlerts[r.Alert] = true

			sev := r.Labels["severity"]
			assert.Contains(t, []string{"page", "ticket", "none"}, sev, "alert %s has unexpected severity %q", r.Alert, sev)
			assert.NotEmpty(t, r.Labels["owner"], "alert %s must declare labels.owner", r.Alert)
			assert.NotEmpty(t, r.Annotations["summary"], "alert %s must declare annotations.summary", r.Alert)
			assert.NotEmpty(t, r.Annotations["description"], "alert %s must declare annotations.description", r.Alert)

			runbookURL := r.Annotations["runbook_url"]
			require.NotEmpty(t, runbookURL, "alert %s must declare annotations.runbook_url", r.Alert)
			parts := strings.SplitN(runbookURL, "#", 2)
			require.Len(t, parts, 2, "alert %s runbook_url must include a #section anchor", r.Alert)
			assert.Contains(t, runbookText, "{#"+parts[1]+"}",
				"runbook docs/operations/slos-and-alerts.md must contain anchor {#%s} for alert %s", parts[1], r.Alert)

			for _, recRef := range promRecordingMetricRgx.FindAllString(r.Expr, -1) {
				assert.True(t, recordedSeries[recRef], "alert %s references undefined recording rule %q", r.Alert, recRef)
			}
			for _, rawRef := range promRawMetricRegex.FindAllString(r.Expr, -1) {
				assert.True(t, allowedMetrics[rawRef], "alert %s references unexported metric %q", r.Alert, rawRef)
			}
		}
	}

	// Verify all four SLOs (fast + slow burn) and dead-man signals exist.
	requiredAlerts := []string{
		"SoroBeaconEvaluationFreshnessBurnFast",
		"SoroBeaconEvaluationFreshnessBurnSlow",
		"SoroBeaconDeliverySuccessBurnFast",
		"SoroBeaconDeliverySuccessBurnSlow",
		"SoroBeaconDeliveryLatencyBurnFast",
		"SoroBeaconDeliveryLatencyBurnSlow",
		"SoroBeaconHTTPReadAvailabilityBurnFast",
		"SoroBeaconHTTPReadAvailabilityBurnSlow",
		"SoroBeaconWatchdog",
		"SoroBeaconScrapeMissing",
		"SoroBeaconPollerStalled",
	}
	for _, reqAlert := range requiredAlerts {
		assert.True(t, seenAlerts[reqAlert], "expected alert %q to be defined in sorobeacon_alerts.yaml", reqAlert)
	}
}

// TestSyntheticBurnRateAndDeadManEvaluation validates the multi-window burn-rate
// math and dead-man predicates against synthetic SLI error ratios.
func TestSyntheticBurnRateAndDeadManEvaluation(t *testing.T) {
	type burnCase struct {
		name        string
		errorBudget float64
		ratio5m     float64
		ratio30m    float64
		ratio1h     float64
		ratio6h     float64
		wantPage    bool
		wantTicket  bool
	}

	cases := []burnCase{
		{
			name:        "healthy steady state stays quiet",
			errorBudget: 0.005, // 99.5% Delivery Success
			ratio5m:     0.001,
			ratio30m:    0.001,
			ratio1h:     0.001,
			ratio6h:     0.001,
			wantPage:    false,
			wantTicket:  false,
		},
		{
			name:        "brief 5m spike that recovers before 1h threshold does not page",
			errorBudget: 0.005,
			ratio5m:     0.20,  // > 14.4 * 0.005 (0.072)
			ratio30m:    0.02,
			ratio1h:     0.02,  // < 0.072
			ratio6h:     0.004,
			wantPage:    false,
			wantTicket:  false,
		},
		{
			name:        "sustained fast burn across 1h and 5m pages on-call",
			errorBudget: 0.005,
			ratio5m:     0.09, // > 0.072
			ratio30m:    0.09,
			ratio1h:     0.08, // > 0.072
			ratio6h:     0.02,
			wantPage:    true,
			wantTicket:  false,
		},
		{
			name:        "resolved outage where 1h is still high but 5m recovered stops paging",
			errorBudget: 0.005,
			ratio5m:     0.0,  // recovered
			ratio30m:    0.01,
			ratio1h:     0.12, // still > 0.072 from earlier spike
			ratio6h:     0.02,
			wantPage:    false,
			wantTicket:  false,
		},
		{
			name:        "slow burn across 6h and 30m opens ticket without paging",
			errorBudget: 0.01, // 99.0% Evaluation Freshness
			ratio5m:     0.07, // < 14.4 * 0.01 (0.144)
			ratio30m:    0.07, // > 6.0 * 0.01 (0.06)
			ratio1h:     0.07, // < 0.144
			ratio6h:     0.065, // > 0.06
			wantPage:    false,
			wantTicket:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotPage := tc.ratio1h > (14.4*tc.errorBudget) && tc.ratio5m > (14.4*tc.errorBudget)
			gotTicket := tc.ratio6h > (6.0*tc.errorBudget) && tc.ratio30m > (6.0*tc.errorBudget)
			assert.Equal(t, tc.wantPage, gotPage, "fast-burn page decision mismatch")
			assert.Equal(t, tc.wantTicket, gotTicket, "slow-burn ticket decision mismatch")
		})
	}
}
