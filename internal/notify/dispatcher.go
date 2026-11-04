package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/sorotrail/sorobeacon/internal/alerts"

	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/telemetry"
	"github.com/sorotrail/sorobeacon/internal/usage"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// Retry gate errors. The HTTP layer maps these onto status codes; the
// dashboard uses the same checks so a stray click cannot double-notify.
var (
	ErrAlreadySucceeded = errors.New("delivery already succeeded")
	ErrChannelDisabled  = errors.New("channel is disabled")
	ErrNoAttempt        = errors.New("no delivery attempt for this channel")
	ErrRetryCooldown    = errors.New("retried too recently")
)

// DefaultDigestFlushInterval is how often the dispatcher checks for digest
// windows that have elapsed. A window is closed within one interval of its
// expiry.
const DefaultDigestFlushInterval = 30 * time.Second

// DefaultRetryCooldown bounds how often an operator can re-send one
// alert to one channel. Long enough to stop a jammed button, short
// enough that a real "the webhook is fixed, try again" is not blocked.
const DefaultRetryCooldown = 30 * time.Second

// DispatchStore is everything the dispatcher needs: channel lookup and
// delivery bookkeeping, plus the escalation policy and per-alert scheduling
// state that keeps a tiered notification going across restarts.
type DispatchStore interface {
	ListChannelsForMonitor(ctx context.Context, monitorID int64) ([]store.Channel, error)
	ListChannelsByIDs(ctx context.Context, ids []int64) ([]store.Channel, error)
	RecordDeliveryAttempt(ctx context.Context, d *store.DeliveryAttempt) error
	CreateDeadLetter(ctx context.Context, d *store.DeadLetter) error
	GetAlert(ctx context.Context, id int64) (*store.Alert, error)
	GetChannel(ctx context.Context, id int64) (*store.Channel, error)
	ListDeliveryAttempts(ctx context.Context, alertID int64, status string) ([]store.DeliveryAttempt, error)
	DeleteDeadLetter(ctx context.Context, id int64) error
	// ListChannels is how the digest flusher discovers channels with a
	// window to close. Dispatch itself only needs ListChannelsForMonitor.
	ListChannels(ctx context.Context, enabledOnly bool) ([]store.Channel, error)
	// The inhibition trio backs the delivery-time suppression check
	// (alerts.InhibitedBy in internal/alerts): the pairs targeting the
	// alert's rule, whether a source is still firing, and the mark that
	// records why delivery stopped.
	ListInhibitionsForTarget(ctx context.Context, targetRuleID int64) ([]store.Inhibition, error)
	RuleFiredWithin(ctx context.Context, ruleID int64, window time.Duration) (bool, error)
	ListRuleAlertTimes(ctx context.Context, ruleID int64, since time.Time) ([]time.Time, error)
	MarkAlertInhibited(ctx context.Context, alertID, sourceRuleID int64) error
	// ActiveMaintenanceWindow reports a window silencing this alert's
	// monitor/contract at the given time, or nil when none is active.
	ActiveMaintenanceWindow(ctx context.Context, monitorID int64, contractID string, at time.Time) (*store.MaintenanceWindow, error)
	// SetAlertSuppressed records why an alert was not delivered.
	SetAlertSuppressed(ctx context.Context, alertID int64, reason string) error
	RecordChannelHealth(ctx context.Context, channelID int64, u store.ChannelHealthUpdate) error
	GetEscalationPolicyForMonitor(ctx context.Context, monitorID int64) (*store.EscalationPolicy, error)
	GetEscalationPolicy(ctx context.Context, policyID int64) (*store.EscalationPolicy, error)
	ScheduleEscalation(ctx context.Context, alertID, policyID int64, snapshot json.RawMessage, nextStep int, nextDue time.Time) error
	DueEscalations(ctx context.Context, now time.Time, limit int) ([]store.EscalationRun, error)
	AdvanceEscalation(ctx context.Context, alertID int64, nextStep int, nextDue time.Time) error
	CompleteEscalation(ctx context.Context, alertID int64) error
}

// Dispatcher fans an alert out to its monitor's channels, retrying each
// channel with exponential backoff and recording every attempt.
type Dispatcher struct {
	store   DispatchStore
	factory *Factory
	log     *slog.Logger
	// metrics is optional Prometheus instrumentation; nil-safe.
	metrics     *metrics.Metrics
	registry    *BreakerRegistry
	rateLimits  RateLimits
	rateLimiter *ChannelRateLimiter
	// telemetry is optional tracing; nil-safe. Delivery spans are started
	// from the alert's context, so they are children of the alert's span —
	// that parent chain, not any attribute, is what joins the delivery to
	// the poll cycle that produced it.
	telemetry *telemetry.Provider

	// MaxAttempts per channel (default 3) and BaseBackoff between attempts
	// (default 1s, doubled each retry: 1s, 2s, 4s...).
	MaxAttempts int
	BaseBackoff time.Duration

	// digest is optional. When attached, channels with digest mode
	// "window" accumulate alerts here and a summary is flushed once the
	// window elapses.
	deliveriesInFlight atomic.Int64
	digest             store.DigestQueue
	fairness           *FairnessScheduler
	// disableAfterFailures is how many consecutive permanent failures
	// auto-disable a channel. Zero — the default — never auto-disables:
	// silently switching off someone's alerting because a webhook 404'd
	// during an outage is a worse failure than the one it would fix, so an
	// operator has to opt in with CHANNEL_DISABLE_AFTER_FAILURES.
	disableAfterFailures int
	// EscalationInterval is how often RunEscalations looks for due steps
	// (default DefaultEscalationInterval). EscalationBatch bounds how many
	// due escalations one pass processes.
	EscalationInterval time.Duration
	EscalationBatch    int
	// now is the clock used for escalation bookkeeping. Nil means time.Now;
	// tests substitute it so a delayed step can be reached without sleeping.
	now           func() time.Time
	flapping      alerts.FlappingConfig
	monitorBudget *MonitorBudgetTracker
	usageMeter    *usage.Meter

	stopMu            sync.RWMutex
	stoppedWorkspaces map[string]struct{}
	stoppedChannels   map[int64]struct{}
	activeSeq         uint64
	activeDeliveries  map[uint64]activeDelivery
}

type activeDelivery struct {
	workspaceID string
	channelID   int64
	cancel      context.CancelFunc
}

// NewDispatcher wires a Dispatcher with default retry settings.
func NewDispatcher(s DispatchStore, f *Factory, log *slog.Logger) *Dispatcher {
	return &Dispatcher{
		store:              s,
		factory:            f,
		log:                log,
		registry:           NewBreakerRegistry(3, 30*time.Second),
		MaxAttempts:        3,
		BaseBackoff:        time.Second,
		rateLimits:         RateLimits{},
		rateLimiter:        NewRateLimiter(RateLimits{}),
		fairness:           NewFairnessScheduler(FairnessConfig{}, nil),
		EscalationInterval: DefaultEscalationInterval,
		EscalationBatch:    DefaultEscalationBatch,
		flapping: alerts.FlappingConfig{
			Episodes:   alerts.DefaultFlappingEpisodes,
			Window:     alerts.DefaultFlappingWindow,
			QuietAfter: alerts.DefaultFlappingQuietAfter,
		},
	}
}

// WithRateLimits applies operator overrides to the channel rate limiter. A
// zero field keeps the built-in default for that provider.
func (d *Dispatcher) WithRateLimits(r RateLimits) *Dispatcher {
	d.rateLimits = r
	d.rateLimiter = NewRateLimiter(r)
	return d
}

// RateLimits returns the configured overrides. Exposed for wiring tests.
func (d *Dispatcher) RateLimits() RateLimits {
	return d.rateLimits
}

// WithFlapping configures when repeated alert episodes are damped.
// WithMonitorBudget attaches a monitor notification budget tracker.
func (d *Dispatcher) WithMonitorBudget(tracker *MonitorBudgetTracker) *Dispatcher {
	d.monitorBudget = tracker
	return d
}

// WithMonitorCap sets a per-monitor notification volume cap.
func (d *Dispatcher) WithMonitorCap(cfg MonitorCapConfig) *Dispatcher {
	if d.monitorBudget == nil {
		d.monitorBudget = NewMonitorBudgetTracker(cfg)
	} else {
		d.monitorBudget.defaultCfg = cfg.Normalized()
	}
	return d
}

// MonitorBudget returns the attached MonitorBudgetTracker, if any.
func (d *Dispatcher) MonitorBudget() *MonitorBudgetTracker {
	return d.monitorBudget
}

func (d *Dispatcher) WithFlapping(config alerts.FlappingConfig) *Dispatcher {
	if config.Episodes > 0 {
		d.flapping.Episodes = config.Episodes
	}
	if config.Window > 0 {
		d.flapping.Window = config.Window
	}
	if config.QuietAfter > 0 {
		d.flapping.QuietAfter = config.QuietAfter
	}
	return d
}

// WithDisableAfterFailures sets how many consecutive permanent failures
// auto-disable a channel. Non-positive values leave auto-disable off, which is
// also the default.
func (d *Dispatcher) WithDisableAfterFailures(n int) *Dispatcher {
	if n > 0 {
		d.disableAfterFailures = n
	}
	return d
}

// WithClock replaces the clock escalation scheduling reads. Pass nil to
// restore time.Now. Tests use it to reach a delayed step without sleeping.
func (d *Dispatcher) WithClock(now func() time.Time) *Dispatcher {
	d.now = now
	return d
}

// nowTime is the dispatcher's clock, defaulting to the wall clock.
func (d *Dispatcher) nowTime() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

// DisableAfterFailures returns the configured threshold. Exposed for tests.
func (d *Dispatcher) DisableAfterFailures() int {
	return d.disableAfterFailures
}

// WithMetrics attaches delivery instrumentation.
func (d *Dispatcher) WithMetrics(m *metrics.Metrics) *Dispatcher {
	d.metrics = m
	if d.fairness != nil {
		d.fairness.setMetrics(m)
	}
	return d
}

// WithFairness configures per-workspace and per-channel concurrency bounds,
// retry lane isolation, and optional non-blocking async dispatch.
func (d *Dispatcher) WithFairness(cfg FairnessConfig) *Dispatcher {
	d.fairness = NewFairnessScheduler(cfg, d.metrics)
	return d
}

// Fairness returns the active fairness configuration.
func (d *Dispatcher) Fairness() FairnessConfig {
	if d.fairness == nil {
		return FairnessConfig{}.Normalized()
	}
	return d.fairness.cfg
}

// Backlog returns a point-in-time snapshot of all workspace and channel queues.
func (d *Dispatcher) Backlog() FairnessSnapshot {
	if d.fairness == nil {
		return FairnessSnapshot{
			Workspaces: map[string]WorkspaceBacklog{},
			Channels:   map[int64]ChannelBacklog{},
		}
	}
	return d.fairness.Snapshot()
}

// WorkspaceBacklog returns the live in-flight and queued delivery counts for one workspace.
func (d *Dispatcher) WorkspaceBacklog(workspaceID string) WorkspaceBacklog {
	if d.fairness == nil {
		return WorkspaceBacklog{WorkspaceID: normalizeWorkspaceID(workspaceID)}
	}
	return d.fairness.WorkspaceBacklog(workspaceID)
}

// ChannelBacklog returns the live in-flight and queued delivery counts for one channel.
func (d *Dispatcher) ChannelBacklog(channelID int64) ChannelBacklog {
	if d.fairness == nil {
		return ChannelBacklog{ChannelID: channelID}
	}
	return d.fairness.ChannelBacklog(channelID)
}

// InFlight returns the total number of channel deliveries currently executing.
func (d *Dispatcher) InFlight() int {
	return int(d.deliveriesInFlight.Load())
}

// Submit enqueues an alert onto its workspace's bounded delivery queue and
// returns immediately so ingestion and rule evaluation never block on
// downstream notification channels. When the workspace's queue is saturated,
// the excess alert fails predictably into the dead-letter and health-recording
// paths and Submit returns false.
func (d *Dispatcher) Submit(ctx context.Context, a Alert) bool {
	if d.fairness == nil {
		d.fairness = NewFairnessScheduler(FairnessConfig{}, d.metrics)
	}
	wsID := d.resolveAlertWorkspace(ctx, a)
	if d.isWorkspaceStopped(wsID) {
		return false
	}
	if err := d.fairness.Submit(ctx, wsID, a, d.dispatchSync); err != nil {
		d.shedAlertOnOverflow(ctx, a, wsID, err)
		return false
	}
	return true
}

// StopWorkspace immediately halts all queued and in-flight deliveries for wsID
// and channelIDs, cancels active delivery contexts, and removes per-channel
// circuit breakers and rate limiters. It returns the number of in-flight
// deliveries cancelled and queued submissions dropped.
func (d *Dispatcher) StopWorkspace(wsID string, channelIDs []int64) (cancelledInFlight, droppedQueued int) {
	if d == nil {
		return 0, 0
	}
	ws := normalizeWorkspaceID(wsID)
	chSet := make(map[int64]struct{}, len(channelIDs))
	for _, id := range channelIDs {
		chSet[id] = struct{}{}
	}

	var cancels []context.CancelFunc
	d.stopMu.Lock()
	if d.stoppedWorkspaces == nil {
		d.stoppedWorkspaces = make(map[string]struct{})
	}
	if d.stoppedChannels == nil {
		d.stoppedChannels = make(map[int64]struct{})
	}
	d.stoppedWorkspaces[ws] = struct{}{}
	for _, id := range channelIDs {
		d.stoppedChannels[id] = struct{}{}
	}
	for seq, ad := range d.activeDeliveries {
		_, matchCh := chSet[ad.channelID]
		if ad.workspaceID == ws || matchCh {
			cancels = append(cancels, ad.cancel)
			delete(d.activeDeliveries, seq)
		}
	}
	d.stopMu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
	if d.fairness != nil {
		droppedQueued = d.fairness.StopWorkspace(ws, channelIDs)
	}
	for _, id := range channelIDs {
		d.registry.Remove(id)
		d.rateLimiter.Remove(id)
	}
	if d.factory != nil {
		d.factory.InvalidateSecretCache()
	}
	return len(cancels), droppedQueued
}

// ResumeWorkspace clears the workspace-level stop flag after deletion completes
// so a workspace re-created with the same ID later can deliver to newly created
// channels while previously deleted channel IDs remain stopped.
func (d *Dispatcher) ResumeWorkspace(wsID string) {
	if d == nil {
		return
	}
	ws := normalizeWorkspaceID(wsID)
	d.stopMu.Lock()
	delete(d.stoppedWorkspaces, ws)
	d.stopMu.Unlock()
}

func (d *Dispatcher) isWorkspaceStopped(wsID string) bool {
	if d == nil {
		return false
	}
	ws := normalizeWorkspaceID(wsID)
	d.stopMu.RLock()
	_, stopped := d.stoppedWorkspaces[ws]
	d.stopMu.RUnlock()
	return stopped
}

func (d *Dispatcher) isChannelStopped(channelID int64) bool {
	if d == nil || channelID <= 0 {
		return false
	}
	d.stopMu.RLock()
	_, stopped := d.stoppedChannels[channelID]
	d.stopMu.RUnlock()
	return stopped
}

func (d *Dispatcher) beginActiveDelivery(ctx context.Context, wsID string, channelID int64) (context.Context, func(), bool) {
	ws := normalizeWorkspaceID(wsID)
	d.stopMu.Lock()
	if _, stopped := d.stoppedWorkspaces[ws]; stopped {
		d.stopMu.Unlock()
		return ctx, func() {}, false
	}
	if _, stopped := d.stoppedChannels[channelID]; stopped {
		d.stopMu.Unlock()
		return ctx, func() {}, false
	}
	delivCtx, cancel := context.WithCancel(ctx)
	d.activeSeq++
	seq := d.activeSeq
	if d.activeDeliveries == nil {
		d.activeDeliveries = make(map[uint64]activeDelivery)
	}
	d.activeDeliveries[seq] = activeDelivery{
		workspaceID: ws,
		channelID:   channelID,
		cancel:      cancel,
	}
	d.stopMu.Unlock()
	return delivCtx, func() {
		cancel()
		d.stopMu.Lock()
		delete(d.activeDeliveries, seq)
		d.stopMu.Unlock()
	}, true
}

// Wait blocks until all queued asynchronous deliveries have completed or ctx expires.
func (d *Dispatcher) Wait(ctx context.Context) error {
	if d.fairness == nil {
		return nil
	}
	return d.fairness.Wait(ctx)
}

func (d *Dispatcher) resolveAlertWorkspace(ctx context.Context, a Alert) string {
	if ws, ok := workspace.From(ctx); ok && !workspace.System(ctx) {
		if trimmed := strings.TrimSpace(string(ws)); trimmed != "" {
			return trimmed
		}
	}
	if d.store != nil && a.MonitorID > 0 {
		if channels, err := d.store.ListChannelsForMonitor(ctx, a.MonitorID); err == nil {
			for _, ch := range channels {
				if ws := strings.TrimSpace(ch.WorkspaceID); ws != "" {
					return ws
				}
			}
		}
	}
	return "default"
}

func (d *Dispatcher) shedAlertOnOverflow(ctx context.Context, a Alert, wsID string, overflowErr error) {
	if d.store == nil {
		return
	}
	channels, err := d.store.ListChannelsForMonitor(ctx, a.MonitorID)
	if err != nil {
		d.log.Error("list channels for overflow dead-letter", "alert_id", a.ID, "monitor_id", a.MonitorID, "err", err)
		return
	}
	safeErr := sanitizeDeliveryError(overflowErr)
	for _, ch := range channels {
		if !severityMeetsThreshold(a.Severity, ch.MinSeverity) {
			continue
		}
		if d.metrics != nil {
			d.metrics.RecordDelivery(ch.Type, false)
		}
		d.recordWorkspaceNotification(ctx, ch, "throttled")
		d.record(ctx, a.ID, ch.ID, "failed", safeErr.Error())
		d.recordHealth(ctx, ch.ID, store.ChannelHealthUpdate{Error: safeErr.Error(), Permanent: false})
		d.deadLetter(ctx, a, ch, overflowErr, 1)
		d.log.Warn("alert delivery shed due to workspace queue saturation",
			"alert_id", a.ID, "channel_id", ch.ID, "workspace", wsID, "err", safeErr)
	}
}

// WithUsageMeter attaches per-workspace usage metering to notification delivery.
func (d *Dispatcher) WithUsageMeter(um *usage.Meter) *Dispatcher {
	d.usageMeter = um
	return d
}

// WithTelemetry attaches tracing to every delivery path, including manual
// retries issued from the API and dashboard.
func (d *Dispatcher) WithTelemetry(t *telemetry.Provider) *Dispatcher {
	d.telemetry = t
	return d
}

// WithDigestQueue attaches the pending-digest store. Without it, digest mode
// is inert and every channel delivers immediately as before.
func (d *Dispatcher) WithDigestQueue(q store.DigestQueue) *Dispatcher {
	d.digest = q
	return d
}

// digestEnabled reports whether a channel batches its delivery.
func digestEnabled(ch store.Channel) bool {
	return ch.DigestMode == store.DigestModeWindow && ch.DigestWindowSeconds > 0
}

// Dispatch delivers one alert. Channel failures are recorded and logged,
// never fatal: one bad channel must not block the others or the poller.
//
// Before delivering, the dispatcher consults the inhibition rules targeting
// the alert's rule. An inhibited alert is recorded (MarkAlertInhibited, so
// the dashboard can show why nothing was sent) and not delivered — but the
// alert row itself is always kept.
//
// What happens after those checks depends on the monitor: one with no
// escalation policy keeps the flat fan-out to every enabled channel, one
// with a policy starts the tiered escalation instead. The suppression checks
// come first either way, so a silenced alert never starts an escalation that
// would page on its next step.
func (d *Dispatcher) Dispatch(ctx context.Context, a Alert) {
	if d.fairness != nil && d.fairness.cfg.AsyncDispatch {
		d.Submit(ctx, a)
		return
	}
	d.dispatchSync(ctx, a)
}

func (d *Dispatcher) dispatchSync(ctx context.Context, a Alert) {
	if d.isWorkspaceStopped(d.resolveAlertWorkspace(ctx, a)) {
		return
	}
	if d.inhibited(ctx, a) {
		return
	}
	// Maintenance windows suppress delivery, not detection: the alert is
	// already persisted and stays visible, it is only marked and skipped.
	// The check happens here, before fan-out, so nothing is delivered.
	if d.suppressed(ctx, a) {
		return
	}
	if d.monitorBudget != nil && a.MonitorID > 0 {
		now := d.nowTime()
		action := d.monitorBudget.RecordAlert(ctx, a, now)
		switch action.Outcome {
		case CapOutcomeSuppressed:
			reason := fmt.Sprintf("monitor %d notification volume cap reached (%d in %s)", a.MonitorID, action.Count, action.Window)
			if err := d.store.SetAlertSuppressed(ctx, a.ID, reason); err != nil {
				d.log.Error("mark alert suppressed due to monitor cap", "alert_id", a.ID, "monitor_id", a.MonitorID, "err", err)
			}
			if d.metrics != nil {
				d.metrics.RecordThrottle("monitor_cap")
			}
			d.log.Warn("monitor notification volume cap reached; alert suppressed", "alert_id", a.ID, "monitor_id", a.MonitorID, "count", action.Count)
			return
		case CapOutcomeSummary:
			a.Digest = fmt.Sprintf("Monitor %q reached notification volume cap: %d alerts in %s. Subsequent notifications are throttled.",
				a.MonitorName, action.Count, action.Window)
			if d.metrics != nil {
				d.metrics.RecordThrottle("monitor_cap")
			}
			d.log.Warn("monitor notification volume cap reached; delivering summary notification", "alert_id", a.ID, "monitor_id", a.MonitorID, "count", action.Count)
		case CapOutcomeAllow:
			// Allowed within normal budget
		}
	}

	if a.RuleID > 0 {
		state, ok := d.flappingState(ctx, a)
		if ok && state.Flapping && state.CurrentEpisodeAlerts > 1 {
			if state.CurrentEpisodeAlerts > 2 {
				reason := fmt.Sprintf("flapping: notification damped after %d alerts in the current episode", state.CurrentEpisodeAlerts)
				if err := d.store.SetAlertSuppressed(ctx, a.ID, reason); err != nil {
					d.log.Error("mark flapping alert suppressed", "alert_id", a.ID, "rule_id", a.RuleID, "err", err)
				}
				d.log.Info("flapping alert damped", "alert_id", a.ID, "rule_id", a.RuleID, "episodes", state.Episodes)
				return
			}
			a.Digest = fmt.Sprintf("Rule %d is flapping: %d alerts in the last %s across %d episodes.",
				a.RuleID, state.AlertCount, flappingWindowLabel(d.flapping.Window), state.Episodes)
		}
	}

	policy, err := d.store.GetEscalationPolicyForMonitor(ctx, a.MonitorID)
	switch {
	case errors.Is(err, store.ErrNotFound) || (err == nil && len(policy.Steps) == 0):
		d.dispatchFlat(ctx, a)
	case err != nil:
		// A store hiccup must not swallow the alert, so fall back to the flat
		// fan-out: a notification that is not escalated still beats silence.
		d.log.Error("load escalation policy", "alert_id", a.ID, "monitor_id", a.MonitorID, "err", err)
		d.dispatchFlat(ctx, a)
	default:
		d.startEscalation(ctx, a, policy)
	}
}

func flappingWindowLabel(window time.Duration) string {
	if window%time.Minute == 0 {
		minutes := int(window / time.Minute)
		unit := "minutes"
		if minutes == 1 {
			unit = "minute"
		}
		return fmt.Sprintf("%d %s", minutes, unit)
	}
	return window.String()
}

func (d *Dispatcher) flappingState(ctx context.Context, a Alert) (alerts.FlappingState, bool) {
	at := a.CreatedAt
	if at.IsZero() {
		at = d.nowTime()
	}
	since := at.Add(-d.flapping.Window - d.flapping.QuietAfter)
	times, err := d.store.ListRuleAlertTimes(ctx, a.RuleID, since)
	if err != nil {
		d.log.Error("list rule alert times for flapping detection", "alert_id", a.ID, "rule_id", a.RuleID, "err", err)
		return alerts.FlappingState{}, false
	}
	return alerts.DetectFlapping(times, at, d.flapping), true
}

// dispatchFlat is the pre-escalation behaviour: deliver to every enabled
// channel attached to the monitor. When channels span multiple workspaces,
// each workspace's channels run concurrently so a slow consumer in one
// workspace never stalls another workspace.
func (d *Dispatcher) dispatchFlat(ctx context.Context, a Alert) {
	channels, err := d.store.ListChannelsForMonitor(ctx, a.MonitorID)
	if err != nil {
		d.log.Error("list channels for alert", "alert_id", a.ID, "monitor_id", a.MonitorID, "err", err)
		return
	}
	var immediate []store.Channel
	for _, ch := range channels {
		if !severityMeetsThreshold(a.Severity, ch.MinSeverity) {
			d.log.Debug("channel skipped due to severity filter", "alert_id", a.ID, "channel_id", ch.ID, "alert_severity", a.Severity, "channel_min_severity", ch.MinSeverity)
			continue
		}
		if d.digest != nil && digestEnabled(ch) {
			d.enqueueDigest(ctx, a, ch)
			continue
		}
		immediate = append(immediate, ch)
	}
	d.deliverChannelGroup(ctx, a, immediate)
}

func (d *Dispatcher) deliverChannelGroup(ctx context.Context, a Alert, channels []store.Channel) {
	if len(channels) == 0 {
		return
	}
	if len(channels) == 1 {
		d.deliveriesInFlight.Add(1)
		d.deliver(ctx, a, channels[0])
		d.deliveriesInFlight.Add(-1)
		return
	}

	var wsOrder []string
	byWS := make(map[string][]store.Channel)
	for _, ch := range channels {
		ws := resolveWorkspaceID(ctx, ch)
		if _, ok := byWS[ws]; !ok {
			wsOrder = append(wsOrder, ws)
		}
		byWS[ws] = append(byWS[ws], ch)
	}

	if len(wsOrder) <= 1 {
		for _, ch := range channels {
			d.deliveriesInFlight.Add(1)
			d.deliver(ctx, a, ch)
			d.deliveriesInFlight.Add(-1)
		}
		return
	}

	var wg sync.WaitGroup
	for _, ws := range wsOrder {
		wsChannels := byWS[ws]
		wg.Add(1)
		go func(chs []store.Channel) {
			defer wg.Done()
			for _, ch := range chs {
				d.deliveriesInFlight.Add(1)
				d.deliver(ctx, a, ch)
				d.deliveriesInFlight.Add(-1)
			}
		}(wsChannels)
	}
	wg.Wait()
}

// inhibited answers the delivery-time suppression check for one alert.
// Failures fail open: an inhibition-store error must not silence real
// alerts, so anything that cannot be evaluated is delivered. When the
// alert is suppressed the mark is best-effort — recording why delivery
// stopped must not turn a suppression into a crash or a retry storm.
func (d *Dispatcher) inhibited(ctx context.Context, a Alert) bool {
	inhibitions, err := d.store.ListInhibitionsForTarget(ctx, a.RuleID)
	if err != nil {
		d.log.Error("list inhibitions for alert", "alert_id", a.ID, "rule_id", a.RuleID, "err", err)
		return false
	}
	sourceID, suppressed, err := alerts.InhibitedBy(ctx, a.RuleID, inhibitions, d.store.RuleFiredWithin)
	if err != nil {
		d.log.Error("evaluate inhibition", "alert_id", a.ID, "rule_id", a.RuleID, "err", err)
		return false
	}
	if !suppressed {
		return false
	}
	if err := d.store.MarkAlertInhibited(ctx, a.ID, sourceID); err != nil {
		d.log.Error("mark alert inhibited", "alert_id", a.ID, "source_rule_id", sourceID, "err", err)
	}
	d.log.Info("delivery inhibited", "alert_id", a.ID, "rule_id", a.RuleID, "source_rule_id", sourceID)
	return true
}

// severityMeetsThreshold reports whether the alert severity meets or exceeds
// the channel's minimum severity. Empty channel minimum means no filter.
func severityMeetsThreshold(alertSeverity string, channelMinSeverity store.Severity) bool {
	if channelMinSeverity == "" {
		return true
	}
	// Empty alert severity defaults to warning for backwards compatibility.
	if alertSeverity == "" {
		alertSeverity = string(store.SeverityWarning)
	}
	return store.Severity(alertSeverity).MeetsThreshold(channelMinSeverity)
}

// enqueueDigest persists an alert for a channel's next digest flush. The
// row outlives the process, so a restart does not drop a partial window.
func (d *Dispatcher) enqueueDigest(ctx context.Context, a Alert, ch store.Channel) {
	payload, err := json.Marshal(a)
	if err != nil {
		d.log.Error("marshal alert for digest", "alert_id", a.ID, "channel_id", ch.ID, "err", err)
		return
	}
	if err := d.digest.PushDigestAlert(ctx, ch.ID, payload); err != nil {
		d.log.Error("queue alert for digest", "alert_id", a.ID, "channel_id", ch.ID, "err", err)
	}
}

// RunDigestFlusher flushes due digests on a ticker until ctx is cancelled.
// interval bounds how promptly a window is closed; a window that elapses
// between ticks is flushed on the next one.
func (d *Dispatcher) RunDigestFlusher(ctx context.Context, interval time.Duration) {
	if d.digest == nil {
		return
	}
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			d.FlushDigests(ctx, now)
		}
	}
}

// FlushDigests sends one summary per channel whose oldest pending alert is
// at least its window old. Called on a ticker by RunDigestFlusher and
// directly by tests. now is injectable so window tests need no sleep.
func (d *Dispatcher) FlushDigests(ctx context.Context, now time.Time) {
	if d.digest == nil {
		return
	}
	channels, err := d.store.ListChannels(ctx, false)
	if err != nil {
		d.log.Error("list channels for digest flush", "err", err)
		return
	}
	type dueDigest struct {
		ch      store.Channel
		pending []store.DigestAlert
	}
	var wsOrder []string
	byWS := make(map[string][]dueDigest)
	for _, ch := range channels {
		if !digestEnabled(ch) {
			continue
		}
		pending, err := d.digest.ListDigestAlerts(ctx, ch.ID)
		if err != nil {
			d.log.Error("list pending digest alerts", "channel_id", ch.ID, "err", err)
			continue
		}
		if len(pending) == 0 {
			continue
		}
		window := time.Duration(ch.DigestWindowSeconds) * time.Second
		if now.Sub(pending[0].CreatedAt) < window {
			continue
		}
		ws := resolveWorkspaceID(ctx, ch)
		if _, ok := byWS[ws]; !ok {
			wsOrder = append(wsOrder, ws)
		}
		byWS[ws] = append(byWS[ws], dueDigest{ch: ch, pending: pending})
	}
	if len(wsOrder) == 0 {
		return
	}
	if len(wsOrder) == 1 {
		for _, item := range byWS[wsOrder[0]] {
			d.flushDigest(ctx, item.ch, item.pending)
		}
		return
	}
	var wg sync.WaitGroup
	for _, ws := range wsOrder {
		items := byWS[ws]
		wg.Add(1)
		go func(list []dueDigest) {
			defer wg.Done()
			for _, item := range list {
				d.flushDigest(ctx, item.ch, item.pending)
			}
		}(items)
	}
	wg.Wait()
}

// flushDigest renders and sends one channel's digest, then clears the rows it
// covered. On a send failure the rows are kept so the next tick retries; a
// partial window is never lost.
func (d *Dispatcher) flushDigest(ctx context.Context, ch store.Channel, pending []store.DigestAlert) {
	alerts := make([]Alert, 0, len(pending))
	ids := make([]int64, 0, len(pending))
	for _, p := range pending {
		var a Alert
		if err := json.Unmarshal(p.Payload, &a); err != nil {
			d.log.Error("decode pending digest alert", "channel_id", ch.ID, "err", err)
			ids = append(ids, p.ID)
			continue
		}
		// Only include alerts that meet the channel's severity threshold.
		if severityMeetsThreshold(a.Severity, ch.MinSeverity) {
			alerts = append(alerts, a)
		}
		ids = append(ids, p.ID)
	}
	if len(alerts) == 0 {
		d.log.Debug("digest skipped: no alerts meet channel severity threshold", "channel_id", ch.ID, "channel_min_severity", ch.MinSeverity)
		d.clearDigest(ctx, ch.ID, ids)
		return
	}

	notifier, err := d.factory.New(ch.Type, ch.Config)
	if err != nil {
		d.log.Error("build notifier for digest", "channel_id", ch.ID, "channel_type", ch.Type, "err", err)
		return
	}
	wsID := resolveWorkspaceID(ctx, ch)
	releaseSlot, slotErr := d.acquireDeliverySlot(ctx, wsID, ch.ID, 1)
	if slotErr != nil {
		if d.metrics != nil {
			d.metrics.RecordDelivery(ch.Type, false)
		}
		d.recordWorkspaceNotification(ctx, ch, "throttled")
		d.log.Warn("digest delivery deferred due to fairness queue saturation; keeping window for next flush",
			"channel_id", ch.ID, "workspace", wsID, "count", len(alerts), "err", slotErr)
		return
	}
	summary := RenderDigest(alerts, DefaultDigestMaxLen)
	synthetic := Alert{MonitorID: alerts[0].MonitorID, MonitorName: alerts[0].MonitorName, Digest: summary}
	timeout := ch.TimeoutDuration()
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	err = d.sendWithRecovery(attemptCtx, synthetic, ch, notifier)
	cancel()
	releaseSlot()
	if err != nil {
		if d.metrics != nil {
			d.metrics.RecordDelivery(ch.Type, false)
		}
		d.recordWorkspaceNotification(ctx, ch, "error")
		d.log.Warn("digest delivery failed; keeping the window for the next flush",
			"channel_id", ch.ID, "count", len(alerts), "err", err)
		return
	}
	if d.metrics != nil {
		d.metrics.RecordDelivery(ch.Type, true)
	}
	d.recordWorkspaceNotification(ctx, ch, "ok")
	d.log.Info("digest delivered", "channel_id", ch.ID, "count", len(alerts))
	d.clearDigest(ctx, ch.ID, ids)
}

func (d *Dispatcher) clearDigest(ctx context.Context, channelID int64, ids []int64) {
	if err := d.digest.DeleteDigestAlerts(ctx, channelID, ids); err != nil {
		d.log.Error("clear flushed digest alerts", "channel_id", channelID, "err", err)
	}
}

// suppressed reports whether a maintenance window covers the alert, marking
// the persisted alert with the window's reason when it does. A lookup error
// fails open (delivery proceeds) so a transient store problem cannot silence
// real alerts.
func (d *Dispatcher) suppressed(ctx context.Context, a Alert) bool {
	at := a.CreatedAt
	if at.IsZero() {
		at = time.Now()
	}
	w, err := d.store.ActiveMaintenanceWindow(ctx, a.MonitorID, a.ContractID, at)
	if err != nil {
		d.log.Error("check maintenance window", "alert_id", a.ID, "monitor_id", a.MonitorID, "err", err)
		return false
	}
	if w == nil {
		return false
	}
	if err := d.store.SetAlertSuppressed(ctx, a.ID, w.Reason); err != nil {
		d.log.Error("mark alert suppressed", "alert_id", a.ID, "window_id", w.ID, "err", err)
	}
	d.log.Info("alert suppressed by maintenance window", "alert_id", a.ID, "window_id", w.ID, "scope", w.Scope)
	return true
}

func (d *Dispatcher) deliver(ctx context.Context, a Alert, ch store.Channel) {
	wsID := resolveWorkspaceID(ctx, ch)
	ctx, endActive, ok := d.beginActiveDelivery(ctx, wsID, ch.ID)
	if !ok {
		return
	}
	defer endActive()

	// One span per channel covers the whole delivery: notifier construction
	// plus every retried attempt. The span's parent is whatever ctx carries
	// — the alert's span when called from Dispatch — which is the whole
	// point: a delivery must never be a root span. Channel identity rides
	// only as the row id and static type; the config (webhook URLs, bot
	// tokens, SMTP credentials) never enters a span, an attribute or an
	// event, here or anywhere.
	var span trace.Span
	if d.telemetry != nil {
		ctx, span = d.telemetry.WithRequestID(ctx, "notify.deliver",
			trace.WithAttributes(
				attribute.Int64(telemetry.AttrAlertID, a.ID),
				attribute.Int64(telemetry.AttrChannelID, ch.ID),
				attribute.String(telemetry.AttrChannelType, ch.Type),
			),
		)
		defer span.End()
	}
	if d.rateLimiter != nil {
		if err := d.rateLimiter.Wait(ctx, ch.ID, ch.Type, 0); err != nil {
			if ctx.Err() != nil && (d.isWorkspaceStopped(wsID) || d.isChannelStopped(ch.ID)) {
				return
			}
			if d.metrics != nil {
				d.metrics.RecordThrottle(ch.Type)
			}
			d.recordWorkspaceNotification(ctx, ch, "throttled")
			d.log.Warn("channel rate limit wait failed", "channel_id", ch.ID, "channel_type", ch.Type, "err", err)
		}
	}

	cb := d.registry.Get(ch.ID)
	state := cb.State()
	if d.metrics != nil {
		d.metrics.SetBreakerState(strconv.FormatInt(ch.ID, 10), ch.Type, string(state))
	}

	if !cb.Allow() {
		d.log.Debug("channel circuit breaker open, skipping delivery attempt", "channel_id", ch.ID, "alert_id", a.ID)
		return
	}

	notifier, err := d.factory.New(ch.Type, ch.Config)
	if err != nil {
		// Bad config: record one failed attempt, no point retrying. The
		// error is sanitized before it reaches the stored snippet, the log,
		// the span or the channel's last_error, because a config error can
		// quote the config. A channel whose config no longer builds a
		// notifier will never deliver again until an operator edits it, so
		// it counts as a permanent failure.
		safeErr := sanitizeDeliveryError(err)
		d.record(ctx, a.ID, ch.ID, "failed", safeErr.Error())
		d.recordHealth(ctx, ch.ID, store.ChannelHealthUpdate{Error: safeErr.Error(), Permanent: true})
		d.recordWorkspaceNotification(ctx, ch, "error")
		d.log.Error("build notifier", "channel_id", ch.ID, "channel_type", ch.Type, "err", safeErr)
		telemetry.RecordError(span, safeErr)
		cb.RecordFailure()
		if d.metrics != nil {
			d.metrics.SetBreakerState(strconv.FormatInt(ch.ID, 10), ch.Type, string(cb.State()))
		}
		return
	}

	backoff := d.BaseBackoff
	timeout := ch.TimeoutDuration()
	var lastErr error
	permanent := false
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil || d.isWorkspaceStopped(wsID) || d.isChannelStopped(ch.ID) {
			return
		}
		releaseSlot, slotErr := d.acquireDeliverySlot(ctx, wsID, ch.ID, attempt)
		if slotErr != nil {
			if ctx.Err() != nil && (d.isWorkspaceStopped(wsID) || d.isChannelStopped(ch.ID)) {
				return
			}
			if errors.Is(slotErr, ErrFairnessQueueFull) {
				if d.metrics != nil {
					d.metrics.RecordDelivery(ch.Type, false)
				}
				d.recordWorkspaceNotification(ctx, ch, "throttled")
				safeErr := sanitizeDeliveryError(slotErr)
				d.record(ctx, a.ID, ch.ID, "failed", safeErr.Error())
				telemetry.RecordError(span, safeErr)
				d.recordHealth(ctx, ch.ID, store.ChannelHealthUpdate{Error: safeErr.Error(), Permanent: permanent})
				d.deadLetter(ctx, a, ch, slotErr, attempt)
				d.log.Warn("alert delivery shed due to fairness queue saturation",
					"alert_id", a.ID, "channel_id", ch.ID, "workspace", wsID, "attempt", attempt, "err", safeErr)
			}
			return
		}
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		err = d.sendWithRecovery(attemptCtx, a, ch, notifier)
		cancel()
		releaseSlot()
		if err != nil && (d.isWorkspaceStopped(wsID) || d.isChannelStopped(ch.ID)) {
			return
		}
		if err == nil {
			cb.RecordSuccess()
			if d.metrics != nil {
				d.metrics.SetBreakerState(strconv.FormatInt(ch.ID, 10), ch.Type, string(cb.State()))
			}
			if d.metrics != nil {
				d.metrics.RecordDelivery(ch.Type, true)
			}
			d.recordWorkspaceNotification(ctx, ch, "ok")
			d.record(ctx, a.ID, ch.ID, "success", "")
			d.recordHealth(ctx, ch.ID, store.ChannelHealthUpdate{Success: true})
			d.log.Info("alert delivered", "alert_id", a.ID, "channel_id", ch.ID, "attempt", attempt)
			return
		}
		cb.RecordFailure()
		if d.metrics != nil {
			d.metrics.SetBreakerState(strconv.FormatInt(ch.ID, 10), ch.Type, string(cb.State()))
		}
		if d.metrics != nil {
			d.metrics.RecordDelivery(ch.Type, false)
		}
		// Sanitize once, then use it for every sink. The recorded string
		// becomes the delivery attempt's response_snippet and the logged and
		// traced values leave the process, so a raw transport error — which
		// can carry the webhook URL or a token from the request — must not
		// reach any of them.
		safeErr := sanitizeDeliveryError(err)
		d.record(ctx, a.ID, ch.ID, "failed", safeErr.Error())
		d.log.Warn("alert delivery failed",
			"alert_id", a.ID, "channel_id", ch.ID, "attempt", attempt, "err", safeErr)
		telemetry.RecordError(span, safeErr)

		// One delivery is several attempts, and health counts deliveries: a
		// threshold of 3 must not park a channel because one alert was
		// retried three times. The delivery is permanent if any attempt said
		// so, because a revoked credential followed by an unrelated 5xx is
		// still a revoked credential. The message stored is the sanitized
		// one — last_error is read back by the dashboard.
		lastErr = safeErr
		permanent = permanent || IsPermanent(err)

		// Honor Retry-After if present in error message or headers
		outcome := "error"
		if errStr := err.Error(); strings.Contains(errStr, "Retry-After") || strings.Contains(errStr, "429") {
			outcome = "throttled"
			if d.metrics != nil {
				d.metrics.RecordThrottle(ch.Type)
			}
		}
		d.recordWorkspaceNotification(ctx, ch, outcome)

		// A receiver that rejected the request itself (a 4xx that is not a
		// "come back later" answer) will reject it again, so the delivery is
		// over rather than paused: spending the retry budget on it would
		// write the same failure three times and delay the dead letter that
		// tells the operator the payload is the problem.
		if !Retryable(err) {
			d.recordHealth(ctx, ch.ID, store.ChannelHealthUpdate{Error: lastErr.Error(), Permanent: permanent})
			d.deadLetter(ctx, a, ch, err, attempt)
			return
		}

		if attempt >= d.MaxAttempts {
			d.recordHealth(ctx, ch.ID, store.ChannelHealthUpdate{Error: lastErr.Error(), Permanent: permanent})
			d.deadLetter(ctx, a, ch, err, attempt)
			return
		}
		if ctx.Err() != nil || cb.State() == StateOpen {
			// Shutting down mid-delivery, or the breaker tripped: the channel
			// is not at fault for either, so this outcome leaves no mark on
			// its health.
			return
		}
		select {
		case <-time.After(backoff):
			backoff *= 2
		case <-ctx.Done():
			return
		}
	}
}

// deadLetter records a delivery that will not be retried. The error is
// sanitized before it is stored: the message becomes the dead letter's
// last_error, which the dashboard reads back, and a transport error can carry
// the webhook URL or a token from the request.
func (d *Dispatcher) deadLetter(ctx context.Context, a Alert, ch store.Channel, err error, attempts int) {
	lastStatus := 0
	var statusErr *StatusError
	if errors.As(err, &statusErr) {
		lastStatus = statusErr.Code
	}
	dl := &store.DeadLetter{
		AlertID:      a.ID,
		ChannelID:    ch.ID,
		LastError:    sanitizeDeliveryError(redactURLError(err)).Error(),
		AttemptCount: attempts,
		LastStatus:   lastStatus,
	}
	if errStore := d.store.CreateDeadLetter(ctx, dl); errStore != nil {
		d.log.Error("create dead letter", "alert_id", a.ID, "channel_id", ch.ID, "err", errStore)
	} else if d.metrics != nil {
		ws, _ := workspace.From(ctx)
		d.metrics.RecordDeadLetterFor(string(ws), ch.Name)
	}
}

func (d *Dispatcher) acquireDeliverySlot(ctx context.Context, wsID string, channelID int64, attempt int) (func(), error) {
	if d.fairness == nil {
		return func() {}, nil
	}
	release, _, err := d.fairness.AcquireSlot(ctx, wsID, channelID, attempt)
	return release, err
}

func (d *Dispatcher) sendWithRecovery(ctx context.Context, a Alert, ch store.Channel, notifier Notifier) (err error) {
	defer func() {
		if r := recover(); r != nil {
			d.log.Error("notifier panicked", "alert_id", a.ID, "channel_id", ch.ID)
			err = errors.New("delivery failed")
		}
	}()
	return notifier.Send(ctx, a)
}

func sanitizeDeliveryError(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(err.Error())
	if msg == "" {
		return errors.New("delivery failed")
	}
	msg = redactSecretText(msg)
	if msg == "" {
		return errors.New("delivery failed")
	}
	return errors.New(msg)
}

func redactSecretText(s string) string {
	secretPattern := regexp.MustCompile(`(?i)\b(?:token|secret|password|passwd|api[_-]?key|auth[_-]?token|access[_-]?token|bearer)\b(?:\s*[:=]\s*|\s+)[A-Za-z0-9._~:/?#[\]@!$&'()*+,;=%-]+`)
	return secretPattern.ReplaceAllString(s, "[redacted]")
}

// recordHealth folds one delivery outcome into the channel's health, which is
// what lets a channel say it is broken without an operator reading delivery
// attempts one alert at a time — and what takes a channel out of rotation once
// its failures are conclusive.
//
// A store error is logged and swallowed: health is derived state, and failing
// a delivery over a bookkeeping write would turn a reporting problem into a
// delivery problem.
func (d *Dispatcher) recordHealth(ctx context.Context, channelID int64, u store.ChannelHealthUpdate) {
	u.DisableAfter = d.disableAfterFailures
	if err := d.store.RecordChannelHealth(ctx, channelID, u); err != nil {
		d.log.Error("record channel health", "channel_id", channelID, "err", err)
	}
}

func (d *Dispatcher) record(ctx context.Context, alertID, channelID int64, status, snippet string) *store.DeliveryAttempt {
	if len(snippet) > 500 {
		snippet = snippet[:500]
	}
	snippet = redactSecretText(snippet)
	if snippet == "" {
		snippet = "delivery failed"
	}
	da := &store.DeliveryAttempt{
		AlertID:         alertID,
		ChannelID:       channelID,
		Status:          status,
		ResponseSnippet: snippet,
	}
	if err := d.store.RecordDeliveryAttempt(ctx, da); err != nil {
		d.log.Error("record delivery attempt", "alert_id", alertID, "channel_id", channelID, "err", err)
	}
	return da
}

// RedriveDeadLetter attempts delivery for a dead-letter record and deletes the record on success.
func (d *Dispatcher) RedriveDeadLetter(ctx context.Context, deadLetterID int64, dl store.DeadLetter) error {
	alert, err := d.store.GetAlert(ctx, dl.AlertID)
	if err != nil {
		return fmt.Errorf("get alert: %w", err)
	}
	ch, err := d.store.GetChannel(ctx, dl.ChannelID)
	if err != nil {
		return fmt.Errorf("get channel: %w", err)
	}
	if !ch.Enabled {
		return ErrChannelDisabled
	}

	attempts, err := d.store.ListDeliveryAttempts(ctx, dl.AlertID, "")
	if err != nil {
		return fmt.Errorf("list attempts: %w", err)
	}
	for _, att := range attempts {
		if att.ChannelID == dl.ChannelID && att.Status == store.DeliveryStatusSuccess {
			_ = d.store.DeleteDeadLetter(ctx, deadLetterID)
			return ErrAlreadySucceeded
		}
	}

	notifier, err := d.factory.New(ch.Type, ch.Config)
	if err != nil {
		return fmt.Errorf("build notifier: %w", err)
	}

	timeout := ch.TimeoutDuration()
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	alertObj := Alert{
		ID:         alert.ID,
		MonitorID:  alert.MonitorID,
		RuleID:     alert.RuleID,
		EventID:    alert.EventID,
		Payload:    alert.Payload,
		Enrichment: alert.Enrichment,
		Severity:   string(alert.Severity),
		CreatedAt:  alert.CreatedAt,
		// A federated alert has no local rule, so the channels that key on
		// the rule need the instance it came from to tell two peers' alerts
		// apart.
		FederationOrigin: alert.FederationOrigin,
		RunbookURL:       alert.RunbookURL,
		ResponseGuidance: alert.ResponseGuidance,
	}

	err = notifier.Send(attemptCtx, alertObj)
	if err != nil {
		safeErr := sanitizeDeliveryError(redactURLError(err))
		d.recordWorkspaceNotification(ctx, *ch, "error")
		d.record(ctx, alert.ID, ch.ID, "failed", "redrive: "+safeErr.Error())
		return safeErr
	}

	d.recordWorkspaceNotification(ctx, *ch, "ok")
	d.record(ctx, alert.ID, ch.ID, "success", "redrive: success")
	if err := d.store.DeleteDeadLetter(ctx, deadLetterID); err != nil {
		d.log.Error("delete dead letter after successful redrive", "dead_letter_id", deadLetterID, "err", err)
	} else if d.metrics != nil {
		ws, _ := workspace.From(ctx)
		d.metrics.ClearDeadLetterFor(string(ws), ch.Name)
	}
	return nil
}

// GateRetry decides whether a manual retry is allowed. attempts is the
// alert's full delivery history; only rows for channelID are considered.
// cooldown is measured from the most recent attempt for that channel.
func GateRetry(attempts []store.DeliveryAttempt, channelID int64, ch store.Channel, now time.Time, cooldown time.Duration) error {
	if !ch.Enabled {
		return ErrChannelDisabled
	}
	var last *store.DeliveryAttempt
	any := false
	for i := range attempts {
		if attempts[i].ChannelID != channelID {
			continue
		}
		any = true
		if attempts[i].Status == "success" {
			return ErrAlreadySucceeded
		}
		if last == nil || attempts[i].AttemptedAt.After(last.AttemptedAt) {
			last = &attempts[i]
		}
	}
	if !any {
		return ErrNoAttempt
	}
	if cooldown > 0 && last != nil && !last.AttemptedAt.IsZero() && now.Sub(last.AttemptedAt) < cooldown {
		return ErrRetryCooldown
	}
	return nil
}

// Retry sends the alert once to ch and records a new delivery attempt.
// Unlike Dispatch it does not loop with backoff: the operator asked for
// one try and the HTTP handler returns that outcome on the same request.
func (d *Dispatcher) Retry(ctx context.Context, a Alert, ch store.Channel) *store.DeliveryAttempt {
	// The retry gets its own span under the API request's trace, so a
	// manual re-send is attributable on the dashboard's request id too.
	var span trace.Span
	if d.telemetry != nil {
		ctx, span = d.telemetry.WithRequestID(ctx, "notify.retry",
			trace.WithAttributes(
				attribute.Int64(telemetry.AttrAlertID, a.ID),
				attribute.Int64(telemetry.AttrChannelID, ch.ID),
				attribute.String(telemetry.AttrChannelType, ch.Type),
			),
		)
		defer span.End()
	}

	if !severityMeetsThreshold(a.Severity, ch.MinSeverity) {
		d.log.Debug("retry skipped due to severity filter", "alert_id", a.ID, "channel_id", ch.ID, "alert_severity", a.Severity, "channel_min_severity", ch.MinSeverity)
		return d.record(ctx, a.ID, ch.ID, "failed", "alert severity below channel minimum")
	}
	if d.rateLimiter != nil {
		if err := d.rateLimiter.Wait(ctx, ch.ID, ch.Type, 0); err != nil {
			if d.metrics != nil {
				d.metrics.RecordThrottle(ch.Type)
			}
			d.recordWorkspaceNotification(ctx, ch, "throttled")
		}
	}
	notifier, err := d.factory.New(ch.Type, ch.Config)
	if err != nil {
		d.log.Error("build notifier", "channel_id", ch.ID, "channel_type", ch.Type, "err", err)
		d.recordHealth(ctx, ch.ID, store.ChannelHealthUpdate{Error: err.Error(), Permanent: true})
		d.recordWorkspaceNotification(ctx, ch, "error")
		return d.record(ctx, a.ID, ch.ID, "failed", err.Error())
	}
	timeout := ch.TimeoutDuration()
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	err = notifier.Send(attemptCtx, a)
	cancel()
	if err == nil {
		if d.metrics != nil {
			d.metrics.RecordDelivery(ch.Type, true)
		}
		d.recordWorkspaceNotification(ctx, ch, "ok")
		d.recordHealth(ctx, ch.ID, store.ChannelHealthUpdate{Success: true})
		d.log.Info("alert delivered", "alert_id", a.ID, "channel_id", ch.ID, "attempt", "retry")
		return d.record(ctx, a.ID, ch.ID, "success", "")
	}
	if d.metrics != nil {
		d.metrics.RecordDelivery(ch.Type, false)
	}
	d.recordWorkspaceNotification(ctx, ch, "error")
	d.log.Warn("alert delivery failed", "alert_id", a.ID, "channel_id", ch.ID, "attempt", "retry", "err", err)
	// A manual retry is a real delivery outcome, so it counts: an operator who
	// fixes a channel and re-sends the alert should see the channel recover,
	// and one that still fails should see the count continue to climb.
	d.recordHealth(ctx, ch.ID, store.ChannelHealthUpdate{Error: err.Error(), Permanent: IsPermanent(err)})
	return d.record(ctx, a.ID, ch.ID, "failed", err.Error())
}

// Shutdown gracefully stops the dispatcher. It waits for all in-flight
// deliveries to complete (up to the configured grace period), then returns.
// This is called from the server's run loop during ordered shutdown so that
// inflight deliveries are persisted as retries before the process exits.
func (d *Dispatcher) Shutdown(ctx context.Context, gracePeriod time.Duration) error {
	// If no grace period is set, just stop immediately without waiting.
	if gracePeriod <= 0 {
		return nil
	}
	// Record that shutdown has been initiated so in-flight deliveries
	// know to stop marking permanent failures and instead just return.
	d.log.Info("shutting down dispatcher", "grace_period", gracePeriod)
	if d.fairness != nil {
		waitCtx, cancel := context.WithTimeout(ctx, gracePeriod)
		defer cancel()
		_ = d.fairness.Wait(waitCtx)
	}
	return nil
}

func (d *Dispatcher) recordWorkspaceNotification(ctx context.Context, ch store.Channel, outcome string) {
	wsID := ch.WorkspaceID
	if wsID == "" {
		if ws, ok := workspace.From(ctx); ok && !workspace.System(ctx) {
			wsID = string(ws)
		}
	}
	if d.usageMeter != nil {
		d.usageMeter.RecordNotification(wsID, ch.Type, outcome, 1)
		return
	}
	if d.metrics != nil {
		d.metrics.RecordWorkspaceNotification(wsID, ch.Type, outcome, 1)
	}
	if uStore, ok := d.store.(store.Usage); ok {
		_ = store.SafeRecordWorkspaceUsageBatch(ctx, uStore, []store.WorkspaceUsageDelta{
			{
				WorkspaceID: wsID,
				Notifications: []store.WorkspaceNotificationUsage{
					{ChannelType: ch.Type, Outcome: outcome, Count: 1},
				},
			},
		})
	}
}
