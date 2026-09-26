package broadcast

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func receive(t *testing.T, sub *Subscriber) (Alert, bool) {
	t.Helper()
	select {
	case a := <-sub.C:
		return a, true
	case <-time.After(time.Second):
		return Alert{}, false
	}
}

// TestBed publishes to subscribers with different monitor filters: every
// subscriber gets the alerts it asked for, and nothing else.
func TestPublishFansOutToMatchingSubscribers(t *testing.T) {
	b := New(4)
	all := b.Subscribe(0)
	one := b.Subscribe(1)
	two := b.Subscribe(2)
	defer all.Close()
	defer one.Close()
	defer two.Close()

	if got := b.SubscriberCount(); got != 3 {
		t.Fatalf("SubscriberCount = %d, want 3", got)
	}

	b.Publish(Alert{ID: 1, MonitorID: 1})
	b.Publish(Alert{ID: 2, MonitorID: 2})

	for _, want := range []int64{1, 2} {
		got, ok := receive(t, all)
		if !ok || got.ID != want {
			t.Fatalf("unfiltered subscriber got %+v ok=%v, want alert %d", got, ok, want)
		}
	}

	if got, ok := receive(t, one); !ok || got.ID != 1 {
		t.Fatalf("monitor 1 subscriber got %+v ok=%v, want alert 1", got, ok)
	}
	if got, ok := receive(t, two); !ok || got.ID != 2 {
		t.Fatalf("monitor 2 subscriber got %+v ok=%v, want alert 2", got, ok)
	}
	assertNothingMore(t, one, "monitor 1 subscriber")
	assertNothingMore(t, two, "monitor 2 subscriber")
}

// assertNothingMore fails if a subscriber has a queued alert it should not
// have received.
func assertNothingMore(t *testing.T, sub *Subscriber, who string) {
	t.Helper()
	select {
	case a := <-sub.C:
		t.Fatalf("%s received an unexpected alert: %+v", who, a)
	default:
	}
}

// The core backpressure guarantee: a subscriber that never reads does not
// make Publish block, and the queue keeps the newest events.
func TestPublishDropsOldestWhenQueueFull(t *testing.T) {
	b := New(2)
	sub := b.Subscribe(0)
	defer sub.Close()

	for i := int64(1); i <= 5; i++ {
		b.Publish(Alert{ID: i})
	}

	if got := b.Dropped(); got != 3 {
		t.Fatalf("Dropped = %d, want 3 (1, 2 and 3 evicted)", got)
	}
	first, ok := receive(t, sub)
	if !ok || first.ID != 4 {
		t.Fatalf("first queued alert = %+v ok=%v, want the post-drop alert 4", first, ok)
	}
	second, ok := receive(t, sub)
	if !ok || second.ID != 5 {
		t.Fatalf("second queued alert = %+v ok=%v, want the newest alert 5", second, ok)
	}
	assertNothingMore(t, sub, "full-queue subscriber")
}

func TestPublishNeverBlocksOnSlowSubscriber(t *testing.T) {
	b := New(1)
	sub := b.Subscribe(0)
	defer sub.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := int64(0); i < 100_000; i++ {
			b.Publish(Alert{ID: i})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a subscriber that never reads")
	}
	if b.Dropped() == 0 {
		t.Fatal("a full queue must count drops")
	}
}

func TestSubscriberCloseReturnsCountToZero(t *testing.T) {
	b := New(0)
	subs := []*Subscriber{b.Subscribe(0), b.Subscribe(1), b.Subscribe(2)}
	if got := b.SubscriberCount(); got != 3 {
		t.Fatalf("SubscriberCount = %d, want 3", got)
	}
	for _, s := range subs {
		s.Close()
	}
	if got := b.SubscriberCount(); got != 0 {
		t.Fatalf("SubscriberCount after Close = %d, want 0", got)
	}
	// Close is idempotent; calling it twice must not corrupt the count.
	subs[0].Close()
	if got := b.SubscriberCount(); got != 0 {
		t.Fatalf("SubscriberCount after a second Close = %d, want 0", got)
	}
}

// A closed subscriber must stop receiving, so a disconnected client cannot
// keep a goroutine busy draining a queue nobody owns.
func TestClosedSubscriberReceivesNothing(t *testing.T) {
	b := New(4)
	sub := b.Subscribe(1)
	sub.Close()
	b.Publish(Alert{ID: 1, MonitorID: 1})
	assertNothingMore(t, sub, "closed subscriber")
}

// Concurrent publishers and disconnects must not race on the subscriber set.
// Run with -race for the full effect.
func TestConcurrentPublishAndClose(t *testing.T) {
	b := New(4)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		sub := b.Subscribe(int64(i))
		wg.Add(2)
		go func(id int64) {
			defer wg.Done()
			for n := 0; n < 200; n++ {
				b.Publish(Alert{ID: int64(n), MonitorID: id})
			}
		}(int64(i))
		go func() {
			defer wg.Done()
			sub.Close()
		}()
	}
	wg.Wait()
	if got := b.SubscriberCount(); got != 0 {
		t.Fatalf("SubscriberCount = %d after every client closed, want 0", got)
	}
}

func TestAlertJSONShape(t *testing.T) {
	// The SSE payload is consumed by API clients, so its field names are part
	// of the contract. Pin them.
	raw, err := json.Marshal(Alert{ID: 1, MonitorID: 2, MonitorName: "ops", RuleID: 3, EventID: "ev"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"id"`, `"monitor_id"`, `"monitor_name"`, `"rule_id"`, `"event_id"`, `"created_at"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("marshalled alert %s missing %s", raw, key)
		}
	}
}
