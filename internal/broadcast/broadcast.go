// Package broadcast fans alerts out to live subscribers in process.
//
// It backs the SSE endpoint in internal/api: the poller publishes every alert
// it creates, each connected subscriber receives it, and a slow or dead
// subscriber never blocks the publisher. Each subscriber owns a buffered
// channel; when that buffer is full the oldest pending event is dropped so
// the newest — the one a live viewer is waiting for — still gets through, and
// the drop is counted. Dropping is deliberate: one client that cannot keep up
// must not stall alert creation for every other client.
package broadcast

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultBuffer is the per-subscriber queue depth. A subscriber that falls
// further behind than this starts losing its oldest pending events.
const DefaultBuffer = 64

// Alert is the live view of a persisted alert: the payload of one SSE event.
// It carries the stored alert's fields plus the monitor's name so a live
// dashboard row needs no second lookup. It is modelled on store.Alert rather
// than importing it, so the fan-out stays a leaf package with no dependency
// on the database.
type Alert struct {
	ID          int64           `json:"id"`
	MonitorID   int64           `json:"monitor_id"`
	MonitorName string          `json:"monitor_name"`
	RuleID      int64           `json:"rule_id"`
	EventID     string          `json:"event_id"`
	Payload     json.RawMessage `json:"payload"`
	CreatedAt   time.Time       `json:"created_at"`
}

// Broadcaster fans Alerts out to its subscribers. The zero value is not
// usable; call New. All methods are safe for concurrent use.
type Broadcaster struct {
	mu      sync.Mutex
	subs    map[*Subscriber]struct{}
	buffer  int
	dropped atomic.Uint64
}

// New returns a Broadcaster whose subscribers each queue buffer events. A
// non-positive buffer falls back to DefaultBuffer so a miswired caller cannot
// make the queue unbuffered (every publish would then drop).
func New(buffer int) *Broadcaster {
	if buffer < 1 {
		buffer = DefaultBuffer
	}
	return &Broadcaster{subs: map[*Subscriber]struct{}{}, buffer: buffer}
}

// Subscriber is one connected SSE client. Read alerts from C until the client
// goes away, then Close it. monitorID 0 receives every alert; any other value
// filters to that monitor, server-side.
type Subscriber struct {
	// C is the subscriber's queue. It is buffered, and the broadcaster may
	// drop the oldest value when it fills, so a reader must not assume it
	// sees every alert.
	C <-chan Alert

	b         *Broadcaster
	ch        chan Alert
	monitorID int64
	closeOnce sync.Once
}

// Subscribe registers a new client. Always Close the returned Subscriber, or
// it (and its buffered channel) leak for the life of the process.
func (b *Broadcaster) Subscribe(monitorID int64) *Subscriber {
	ch := make(chan Alert, b.buffer)
	sub := &Subscriber{C: ch, ch: ch, monitorID: monitorID, b: b}
	b.mu.Lock()
	b.subs[sub] = struct{}{}
	b.mu.Unlock()
	return sub
}

// Close unregisters the subscriber and releases its queue. It is idempotent,
// so a deferred Close plus an explicit one in a disconnect path is safe.
func (s *Subscriber) Close() {
	s.closeOnce.Do(func() {
		s.b.mu.Lock()
		delete(s.b.subs, s)
		s.b.mu.Unlock()
	})
}

// Publish fans a out to every matching subscriber and returns without
// blocking. A subscriber whose queue is full loses its oldest pending event
// instead of stalling the caller, and that loss is counted by Dropped.
func (b *Broadcaster) Publish(a Alert) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for sub := range b.subs {
		if sub.monitorID != 0 && sub.monitorID != a.MonitorID {
			continue
		}
		b.deliver(sub, a)
	}
}

// deliver enqueues a for one subscriber, applying the drop-oldest policy when
// the queue is full. It must run under b.mu: that serializes the only writes
// to sub.ch, so the drain below cannot race another publisher.
func (b *Broadcaster) deliver(sub *Subscriber, a Alert) {
	select {
	case sub.ch <- a:
		return
	default:
	}
	// Full queue: discard the oldest queued event to make room for the
	// newest. The reader may take a value between the two selects; that is
	// still a drop, so it is counted either way.
	select {
	case <-sub.ch:
		b.dropped.Add(1)
	default:
	}
	select {
	case sub.ch <- a:
	default:
		// Only reachable if a reader drained a value that raced the drain
		// above; never block on a client.
		b.dropped.Add(1)
	}
}

// SubscriberCount is the number of registered subscribers. It returns to zero
// once every client has disconnected — the leak assertion in tests.
func (b *Broadcaster) SubscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// Dropped counts events discarded because a subscriber's queue was full. A
// steadily rising value means at least one client cannot keep up with alert
// volume; it is exported to Prometheus as
// sorobeacon_alerts_stream_dropped_total.
func (b *Broadcaster) Dropped() uint64 {
	return b.dropped.Load()
}
