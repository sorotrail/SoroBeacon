// Package usage buffers per-workspace usage counters in memory and flushes
// aggregated hourly rollups to the store on a schedule rather than writing a
// database row on every request or event evaluation.
package usage

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// DefaultFlushInterval is how often buffered workspace usage is flushed to the store.
const DefaultFlushInterval = 30 * time.Second

type periodKey struct {
	workspaceID string
	periodStart time.Time
}

type notifKey struct {
	channelType string
	outcome     string
}

type periodAccum struct {
	periodEnd       time.Time
	alertsRaised    int64
	ruleEvaluations int64
	apiRequests     int64
	notifications   map[notifKey]int64
}

// Meter accumulates per-workspace usage in memory, updates Prometheus counters
// immediately, and flushes durable period rollups into the store in batches.
type Meter struct {
	store   store.Usage
	metrics *metrics.Metrics
	log     *slog.Logger
	now     func() time.Time

	mu      sync.Mutex
	pending map[periodKey]*periodAccum
}

// NewMeter constructs a workspace usage meter backed by st and m. Both st and m
// may be nil in unit tests; nil methods are safe no-ops.
func NewMeter(st store.Usage, m *metrics.Metrics, log *slog.Logger) *Meter {
	if log == nil {
		log = slog.Default()
	}
	return &Meter{
		store:   st,
		metrics: m,
		log:     log,
		now:     time.Now,
		pending: make(map[periodKey]*periodAccum),
	}
}

// WithMetrics attaches or replaces the Prometheus metrics sink.
func (m *Meter) WithMetrics(met *metrics.Metrics) *Meter {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	m.metrics = met
	m.mu.Unlock()
	return m
}

// WithNow overrides the clock used for period bucketing (used in tests).
func (m *Meter) WithNow(nowFn func() time.Time) *Meter {
	if m == nil || nowFn == nil {
		return m
	}
	m.mu.Lock()
	m.now = nowFn
	m.mu.Unlock()
	return m
}

func normalizeWorkspace(ws string) string {
	ws = strings.TrimSpace(ws)
	if ws == "" {
		return string(workspace.Default)
	}
	return ws
}

func (m *Meter) bucketLocked(ws string, at time.Time) *periodAccum {
	start, end := store.TruncateUsagePeriod(at)
	k := periodKey{workspaceID: ws, periodStart: start}
	acc, ok := m.pending[k]
	if !ok {
		acc = &periodAccum{
			periodEnd:     end,
			notifications: make(map[notifKey]int64),
		}
		m.pending[k] = acc
	}
	return acc
}

// RecordAlertRaised attributes n raised alerts to workspaceID.
func (m *Meter) RecordAlertRaised(workspaceID string, n int64) {
	if m == nil || n <= 0 {
		return
	}
	ws := normalizeWorkspace(workspaceID)
	m.mu.Lock()
	acc := m.bucketLocked(ws, m.now())
	acc.alertsRaised += n
	met := m.metrics
	m.mu.Unlock()

	met.RecordWorkspaceAlertRaised(ws, n)
}

// RecordRuleEvaluations attributes n rule evaluations to workspaceID.
func (m *Meter) RecordRuleEvaluations(workspaceID string, n int64) {
	if m == nil || n <= 0 {
		return
	}
	ws := normalizeWorkspace(workspaceID)
	m.mu.Lock()
	acc := m.bucketLocked(ws, m.now())
	acc.ruleEvaluations += n
	met := m.metrics
	m.mu.Unlock()

	met.RecordWorkspaceRuleEvaluations(ws, n)
}

// RecordNotification attributes n notification attempts for channelType and
// outcome (e.g. "ok", "error", "throttled") to workspaceID.
func (m *Meter) RecordNotification(workspaceID, channelType, outcome string, n int64) {
	if m == nil || n <= 0 {
		return
	}
	ws := normalizeWorkspace(workspaceID)
	chType := strings.TrimSpace(channelType)
	if chType == "" {
		chType = "unknown"
	}
	out := strings.TrimSpace(outcome)
	if out == "" {
		out = "unknown"
	}

	m.mu.Lock()
	acc := m.bucketLocked(ws, m.now())
	acc.notifications[notifKey{channelType: chType, outcome: out}] += n
	met := m.metrics
	m.mu.Unlock()

	met.RecordWorkspaceNotification(ws, chType, out, n)
}

// RecordAPIRequest attributes n authenticated HTTP API requests to workspaceID.
func (m *Meter) RecordAPIRequest(workspaceID string, n int64) {
	if m == nil || n <= 0 {
		return
	}
	ws := normalizeWorkspace(workspaceID)
	m.mu.Lock()
	acc := m.bucketLocked(ws, m.now())
	acc.apiRequests += n
	met := m.metrics
	m.mu.Unlock()

	met.RecordWorkspaceAPIRequest(ws, n)
}

// ObserveStorageCounts updates the Prometheus stored-row gauges for workspaceID.
func (m *Meter) ObserveStorageCounts(workspaceID string, counts store.WorkspaceStorageCounts) {
	if m == nil {
		return
	}
	ws := normalizeWorkspace(workspaceID)
	m.mu.Lock()
	met := m.metrics
	m.mu.Unlock()
	if met == nil {
		return
	}
	met.SetWorkspaceStoredRows(ws, "monitors", counts.Monitors)
	met.SetWorkspaceStoredRows(ws, "rules", counts.Rules)
	met.SetWorkspaceStoredRows(ws, "channels", counts.Channels)
	met.SetWorkspaceStoredRows(ws, "alerts", counts.Alerts)
	met.SetWorkspaceStoredRows(ws, "delivery_attempts", counts.DeliveryAttempts)
	met.SetWorkspaceStoredRows(ws, "dead_letters", counts.DeadLetters)
}

// Flush atomically drains buffered usage deltas and persists them to the store.
// If the store write fails, the drained deltas are merged back into memory so
// no usage is lost across transient database errors.
func (m *Meter) Flush(ctx context.Context) error {
	if m == nil || m.store == nil {
		return nil
	}

	m.mu.Lock()
	if len(m.pending) == 0 {
		m.mu.Unlock()
		return nil
	}
	snapshot := m.pending
	m.pending = make(map[periodKey]*periodAccum)
	m.mu.Unlock()

	deltas := make([]store.WorkspaceUsageDelta, 0, len(snapshot))
	for k, acc := range snapshot {
		notifs := make([]store.WorkspaceNotificationUsage, 0, len(acc.notifications))
		for nk, count := range acc.notifications {
			if count <= 0 {
				continue
			}
			notifs = append(notifs, store.WorkspaceNotificationUsage{
				ChannelType: nk.channelType,
				Outcome:     nk.outcome,
				Count:       count,
			})
		}
		sort.Slice(notifs, func(i, j int) bool {
			if notifs[i].ChannelType != notifs[j].ChannelType {
				return notifs[i].ChannelType < notifs[j].ChannelType
			}
			return notifs[i].Outcome < notifs[j].Outcome
		})
		deltas = append(deltas, store.WorkspaceUsageDelta{
			WorkspaceID:     k.workspaceID,
			PeriodStart:     k.periodStart,
			PeriodEnd:       acc.periodEnd,
			AlertsRaised:    acc.alertsRaised,
			RuleEvaluations: acc.ruleEvaluations,
			APIRequests:     acc.apiRequests,
			Notifications:   notifs,
		})
	}
	sort.Slice(deltas, func(i, j int) bool {
		if deltas[i].WorkspaceID != deltas[j].WorkspaceID {
			return deltas[i].WorkspaceID < deltas[j].WorkspaceID
		}
		return deltas[i].PeriodStart.Before(deltas[j].PeriodStart)
	})

	sysCtx := workspace.WithSystem(ctx)
	if err := store.SafeRecordWorkspaceUsageBatch(sysCtx, m.store, deltas); err != nil {
		// Restore drained snapshot back into pending so a transient database
		// error never drops usage counts.
		m.mu.Lock()
		for k, acc := range snapshot {
			cur, ok := m.pending[k]
			if !ok {
				m.pending[k] = acc
				continue
			}
			cur.alertsRaised += acc.alertsRaised
			cur.ruleEvaluations += acc.ruleEvaluations
			cur.apiRequests += acc.apiRequests
			for nk, c := range acc.notifications {
				cur.notifications[nk] += c
			}
		}
		m.mu.Unlock()
		return err
	}
	return nil
}

// Start runs a periodic flush loop until ctx is cancelled, performing one final
// flush before returning.
func (m *Meter) Start(ctx context.Context, interval time.Duration) {
	if m == nil {
		return
	}
	if interval <= 0 {
		interval = DefaultFlushInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := m.Flush(flushCtx); err != nil && m.log != nil {
				m.log.Warn("final workspace usage flush failed", "err", err)
			}
			cancel()
			return
		case <-ticker.C:
			if err := m.Flush(ctx); err != nil && m.log != nil {
				m.log.Warn("periodic workspace usage flush failed", "err", err)
			}
		}
	}
}
