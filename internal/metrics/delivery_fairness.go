package metrics

import (
	"strconv"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

type deliveryFairnessCollectors struct {
	workspaceInFlight   *prometheus.GaugeVec
	workspaceQueueDepth *prometheus.GaugeVec
	channelInFlight     *prometheus.GaugeVec
	channelQueueDepth   *prometheus.GaugeVec
	fairnessOverflow    *prometheus.CounterVec
}

var (
	deliveryFairnessMu         sync.Mutex
	deliveryFairnessByRegistry = make(map[*prometheus.Registry]*deliveryFairnessCollectors)
)

func (m *Metrics) deliveryFairnessSet() *deliveryFairnessCollectors {
	if m == nil || m.registry == nil {
		return nil
	}
	deliveryFairnessMu.Lock()
	defer deliveryFairnessMu.Unlock()
	if c, ok := deliveryFairnessByRegistry[m.registry]; ok {
		return c
	}
	c := &deliveryFairnessCollectors{
		workspaceInFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sorobeacon_notify_workspace_in_flight",
			Help: "Current number of notification delivery attempts in flight for a workspace, partitioned by lane (first_attempt|retry).",
		}, []string{"network", "workspace", "lane"}),
		workspaceQueueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sorobeacon_notify_workspace_queue_depth",
			Help: "Current number of notification delivery attempts waiting for a workspace slot, partitioned by lane (first_attempt|retry).",
		}, []string{"network", "workspace", "lane"}),
		channelInFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sorobeacon_notify_channel_in_flight",
			Help: "Current number of notification delivery attempts in flight for a channel.",
		}, []string{"network", "channel_id"}),
		channelQueueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sorobeacon_notify_channel_queue_depth",
			Help: "Current number of notification delivery attempts queued behind an active delivery on a channel.",
		}, []string{"network", "channel_id"}),
		fairnessOverflow: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sorobeacon_notify_fairness_overflow_total",
			Help: "Total notification deliveries shed to dead letters because a workspace or channel fairness queue bound was exceeded.",
		}, []string{"network", "workspace", "reason"}),
	}
	m.registry.MustRegister(
		c.workspaceInFlight,
		c.workspaceQueueDepth,
		c.channelInFlight,
		c.channelQueueDepth,
		c.fairnessOverflow,
	)
	deliveryFairnessByRegistry[m.registry] = c
	return c
}

func normalizeFairnessLane(lane string) string {
	switch strings.ToLower(strings.TrimSpace(lane)) {
	case "retry":
		return "retry"
	default:
		return "first_attempt"
	}
}

func normalizeFairnessOverflowReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "workspace_queue_full":
		return "workspace_queue_full"
	case "retry_queue_full":
		return "retry_queue_full"
	case "channel_queue_full":
		return "channel_queue_full"
	case "submit_queue_full":
		return "submit_queue_full"
	default:
		return "workspace_queue_full"
	}
}

func formatChannelIDLabel(channelID int64) string {
	if channelID <= 0 {
		return "0"
	}
	return strconv.FormatInt(channelID, 10)
}

// SetNotifyWorkspaceBacklog updates the in-flight and queue depth gauges for a workspace and delivery lane.
func (m *Metrics) SetNotifyWorkspaceBacklog(workspaceID, lane string, inFlight, queueDepth int) {
	c := m.deliveryFairnessSet()
	if c == nil {
		return
	}
	if inFlight < 0 {
		inFlight = 0
	}
	if queueDepth < 0 {
		queueDepth = 0
	}
	wsLabel := m.WorkspaceMetricLabel(workspaceID)
	laneLabel := normalizeFairnessLane(lane)
	c.workspaceInFlight.WithLabelValues(m.network, wsLabel, laneLabel).Set(float64(inFlight))
	c.workspaceQueueDepth.WithLabelValues(m.network, wsLabel, laneLabel).Set(float64(queueDepth))
}

// SetNotifyChannelBacklog updates the in-flight and queue depth gauges for a channel.
func (m *Metrics) SetNotifyChannelBacklog(channelID int64, inFlight, queueDepth int) {
	c := m.deliveryFairnessSet()
	if c == nil {
		return
	}
	if inFlight < 0 {
		inFlight = 0
	}
	if queueDepth < 0 {
		queueDepth = 0
	}
	chLabel := formatChannelIDLabel(channelID)
	c.channelInFlight.WithLabelValues(m.network, chLabel).Set(float64(inFlight))
	c.channelQueueDepth.WithLabelValues(m.network, chLabel).Set(float64(queueDepth))
}

// RecordNotifyFairnessOverflow increments the counter when a delivery attempt is shed due to queue saturation.
func (m *Metrics) RecordNotifyFairnessOverflow(workspaceID, reason string) {
	c := m.deliveryFairnessSet()
	if c == nil {
		return
	}
	wsLabel := m.WorkspaceMetricLabel(workspaceID)
	c.fairnessOverflow.WithLabelValues(m.network, wsLabel, normalizeFairnessOverflowReason(reason)).Inc()
}
