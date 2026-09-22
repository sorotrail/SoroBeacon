package notify

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
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

// DefaultRetryCooldown bounds how often an operator can re-send one
// alert to one channel. Long enough to stop a jammed button, short
// enough that a real "the webhook is fixed, try again" is not blocked.
const DefaultRetryCooldown = 30 * time.Second

// abandonedSnippet is recorded on delivery_attempts that were still
// in-flight when the shutdown grace period expired. It is a fixed
// string so channel config never leaks into the snippet.
const abandonedSnippet = "abandoned at shutdown"

// drainCancelMargin is how long Drain waits after cancelling hung
// sends for them to record and return, so the process can still exit
// inside the grace period plus a small bound.
const drainCancelMargin = 250 * time.Millisecond

// DispatchStore is the slice of the store the dispatcher needs.
type DispatchStore interface {
	ListChannelsForMonitor(ctx context.Context, monitorID int64) ([]store.Channel, error)
	RecordDeliveryAttempt(ctx context.Context, d *store.DeliveryAttempt) error
}

// flight is one in-flight channel send, tracked so Drain can persist
// leftovers as retryable when the grace period expires.
type flight struct {
	alertID   int64
	channelID int64
	abandoned atomic.Bool
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

	// sendCtx is cancelled when Drain's grace period expires so a hung
	// HTTP call cannot block process exit. Independent of the poller's
	// context, which is cancelled immediately on SIGTERM.
	sendCtx    context.Context
	sendCancel context.CancelFunc

	inflight sync.WaitGroup
	mu       sync.Mutex
	flights  map[*flight]struct{}
}

// NewDispatcher wires a Dispatcher with default retry settings.
func NewDispatcher(s DispatchStore, f *Factory, log *slog.Logger) *Dispatcher {
	sendCtx, cancel := context.WithCancel(context.Background())
	return &Dispatcher{
		store:       s,
		factory:     f,
		log:         log,
		MaxAttempts: 3,
		BaseBackoff: time.Second,
		sendCtx:     sendCtx,
		sendCancel:  cancel,
		flights:     make(map[*flight]struct{}),
	}
}

// WithMetrics attaches delivery instrumentation.
func (d *Dispatcher) WithMetrics(m *metrics.Metrics) *Dispatcher {
	d.metrics = m
	return d
}

// Dispatch delivers one alert to every enabled channel attached to its
// monitor. Channel failures are recorded and logged, never fatal: one bad
// channel must not block the others or the poller.
//
// A cancelled caller context (poller stopping) means do not start new
// work. Sends that have already begun use sendCtx so SIGTERM does not
// drop them; Drain cancels sendCtx when the grace period expires.
func (d *Dispatcher) Dispatch(ctx context.Context, a Alert) {
	if ctx.Err() != nil {
		return
	}
	d.inflight.Add(1)
	defer d.inflight.Done()

	channels, err := d.store.ListChannelsForMonitor(ctx, a.MonitorID)
	if err != nil {
		d.log.Error("list channels for alert", "alert_id", a.ID, "monitor_id", a.MonitorID, "err", err)
		return
	}
	for _, ch := range channels {
		d.deliver(a, ch)
	}
}

func (d *Dispatcher) deliver(a Alert, ch store.Channel) {
	f := &flight{alertID: a.ID, channelID: ch.ID}
	d.mu.Lock()
	d.flights[f] = struct{}{}
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.flights, f)
		d.mu.Unlock()
	}()

	notifier, err := d.factory.New(ch.Type, ch.Config)
	if err != nil {
		// Bad config: record one failed attempt, no point retrying.
		d.record(context.Background(), a.ID, ch.ID, "failed", err.Error())
		d.log.Error("build notifier", "channel_id", ch.ID, "channel_type", ch.Type, "err", err)
		return
	}

	backoff := d.BaseBackoff
	for attempt := 1; ; attempt++ {
		if f.abandoned.Load() {
			return
		}
		err := notifier.Send(d.sendCtx, a)
		if f.abandoned.Load() {
			return
		}
		if err == nil {
			d.metrics.RecordDelivery(ch.Type, true)
			d.record(context.Background(), a.ID, ch.ID, "success", "")
			d.log.Info("alert delivered", "alert_id", a.ID, "channel_id", ch.ID, "attempt", attempt)
			return
		}
		if d.sendCtx.Err() != nil {
			d.abandon(f)
			return
		}
		d.metrics.RecordDelivery(ch.Type, false)
		d.record(context.Background(), a.ID, ch.ID, "failed", err.Error())
		d.log.Warn("alert delivery failed",
			"alert_id", a.ID, "channel_id", ch.ID, "attempt", attempt, "err", err)

		if attempt >= d.MaxAttempts {
			return
		}
		select {
		case <-time.After(backoff):
			backoff *= 2
		case <-d.sendCtx.Done():
			d.abandon(f)
			return
		}
	}
}

// Drain waits for in-flight deliveries up to ctx. When the deadline
// expires it persists leftovers as failed/retryable, cancels hung sends,
// and returns how many were abandoned. The process can then exit.
func (d *Dispatcher) Drain(ctx context.Context) int {
	done := make(chan struct{})
	go func() {
		d.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return 0
	case <-ctx.Done():
		n := d.persistAbandoned()
		d.sendCancel()
		timer := time.NewTimer(drainCancelMargin)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
		}
		return n
	}
}

func (d *Dispatcher) persistAbandoned() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for f := range d.flights {
		if d.abandonLocked(f) {
			n++
		}
	}
	return n
}

func (d *Dispatcher) abandon(f *flight) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.abandonLocked(f)
}

func (d *Dispatcher) abandonLocked(f *flight) bool {
	if !f.abandoned.CompareAndSwap(false, true) {
		return false
	}
	d.record(context.Background(), f.alertID, f.channelID, "failed", abandonedSnippet)
	d.log.Info("delivery abandoned at shutdown", "alert_id", f.alertID, "channel_id", f.channelID)
	return true
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
	if ctx.Err() != nil && d.sendCtx.Err() != nil {
		return d.record(context.Background(), a.ID, ch.ID, "failed", abandonedSnippet)
	}
	d.inflight.Add(1)
	defer d.inflight.Done()
	f := &flight{alertID: a.ID, channelID: ch.ID}
	d.mu.Lock()
	d.flights[f] = struct{}{}
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.flights, f)
		d.mu.Unlock()
	}()

	notifier, err := d.factory.New(ch.Type, ch.Config)
	if err != nil {
		d.log.Error("build notifier", "channel_id", ch.ID, "channel_type", ch.Type, "err", err)
		return d.record(context.Background(), a.ID, ch.ID, "failed", err.Error())
	}
	err = notifier.Send(d.sendCtx, a)
	if f.abandoned.Load() {
		return &store.DeliveryAttempt{AlertID: a.ID, ChannelID: ch.ID, Status: "failed", ResponseSnippet: abandonedSnippet}
	}
	if err == nil {
		d.metrics.RecordDelivery(ch.Type, true)
		d.log.Info("alert delivered", "alert_id", a.ID, "channel_id", ch.ID, "attempt", "retry")
		return d.record(context.Background(), a.ID, ch.ID, "success", "")
	}
	if d.sendCtx.Err() != nil {
		d.abandon(f)
		return &store.DeliveryAttempt{AlertID: a.ID, ChannelID: ch.ID, Status: "failed", ResponseSnippet: abandonedSnippet}
	}
	d.metrics.RecordDelivery(ch.Type, false)
	d.log.Warn("alert delivery failed", "alert_id", a.ID, "channel_id", ch.ID, "attempt", "retry", "err", err)
	return d.record(context.Background(), a.ID, ch.ID, "failed", err.Error())
}
