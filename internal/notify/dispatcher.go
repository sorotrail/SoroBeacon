package notify

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/store"
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
}

// Dispatcher fans an alert out to its monitor's channels, retrying each
// channel with exponential backoff and recording every attempt.
type Dispatcher struct {
	store   DispatchStore
	factory *Factory
	log     *slog.Logger
	// metrics is optional Prometheus instrumentation; nil-safe.
	metrics *metrics.Metrics

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
func (d *Dispatcher) Dispatch(ctx context.Context, a Alert) {
	channels, err := d.store.ListChannelsForMonitor(ctx, a.MonitorID)
	if err != nil {
		d.log.Error("list channels for alert", "alert_id", a.ID, "monitor_id", a.MonitorID, "err", err)
		return
	}
	for _, ch := range channels {
		if d.digest != nil && digestEnabled(ch) {
			d.enqueueDigest(ctx, a, ch)
			continue
		}
		d.deliver(ctx, a, ch)
	}
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
		alerts = append(alerts, a)
		ids = append(ids, p.ID)
	}
	if len(alerts) == 0 {
		d.clearDigest(ctx, ch.ID, ids)
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