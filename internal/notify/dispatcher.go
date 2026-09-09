package notify

import (
	"context"
	"log/slog"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// DispatchStore is the slice of the store the dispatcher needs.
type DispatchStore interface {
	ListChannelsForMonitor(ctx context.Context, monitorID int64) ([]store.Channel, error)
	RecordDeliveryAttempt(ctx context.Context, d *store.DeliveryAttempt) error
}

// Dispatcher fans an alert out to its monitor's channels, retrying each
// channel with exponential backoff and recording every attempt.
type Dispatcher struct {
	store   DispatchStore
	factory *Factory
	log     *slog.Logger

	// MaxAttempts per channel (default 3) and BaseBackoff between attempts
	// (default 1s, doubled each retry: 1s, 2s, 4s...).
	MaxAttempts int
	BaseBackoff time.Duration
}

// NewDispatcher wires a Dispatcher with default retry settings.
func NewDispatcher(s DispatchStore, f *Factory, log *slog.Logger) *Dispatcher {
	return &Dispatcher{
		store:       s,
		factory:     f,
		log:         log,
		MaxAttempts: 3,
		BaseBackoff: time.Second,
	}
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
		d.deliver(ctx, a, ch)
	}
}

func (d *Dispatcher) deliver(ctx context.Context, a Alert, ch store.Channel) {
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
			d.record(ctx, a.ID, ch.ID, "success", "")
			d.log.Info("alert delivered", "alert_id", a.ID, "channel_id", ch.ID, "attempt", attempt)
			return
		}
		d.record(ctx, a.ID, ch.ID, "failed", err.Error())
		d.log.Warn("alert delivery failed",
			"alert_id", a.ID, "channel_id", ch.ID, "attempt", attempt, "err", err)

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

func (d *Dispatcher) record(ctx context.Context, alertID, channelID int64, status, snippet string) {
	if len(snippet) > 500 {
		snippet = snippet[:500]
	}
	err := d.store.RecordDeliveryAttempt(ctx, &store.DeliveryAttempt{
		AlertID:         alertID,
		ChannelID:       channelID,
		Status:          status,
		ResponseSnippet: snippet,
	})
	if err != nil {
		d.log.Error("record delivery attempt", "alert_id", alertID, "channel_id", channelID, "err", err)
	}
}
