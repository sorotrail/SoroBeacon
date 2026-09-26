package notify

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/alerts"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/telemetry"
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

// DispatchStore is the slice of the store the dispatcher needs.
type DispatchStore interface {
	ListChannelsForMonitor(ctx context.Context, monitorID int64) ([]store.Channel, error)
	RecordDeliveryAttempt(ctx context.Context, d *store.DeliveryAttempt) error
	// ListChannels is how the digest flusher discovers channels with a
	// window to close. Dispatch itself only needs ListChannelsForMonitor.
	ListChannels(ctx context.Context, enabledOnly bool) ([]store.Channel, error)
	// The inhibition trio backs the delivery-time suppression check
	// (alerts.InhibitedBy in internal/alerts): the pairs targeting the
	// alert's rule, whether a source is still firing, and the mark that
	// records why delivery stopped.
	ListInhibitionsForTarget(ctx context.Context, targetRuleID int64) ([]store.Inhibition, error)
	RuleFiredWithin(ctx context.Context, ruleID int64, window time.Duration) (bool, error)
	MarkAlertInhibited(ctx context.Context, alertID, sourceRuleID int64) error
}

// Dispatcher fans an alert out to its monitor's channels, retrying each
// channel with exponential backoff and recording every attempt.
type Dispatcher struct {
	store   DispatchStore
	factory *Factory
	log     *slog.Logger
	// metrics is optional Prometheus instrumentation; nil-safe.
	metrics *metrics.Metrics
	// telemetry is optional tracing; nil-safe. Delivery spans are started
	// from the alert's context, so they are children of the alert's span —
	// that parent chain, not any attribute, is what joins the delivery to
	// the poll cycle that produced it.
	telemetry *telemetry.Provider

	// MaxAttempts per channel (default 3) and BaseBackoff between attempts
	// (default 1s, doubled each retry: 1s, 2s, 4s...).
	MaxAttempts int
	BaseBackoff time.Duration
	rateLimiter *ChannelRateLimiter

	// digest is optional. When attached, channels with digest mode
	// "window" accumulate alerts here and a summary is flushed once the
	// window elapses.
	digest store.DigestQueue
}

// NewDispatcher wires a Dispatcher with default retry settings.
func NewDispatcher(s DispatchStore, f *Factory, log *slog.Logger) *Dispatcher {
	defaults := map[string]float64{
		"slack":     1.0,
		"telegram":  30.0,
		"pagerduty": 2.0,
		"discord":   5.0,
		"email":     5.0,
		"webhook":   5.0,
	}
	return &Dispatcher{
		store:       s,
		factory:     f,
		log:         log,
		MaxAttempts: 3,
		BaseBackoff: time.Second,
		rateLimiter: NewChannelRateLimiter(defaults),
	}
}

// WithMetrics attaches delivery instrumentation.
func (d *Dispatcher) WithMetrics(m *metrics.Metrics) *Dispatcher {
	d.metrics = m
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

// Dispatch delivers one alert to every enabled channel attached to its
// monitor. Channel failures are recorded and logged, never fatal: one bad
// channel must not block the others or the poller.
//
// Before delivering, the dispatcher consults the inhibition rules targeting
// the alert's rule. An inhibited alert is recorded (MarkAlertInhibited, so
// the dashboard can show why nothing was sent) and not delivered — but the
// alert row itself is always kept.
func (d *Dispatcher) Dispatch(ctx context.Context, a Alert) {
	if d.inhibited(ctx, a) {
		return
	}
	channels, err := d.store.ListChannelsForMonitor(ctx, a.MonitorID)
	if err != nil {
		d.log.Error("list channels for alert", "alert_id", a.ID, "monitor_id", a.MonitorID, "err", err)
		return
	}
	for _, ch := range channels {
		if !severityMeetsThreshold(a.Severity, ch.MinSeverity) {
			d.log.Debug("channel skipped due to severity filter", "alert_id", a.ID, "channel_id", ch.ID, "alert_severity", a.Severity, "channel_min_severity", ch.MinSeverity)
			continue
		}
		if d.digest != nil && digestEnabled(ch) {
			d.enqueueDigest(ctx, a, ch)
			continue
		}
		d.deliver(ctx, a, ch)
	}
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
		d.flushDigest(ctx, ch, pending)
	}
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
		return
	}

	notifier, err := d.factory.New(ch.Type, ch.Config)
	if err != nil {
		d.log.Error("build notifier for digest", "channel_id", ch.ID, "channel_type", ch.Type, "err", err)
		return
	}
	summary := RenderDigest(alerts, DefaultDigestMaxLen)
	synthetic := Alert{MonitorID: alerts[0].MonitorID, MonitorName: alerts[0].MonitorName, Digest: summary}
	if err := notifier.Send(ctx, synthetic); err != nil {
		if d.metrics != nil {
			d.metrics.RecordDelivery(ch.Type, false)
		}
		d.log.Warn("digest delivery failed; keeping the window for the next flush",
			"channel_id", ch.ID, "count", len(alerts), "err", err)
		return
	}
	if d.metrics != nil {
		d.metrics.RecordDelivery(ch.Type, true)
	}
	d.log.Info("digest delivered", "channel_id", ch.ID, "count", len(alerts))
	d.clearDigest(ctx, ch.ID, ids)
}

func (d *Dispatcher) clearDigest(ctx context.Context, channelID int64, ids []int64) {
	if err := d.digest.DeleteDigestAlerts(ctx, channelID, ids); err != nil {
		d.log.Error("clear flushed digest alerts", "channel_id", channelID, "err", err)
	}
}

func (d *Dispatcher) deliver(ctx context.Context, a Alert, ch store.Channel) {
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
			if d.metrics != nil {
				d.metrics.RecordThrottle(ch.Type)
			}
			d.log.Warn("channel rate limit wait failed", "channel_id", ch.ID, "channel_type", ch.Type, "err", err)
		}
	}

	notifier, err := d.factory.New(ch.Type, ch.Config)
	if err != nil {
		// Bad config: record one failed attempt, no point retrying.
		d.record(ctx, a.ID, ch.ID, "failed", err.Error())
		d.log.Error("build notifier", "channel_id", ch.ID, "channel_type", ch.Type, "err", err)
		telemetry.RecordError(span, err)
		return
	}

	backoff := d.BaseBackoff
	for attempt := 1; ; attempt++ {
		err := notifier.Send(ctx, a)
		if err == nil {
			if d.metrics != nil {
				d.metrics.RecordDelivery(ch.Type, true)
			}
			d.record(ctx, a.ID, ch.ID, "success", "")
			d.log.Info("alert delivered", "alert_id", a.ID, "channel_id", ch.ID, "attempt", attempt)
			return
		}
		if d.metrics != nil {
			d.metrics.RecordDelivery(ch.Type, false)
		}
		d.record(ctx, a.ID, ch.ID, "failed", err.Error())
		d.log.Warn("alert delivery failed",
			"alert_id", a.ID, "channel_id", ch.ID, "attempt", attempt, "err", err)
		telemetry.RecordError(span, err)

		// Honor Retry-After if present in error message or headers
		if errStr := err.Error(); strings.Contains(errStr, "Retry-After") || strings.Contains(errStr, "429") {
			if d.metrics != nil {
				d.metrics.RecordThrottle(ch.Type)
			}
		}

		if attempt >= d.MaxAttempts || ctx.Err() != nil {
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

func (d *Dispatcher) record(ctx context.Context, alertID, channelID int64, status, snippet string) *store.DeliveryAttempt {
	if len(snippet) > 500 {
		snippet = snippet[:500]
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
		}
	}
	notifier, err := d.factory.New(ch.Type, ch.Config)
	if err != nil {
		d.log.Error("build notifier", "channel_id", ch.ID, "channel_type", ch.Type, "err", err)
		return d.record(ctx, a.ID, ch.ID, "failed", err.Error())
	}
	err = notifier.Send(ctx, a)
	if err == nil {
		if d.metrics != nil {
			d.metrics.RecordDelivery(ch.Type, true)
		}
		d.log.Info("alert delivered", "alert_id", a.ID, "channel_id", ch.ID, "attempt", "retry")
		return d.record(ctx, a.ID, ch.ID, "success", "")
	}
	if d.metrics != nil {
		d.metrics.RecordDelivery(ch.Type, false)
	}
	d.log.Warn("alert delivery failed", "alert_id", a.ID, "channel_id", ch.ID, "attempt", "retry", "err", err)
	return d.record(ctx, a.ID, ch.ID, "failed", err.Error())
}
