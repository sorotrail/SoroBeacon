package metrics

import (
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

type quotaCollectors struct {
	quotaLimit           *prometheus.GaugeVec
	quotaUsage           *prometheus.GaugeVec
	quotaRejections      *prometheus.CounterVec
	queryCostCharged     *prometheus.CounterVec
	queryBudgetRemaining *prometheus.GaugeVec
	queryBudgetThrottled *prometheus.CounterVec
}

var (
	quotaCollectorsMu sync.Mutex
	quotaByRegistry   = make(map[*prometheus.Registry]*quotaCollectors)
)

func (m *Metrics) quotaSet() *quotaCollectors {
	if m == nil || m.registry == nil {
		return nil
	}
	quotaCollectorsMu.Lock()
	defer quotaCollectorsMu.Unlock()
	if qc, ok := quotaByRegistry[m.registry]; ok {
		return qc
	}
	qc := &quotaCollectors{
		quotaLimit: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sorobeacon_workspace_quota_limit",
			Help: "Configured resource quota limit per workspace and resource (0 means unlimited).",
		}, []string{"workspace", "resource"}),
		quotaUsage: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sorobeacon_workspace_quota_usage",
			Help: "Current resource count or active subscriptions against workspace quota.",
		}, []string{"workspace", "resource"}),
		quotaRejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sorobeacon_workspace_quota_rejections_total",
			Help: "Resource creation or subscription requests rejected due to workspace quota exhaustion.",
		}, []string{"workspace", "resource"}),
		queryCostCharged: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sorobeacon_workspace_query_cost_charged_total",
			Help: "Total reconciled query cost units charged to a workspace across REST and GraphQL.",
		}, []string{"workspace", "surface"}),
		queryBudgetRemaining: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sorobeacon_workspace_query_budget_remaining",
			Help: "Remaining query cost budget tokens for a workspace.",
		}, []string{"workspace"}),
		queryBudgetThrottled: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sorobeacon_workspace_query_budget_throttled_total",
			Help: "REST and GraphQL queries throttled due to workspace query cost budget exhaustion.",
		}, []string{"workspace", "surface"}),
	}
	m.registry.MustRegister(
		qc.quotaLimit,
		qc.quotaUsage,
		qc.quotaRejections,
		qc.queryCostCharged,
		qc.queryBudgetRemaining,
		qc.queryBudgetThrottled,
	)
	quotaByRegistry[m.registry] = qc
	return qc
}

func normalizeQuotaSurface(surface string) string {
	switch strings.ToLower(strings.TrimSpace(surface)) {
	case "rest":
		return "rest"
	case "graphql":
		return "graphql"
	default:
		return "other"
	}
}

func normalizeQuotaResource(resource string) string {
	switch strings.ToLower(strings.TrimSpace(resource)) {
	case "monitors":
		return "monitors"
	case "rules":
		return "rules"
	case "channels":
		return "channels"
	case "subscriptions":
		return "subscriptions"
	case "query_cost_budget":
		return "query_cost_budget"
	default:
		return "other"
	}
}

// SetWorkspaceQuotaLimit sets sorobeacon_workspace_quota_limit{workspace, resource}.
func (m *Metrics) SetWorkspaceQuotaLimit(workspace, resource string, limit float64) {
	qc := m.quotaSet()
	if qc == nil || limit < 0 {
		return
	}
	ws := HashWorkspaceLabel(workspace)
	qc.quotaLimit.WithLabelValues(ws, normalizeQuotaResource(resource)).Set(limit)
}

// SetWorkspaceQuotaUsage sets sorobeacon_workspace_quota_usage{workspace, resource}.
func (m *Metrics) SetWorkspaceQuotaUsage(workspace, resource string, usage float64) {
	qc := m.quotaSet()
	if qc == nil || usage < 0 {
		return
	}
	ws := HashWorkspaceLabel(workspace)
	qc.quotaUsage.WithLabelValues(ws, normalizeQuotaResource(resource)).Set(usage)
}

// RecordWorkspaceQuotaRejection increments sorobeacon_workspace_quota_rejections_total{workspace, resource}.
func (m *Metrics) RecordWorkspaceQuotaRejection(workspace, resource string) {
	qc := m.quotaSet()
	if qc == nil {
		return
	}
	ws := HashWorkspaceLabel(workspace)
	qc.quotaRejections.WithLabelValues(ws, normalizeQuotaResource(resource)).Inc()
}

// RecordWorkspaceQueryCostCharged adds cost to sorobeacon_workspace_query_cost_charged_total{workspace, surface}.
func (m *Metrics) RecordWorkspaceQueryCostCharged(workspace, surface string, cost float64) {
	qc := m.quotaSet()
	if qc == nil || cost <= 0 {
		return
	}
	ws := HashWorkspaceLabel(workspace)
	qc.queryCostCharged.WithLabelValues(ws, normalizeQuotaSurface(surface)).Add(cost)
}

// SetWorkspaceQueryBudgetRemaining sets sorobeacon_workspace_query_budget_remaining{workspace}.
func (m *Metrics) SetWorkspaceQueryBudgetRemaining(workspace string, remaining float64) {
	qc := m.quotaSet()
	if qc == nil {
		return
	}
	if remaining < 0 {
		remaining = 0
	}
	ws := HashWorkspaceLabel(workspace)
	qc.queryBudgetRemaining.WithLabelValues(ws).Set(remaining)
}

// RecordWorkspaceQueryBudgetThrottled increments sorobeacon_workspace_query_budget_throttled_total{workspace, surface}.
func (m *Metrics) RecordWorkspaceQueryBudgetThrottled(workspace, surface string) {
	qc := m.quotaSet()
	if qc == nil {
		return
	}
	ws := HashWorkspaceLabel(workspace)
	qc.queryBudgetThrottled.WithLabelValues(ws, normalizeQuotaSurface(surface)).Inc()
}
