package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/broadcast"
	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// concurrentFairnessStore is a thread-safe DispatchStore for multi-workspace
// fairness, retry-storm, digest, and escalation tests.
type concurrentFairnessStore struct {
	mu               sync.Mutex
	channelsByMon    map[int64][]store.Channel
	channelsByID     map[int64]store.Channel
	attempts         []store.DeliveryAttempt
	deadLetters      []store.DeadLetter
	health           []channelHealthCall
	digestRows       map[int64][]store.DigestAlert
	digestSeq        int64
	policies         map[int64]*store.EscalationPolicy
	runs             map[int64]*store.EscalationRun
	alerts           map[int64]*store.Alert
	completedEsc     []int64
}

func newConcurrentFairnessStore() *concurrentFairnessStore {
	return &concurrentFairnessStore{
		channelsByMon: make(map[int64][]store.Channel),
		channelsByID:  make(map[int64]store.Channel),
		digestRows:    make(map[int64][]store.DigestAlert),
		policies:      make(map[int64]*store.EscalationPolicy),
		runs:          make(map[int64]*store.EscalationRun),
		alerts:        make(map[int64]*store.Alert),
	}
}

func (s *concurrentFairnessStore) addChannel(monitorID int64, ch store.Channel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.channelsByMon[monitorID] = append(s.channelsByMon[monitorID], ch)
	s.channelsByID[ch.ID] = ch
}

func (s *concurrentFairnessStore) ListChannelsForMonitor(_ context.Context, monitorID int64) ([]store.Channel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	chs := s.channelsByMon[monitorID]
	out := make([]store.Channel, len(chs))
	copy(out, chs)
	return out, nil
}

func (s *concurrentFairnessStore) ListChannelsByIDs(_ context.Context, ids []int64) ([]store.Channel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.Channel, 0, len(ids))
	for _, id := range ids {
		if ch, ok := s.channelsByID[id]; ok && ch.Enabled {
			out = append(out, ch)
		}
	}
	return out, nil
}

func (s *concurrentFairnessStore) ListChannels(_ context.Context, enabledOnly bool) ([]store.Channel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.Channel, 0, len(s.channelsByID))
	for _, ch := range s.channelsByID {
		if enabledOnly && !ch.Enabled {
			continue
		}
		out = append(out, ch)
	}
	return out, nil
}

func (s *concurrentFairnessStore) RecordDeliveryAttempt(_ context.Context, d *store.DeliveryAttempt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts = append(s.attempts, *d)
	return nil
}

func (s *concurrentFairnessStore) CreateDeadLetter(_ context.Context, d *store.DeadLetter) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deadLetters = append(s.deadLetters, *d)
	return nil
}

func (s *concurrentFairnessStore) GetAlert(_ context.Context, id int64) (*store.Alert, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.alerts[id]; ok {
		cp := *a
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (s *concurrentFairnessStore) GetChannel(_ context.Context, id int64) (*store.Channel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.channelsByID[id]; ok {
		cp := ch
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (s *concurrentFairnessStore) ListDeliveryAttempts(_ context.Context, alertID int64, status string) ([]store.DeliveryAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.DeliveryAttempt
	for _, a := range s.attempts {
		if a.AlertID == alertID && (status == "" || a.Status == status) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *concurrentFairnessStore) DeleteDeadLetter(_ context.Context, _ int64) error { return nil }
func (s *concurrentFairnessStore) ListInhibitionsForTarget(context.Context, int64) ([]store.Inhibition, error) {
	return nil, nil
}
func (s *concurrentFairnessStore) RuleFiredWithin(context.Context, int64, time.Duration) (bool, error) {
	return false, nil
}
func (s *concurrentFairnessStore) ListRuleAlertTimes(context.Context, int64, time.Time) ([]time.Time, error) {
	return nil, nil
}
func (s *concurrentFairnessStore) MarkAlertInhibited(context.Context, int64, int64) error { return nil }
func (s *concurrentFairnessStore) ActiveMaintenanceWindow(context.Context, int64, string, time.Time) (*store.MaintenanceWindow, error) {
	return nil, nil
}
func (s *concurrentFairnessStore) SetAlertSuppressed(context.Context, int64, string) error {
	return nil
}

func (s *concurrentFairnessStore) RecordChannelHealth(_ context.Context, channelID int64, u store.ChannelHealthUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.health = append(s.health, channelHealthCall{channelID: channelID, update: u})
	return nil
}

func (s *concurrentFairnessStore) GetEscalationPolicyForMonitor(_ context.Context, monitorID int64) (*store.EscalationPolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.policies {
		if p.MonitorID == monitorID {
			return p, nil
		}
	}
	return nil, store.ErrNotFound
}

func (s *concurrentFairnessStore) GetEscalationPolicy(_ context.Context, policyID int64) (*store.EscalationPolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.policies[policyID]; ok {
		return p, nil
	}
	return nil, store.ErrNotFound
}

func (s *concurrentFairnessStore) ScheduleEscalation(_ context.Context, alertID, policyID int64, snapshot json.RawMessage, nextStep int, nextDue time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[alertID] = &store.EscalationRun{
		AlertID: alertID, PolicyID: policyID, NextStep: nextStep, NextDueAt: nextDue, Snapshot: snapshot,
	}
	return nil
}

func (s *concurrentFairnessStore) DueEscalations(_ context.Context, now time.Time, limit int) ([]store.EscalationRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.EscalationRun
	for _, r := range s.runs {
		if !r.NextDueAt.After(now) {
			out = append(out, *r)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *concurrentFairnessStore) AdvanceEscalation(_ context.Context, alertID int64, nextStep int, nextDue time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.runs[alertID]; ok {
		r.NextStep = nextStep
		r.NextDueAt = nextDue
	}
	return nil
}

func (s *concurrentFairnessStore) CompleteEscalation(_ context.Context, alertID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.runs, alertID)
	s.completedEsc = append(s.completedEsc, alertID)
	return nil
}

func (s *concurrentFairnessStore) PushDigestAlert(_ context.Context, channelID int64, payload json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.digestSeq++
	s.digestRows[channelID] = append(s.digestRows[channelID], store.DigestAlert{
		ID:        s.digestSeq,
		ChannelID: channelID,
		Payload:   payload,
		CreatedAt: time.Now().Add(-5 * time.Minute),
	})
	return nil
}

func (s *concurrentFairnessStore) ListDigestAlerts(_ context.Context, channelID int64) ([]store.DigestAlert, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := s.digestRows[channelID]
	out := make([]store.DigestAlert, len(rows))
	copy(out, rows)
	return out, nil
}

func (s *concurrentFairnessStore) DeleteDigestAlerts(_ context.Context, channelID int64, ids []int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	drop := make(map[int64]bool, len(ids))
	for _, id := range ids {
		drop[id] = true
	}
	var kept []store.DigestAlert
	for _, row := range s.digestRows[channelID] {
		if !drop[row.ID] {
			kept = append(kept, row)
		}
	}
	s.digestRows[channelID] = kept
	return nil
}

func (s *concurrentFairnessStore) snapshotCounts(channelID int64) (attempts, successes, deadLetters int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.attempts {
		if a.ChannelID == channelID {
			attempts++
			if a.Status == "success" {
				successes++
			}
		}
	}
	for _, dl := range s.deadLetters {
		if dl.ChannelID == channelID {
			deadLetters++
		}
	}
	return attempts, successes, deadLetters
}

type funcNotifier struct {
	fn func(ctx context.Context, a Alert) error
}

func (n *funcNotifier) Send(ctx context.Context, a Alert) error {
	return n.fn(ctx, a)
}

func TestStalledWorkspaceDoesNotDelayOtherWorkspaces(t *testing.T) {
	st := newConcurrentFairnessStore()
	st.addChannel(1, store.Channel{
		ID: 101, WorkspaceID: "stalled-ws", Name: "stalled-hook", Type: "stalled", Config: json.RawMessage(`{}`), Enabled: true,
	})
	st.addChannel(2, store.Channel{
		ID: 202, WorkspaceID: "healthy-ws", Name: "fast-hook", Type: "fast", Config: json.RawMessage(`{}`), Enabled: true,
	})

	stalledEntered := make(chan struct{}, 4)
	releaseStalled := make(chan struct{})
	var fastDelivered atomic.Int64

	f := &Factory{constructors: map[string]Constructor{}}
	f.Register("stalled", func(json.RawMessage) (Notifier, error) {
		return &funcNotifier{fn: func(ctx context.Context, _ Alert) error {
			stalledEntered <- struct{}{}
			select {
			case <-releaseStalled:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}, nil
	})
	f.Register("fast", func(json.RawMessage) (Notifier, error) {
		return &funcNotifier{fn: func(_ context.Context, _ Alert) error {
			fastDelivered.Add(1)
			return nil
		}}, nil
	})

	m := metrics.New().WithNetwork("testnet")
	d := NewDispatcher(st, f, slog.New(slog.DiscardHandler)).
		WithMetrics(m).
		WithFairness(FairnessConfig{
			MaxWorkspaceInFlight: 2,
			MaxWorkspaceQueue:    4,
			MaxChannelInFlight:   1,
			MaxChannelQueue:      4,
		})

	// Launch 2 deliveries in stalled-ws so its channel is actively blocked and queued.
	var stalledWG sync.WaitGroup
	for i := 1; i <= 2; i++ {
		stalledWG.Add(1)
		go func(id int64) {
			defer stalledWG.Done()
			d.Dispatch(workspace.With(context.Background(), "stalled-ws"), Alert{ID: id, MonitorID: 1, MonitorName: "stalled-mon"})
		}(int64(i))
	}

	// Wait until stalled-ws has entered its notifier and blocked.
	<-stalledEntered
	require.Eventually(t, func() bool {
		b := d.ChannelBacklog(101)
		return b.InFlight == 1 && b.QueueDepth == 1
	}, time.Second, 2*time.Millisecond)

	// Measure delivery latency for healthy-ws while stalled-ws is completely hung.
	start := time.Now()
	for i := 1; i <= 5; i++ {
		d.Dispatch(workspace.With(context.Background(), "healthy-ws"), Alert{ID: int64(100 + i), MonitorID: 2, MonitorName: "healthy-mon"})
	}
	healthyLatency := time.Since(start)

	assert.Equal(t, int64(5), fastDelivered.Load(), "all healthy-ws alerts must deliver while stalled-ws is blocked")
	assert.Less(t, healthyLatency, 50*time.Millisecond, "stalled workspace must not add tail latency to healthy workspace")

	// Verify live backlog snapshot and Prometheus metrics while stalled-ws is still blocked.
	wsBacklog := d.WorkspaceBacklog("stalled-ws")
	assert.Equal(t, 2, wsBacklog.InFlight)
	chBacklog := d.ChannelBacklog(101)
	assert.Equal(t, 1, chBacklog.InFlight)
	assert.Equal(t, 1, chBacklog.QueueDepth)

	close(releaseStalled)
	stalledWG.Wait()

	_, stalledSuccess, stalledDL := st.snapshotCounts(101)
	assert.Equal(t, 2, stalledSuccess)
	assert.Equal(t, 0, stalledDL)
}

func TestNoisyWorkspaceBurstOverflowsToDeadLetterWithoutAffectingQuietWorkspace(t *testing.T) {
	const secretToken = "SECRET_WEBHOOK_BEARER_999"
	st := newConcurrentFairnessStore()
	st.addChannel(1, store.Channel{
		ID:          10,
		WorkspaceID: "noisy-ws",
		Name:        "noisy-hook",
		Type:        "noisy",
		Config:      json.RawMessage(`{"token":"` + secretToken + `"}`),
		Enabled:     true,
	})
	st.addChannel(2, store.Channel{
		ID:          20,
		WorkspaceID: "quiet-ws",
		Name:        "quiet-hook",
		Type:        "quiet",
		Config:      json.RawMessage(`{}`),
		Enabled:     true,
	})

	noisyBlocked := make(chan struct{}, 10)
	releaseNoisy := make(chan struct{})
	var noisyOrderMu sync.Mutex
	var noisyDeliveredOrder []int64
	var quietDelivered atomic.Int64

	f := &Factory{constructors: map[string]Constructor{}}
	f.Register("noisy", func(json.RawMessage) (Notifier, error) {
		return &funcNotifier{fn: func(ctx context.Context, a Alert) error {
			noisyBlocked <- struct{}{}
			select {
			case <-releaseNoisy:
				noisyOrderMu.Lock()
				noisyDeliveredOrder = append(noisyDeliveredOrder, a.ID)
				noisyOrderMu.Unlock()
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}, nil
	})
	f.Register("quiet", func(json.RawMessage) (Notifier, error) {
		return &funcNotifier{fn: func(_ context.Context, _ Alert) error {
			quietDelivered.Add(1)
			return nil
		}}, nil
	})

	var logs bytes.Buffer
	m := metrics.New().WithNetwork("testnet")
	d := NewDispatcher(st, f, slog.New(slog.NewTextHandler(&logs, nil))).
		WithMetrics(m).
		WithDisableAfterFailures(5).
		WithFairness(FairnessConfig{
			MaxWorkspaceInFlight: 2,
			MaxWorkspaceQueue:    3,
			MaxChannelInFlight:   2,
			MaxChannelQueue:      3,
		})

	// Flood noisy-ws with 12 alerts via non-blocking Submit (admits 2 in-flight + 3 queued = 5; sheds 7).
	admitted := 0
	shed := 0
	for i := 1; i <= 12; i++ {
		if d.Submit(workspace.With(context.Background(), "noisy-ws"), Alert{ID: int64(i), MonitorID: 1, MonitorName: "noisy"}) {
			admitted++
		} else {
			shed++
		}
	}
	assert.Equal(t, 5, admitted, "2 active workers + 3 queued slots must be admitted")
	assert.Equal(t, 7, shed, "7 excess alerts beyond workspace queue bound must be shed immediately")

	// Wait for the 2 in-flight workers on noisy-ws to enter Send.
	<-noisyBlocked
	<-noisyBlocked

	// Meanwhile quiet-ws submits 4 alerts and they complete immediately.
	startQuiet := time.Now()
	for i := 1; i <= 4; i++ {
		ok := d.Submit(workspace.With(context.Background(), "quiet-ws"), Alert{ID: int64(200 + i), MonitorID: 2, MonitorName: "quiet"})
		require.True(t, ok)
	}
	require.Eventually(t, func() bool {
		return quietDelivered.Load() == 4
	}, time.Second, 2*time.Millisecond)
	assert.Less(t, time.Since(startQuiet), 50*time.Millisecond)

	// Unblock noisy-ws and wait for all admitted background deliveries to drain.
	close(releaseNoisy)
	require.NoError(t, d.Wait(context.Background()))

	noisyAttempts, noisySuccesses, noisyDLs := st.snapshotCounts(10)
	assert.Equal(t, 12, noisyAttempts, "5 delivered + 7 shed attempts must all be recorded")
	assert.Equal(t, 5, noisySuccesses, "5 admitted alerts must succeed once unblocked")
	assert.Equal(t, 7, noisyDLs, "7 overflowed alerts must be persisted as dead letters")
	assert.Equal(t, []int64{1, 2, 3, 4, 5}, func() []int64 {
		// Workers 1 and 2 start concurrently on alerts 1 & 2, then drain 3, 4, 5 in FIFO order.
		noisyOrderMu.Lock()
		defer noisyOrderMu.Unlock()
		assert.ElementsMatch(t, []int64{1, 2}, noisyDeliveredOrder[:2])
		assert.Equal(t, []int64{3, 4, 5}, noisyDeliveredOrder[2:])
		return []int64{1, 2, 3, 4, 5}
	}())

	// Verify Prometheus metrics recorded the workspace overflow and hashed the fairness workspace label.
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	hashedNoisy := metrics.HashWorkspaceLabel("noisy-ws")
	assert.Contains(t, body, `sorobeacon_notify_fairness_overflow_total{network="testnet",reason="workspace_queue_full",workspace="`+hashedNoisy+`"} 7`)
	assert.NotContains(t, body, `sorobeacon_notify_fairness_overflow_total{network="testnet",reason="workspace_queue_full",workspace="noisy-ws"}`)
	assert.NotContains(t, logs.String(), secretToken)
}

func TestRetryStormDoesNotStarveFirstAttemptDeliveries(t *testing.T) {
	st := newConcurrentFairnessStore()
	// Five flaky monitors/channels in ws-storm (each channel stays below the per-channel
	// circuit breaker threshold of 3 on attempt 1, so all 5 enter retry attempt 2 together).
	for i := int64(1); i <= 5; i++ {
		st.addChannel(i, store.Channel{
			ID: 10 + i, WorkspaceID: "ws-storm", Name: "flaky-hook", Type: "flaky", Config: json.RawMessage(`{}`), Enabled: true,
		})
	}
	st.addChannel(99, store.Channel{
		ID: 99, WorkspaceID: "ws-storm", Name: "healthy-sibling", Type: "healthy", Config: json.RawMessage(`{}`), Enabled: true,
	})

	var flakyCallsByAlert sync.Map
	retryInProgress := make(chan struct{}, 16)
	releaseRetries := make(chan struct{})
	var healthySiblingDelivered atomic.Int64
	var freshOnFlakyDelivered atomic.Int64

	f := &Factory{constructors: map[string]Constructor{}}
	f.Register("flaky", func(json.RawMessage) (Notifier, error) {
		return &funcNotifier{fn: func(ctx context.Context, a Alert) error {
			// Alert ID >= 500 is a fresh first-attempt alert sent during the retry storm.
			if a.ID >= 500 {
				freshOnFlakyDelivered.Add(1)
				return nil
			}
			val, _ := flakyCallsByAlert.LoadOrStore(a.ID, new(atomic.Int32))
			attemptNum := val.(*atomic.Int32).Add(1)
			if attemptNum == 1 {
				// First attempt fails fast with a retryable 503 error to trigger the retry storm.
				return &HTTPStatusError{StatusCode: http.StatusServiceUnavailable, Body: "try again"}
			}
			// Retry attempts block in the retry lane until released.
			retryInProgress <- struct{}{}
			select {
			case <-releaseRetries:
				return &HTTPStatusError{StatusCode: http.StatusServiceUnavailable, Body: "still down"}
			case <-ctx.Done():
				return ctx.Err()
			}
		}}, nil
	})
	f.Register("healthy", func(json.RawMessage) (Notifier, error) {
		return &funcNotifier{fn: func(_ context.Context, _ Alert) error {
			healthySiblingDelivered.Add(1)
			return nil
		}}, nil
	})

	m := metrics.New().WithNetwork("testnet")
	d := NewDispatcher(st, f, slog.New(slog.DiscardHandler)).
		WithMetrics(m).
		WithFairness(FairnessConfig{
			MaxWorkspaceInFlight:      4,
			MaxWorkspaceQueue:         8,
			MaxWorkspaceRetryInFlight: 1,
			MaxWorkspaceRetryQueue:    1,
			MaxChannelInFlight:        2,
			MaxChannelQueue:           8,
		})
	d.MaxAttempts = 2
	d.BaseBackoff = 10 * time.Millisecond

	// Start 5 alerts across ws-storm's flaky channels so they all fail attempt 1 and storm the retry lane.
	var stormWG sync.WaitGroup
	for i := int64(1); i <= 5; i++ {
		stormWG.Add(1)
		go func(id int64) {
			defer stormWG.Done()
			d.Dispatch(workspace.With(context.Background(), "ws-storm"), Alert{ID: id, MonitorID: id})
		}(i)
	}

	// Wait until the retry lane is occupied (1 retry in flight, 1 queued, 3 excess retries shed to dead letters).
	<-retryInProgress
	require.Eventually(t, func() bool {
		b := d.WorkspaceBacklog("ws-storm")
		return b.RetryInFlight == 1 && b.OverflowTotal >= 3
	}, time.Second, 5*time.Millisecond)

	// While the retry storm is active and holding the retry lane, send fresh first-attempt alerts!
	startFresh := time.Now()
	for i := 1; i <= 3; i++ {
		d.Dispatch(workspace.With(context.Background(), "ws-storm"), Alert{ID: int64(100 + i), MonitorID: 99})
	}
	d.Dispatch(workspace.With(context.Background(), "ws-storm"), Alert{ID: 501, MonitorID: 1})
	freshElapsed := time.Since(startFresh)

	assert.Equal(t, int64(3), healthySiblingDelivered.Load(), "first-attempt alerts on sibling channel must not be starved by retry storm")
	assert.Equal(t, int64(1), freshOnFlakyDelivered.Load(), "first-attempt alert on the same channel must use reserved first-attempt capacity")
	assert.Less(t, freshElapsed, 50*time.Millisecond, "first-attempt deliveries must complete promptly during retry storm")

	close(releaseRetries)
	stormWG.Wait()

	// Verify that all 5 storming alerts were dead-lettered (3 via retry_queue_full overflow, 2 after exhausting MaxAttempts).
	totalFlakyDeadLetters := 0
	for i := int64(1); i <= 5; i++ {
		_, _, dl := st.snapshotCounts(10 + i)
		totalFlakyDeadLetters += dl
	}
	assert.Equal(t, 5, totalFlakyDeadLetters, "all 5 storming alerts must end up dead-lettered")
}

func TestChannelConcurrencyAndFIFOHeadOfLineOrdering(t *testing.T) {
	st := newConcurrentFairnessStore()
	st.addChannel(1, store.Channel{
		ID: 77, WorkspaceID: "ws-fifo", Name: "ordered-hook", Type: "ordered", Config: json.RawMessage(`{}`), Enabled: true,
	})

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var activeInChannel atomic.Int32
	var maxObservedInChannel atomic.Int32
	var orderMu sync.Mutex
	var deliveredOrder []int64

	f := &Factory{constructors: map[string]Constructor{}}
	f.Register("ordered", func(json.RawMessage) (Notifier, error) {
		return &funcNotifier{fn: func(_ context.Context, a Alert) error {
			cur := activeInChannel.Add(1)
			for {
				prev := maxObservedInChannel.Load()
				if cur <= prev || maxObservedInChannel.CompareAndSwap(prev, cur) {
					break
				}
			}
			defer activeInChannel.Add(-1)

			if a.ID == 1 {
				close(firstEntered)
				<-releaseFirst
			}
			orderMu.Lock()
			deliveredOrder = append(deliveredOrder, a.ID)
			orderMu.Unlock()
			return nil
		}}, nil
	})

	d := NewDispatcher(st, f, slog.New(slog.DiscardHandler)).
		WithFairness(FairnessConfig{
			MaxWorkspaceInFlight: 8,
			MaxWorkspaceQueue:    16,
			MaxChannelInFlight:   1,
			MaxChannelQueue:      3,
		})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		d.Dispatch(context.Background(), Alert{ID: 1, MonitorID: 1})
	}()
	<-firstEntered

	// Enqueue alerts 2, 3, 4 in order so they line up in channel 77's FIFO waiter slice.
	for id := int64(2); id <= 4; id++ {
		prevDepth := int(id - 2)
		require.Eventually(t, func() bool {
			return d.ChannelBacklog(77).QueueDepth == prevDepth
		}, time.Second, time.Millisecond)
		wg.Add(1)
		go func(alertID int64) {
			defer wg.Done()
			d.Dispatch(context.Background(), Alert{ID: alertID, MonitorID: 1})
		}(id)
	}

	require.Eventually(t, func() bool {
		return d.ChannelBacklog(77).QueueDepth == 3
	}, time.Second, time.Millisecond)

	// Alert 5 exceeds MaxChannelQueue (3) and must fail immediately into dead letters.
	d.Dispatch(context.Background(), Alert{ID: 5, MonitorID: 1})
	_, _, dls := st.snapshotCounts(77)
	assert.Equal(t, 1, dls, "alert 5 must overflow channel queue to dead letter immediately")

	close(releaseFirst)
	wg.Wait()

	assert.Equal(t, int32(1), maxObservedInChannel.Load(), "channel concurrency must never exceed MaxChannelInFlight=1")
	orderMu.Lock()
	assert.Equal(t, []int64{1, 2, 3, 4}, deliveredOrder, "channel deliveries must preserve strict FIFO head-of-line order")
	orderMu.Unlock()
}

func TestNonBlockingIngestionAndBroadcastWithStalledConsumer(t *testing.T) {
	st := newConcurrentFairnessStore()
	st.addChannel(1, store.Channel{
		ID: 1, WorkspaceID: "ws-stalled", Name: "hung-webhook", Type: "hung", Config: json.RawMessage(`{}`), Enabled: true,
	})

	release := make(chan struct{})
	defer close(release)

	f := &Factory{constructors: map[string]Constructor{}}
	f.Register("hung", func(json.RawMessage) (Notifier, error) {
		return &funcNotifier{fn: func(ctx context.Context, _ Alert) error {
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}, nil
	})

	d := NewDispatcher(st, f, slog.New(slog.DiscardHandler)).
		WithFairness(FairnessConfig{
			MaxWorkspaceInFlight: 2,
			MaxWorkspaceQueue:    4,
			AsyncDispatch:        true,
		})

	b := broadcast.New(0)
	sub := b.Subscribe(0)
	defer sub.Close()

	start := time.Now()
	for i := 1; i <= 20; i++ {
		b.Publish(broadcast.Alert{ID: int64(i), MonitorID: 1, MonitorName: "m"})
		d.Dispatch(workspace.With(context.Background(), "ws-stalled"), Alert{ID: int64(i), MonitorID: 1, MonitorName: "m"})
	}
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 30*time.Millisecond, "live broadcast and async notification ingestion must never block on hung webhooks")
	select {
	case got := <-sub.C:
		assert.Equal(t, int64(1), got.ID)
	default:
		t.Fatal("expected broadcast subscriber to receive published alert without delay")
	}
}

func TestDigestAndEscalationRespectWorkspaceFairness(t *testing.T) {
	st := newConcurrentFairnessStore()
	// Digest channels across two workspaces
	st.addChannel(1, store.Channel{
		ID: 301, WorkspaceID: "ws-slow", Name: "slow-digest", Type: "slow", Config: json.RawMessage(`{}`),
		Enabled: true, DigestMode: store.DigestModeWindow, DigestWindowSeconds: 60,
	})
	st.addChannel(2, store.Channel{
		ID: 302, WorkspaceID: "ws-fast", Name: "fast-digest", Type: "fast", Config: json.RawMessage(`{}`),
		Enabled: true, DigestMode: store.DigestModeWindow, DigestWindowSeconds: 60,
	})
	// Escalation channels across two workspaces
	st.addChannel(3, store.Channel{
		ID: 401, WorkspaceID: "ws-slow", Name: "slow-esc", Type: "slow", Config: json.RawMessage(`{}`), Enabled: true,
	})
	st.addChannel(4, store.Channel{
		ID: 402, WorkspaceID: "ws-fast", Name: "fast-esc", Type: "fast", Config: json.RawMessage(`{}`), Enabled: true,
	})

	slowEntered := make(chan struct{}, 4)
	releaseSlow := make(chan struct{})
	var fastCalls atomic.Int64
	fastDone := make(chan struct{}, 4)

	f := &Factory{constructors: map[string]Constructor{}}
	f.Register("slow", func(json.RawMessage) (Notifier, error) {
		return &funcNotifier{fn: func(ctx context.Context, _ Alert) error {
			slowEntered <- struct{}{}
			select {
			case <-releaseSlow:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}, nil
	})
	f.Register("fast", func(json.RawMessage) (Notifier, error) {
		return &funcNotifier{fn: func(_ context.Context, _ Alert) error {
			fastCalls.Add(1)
			fastDone <- struct{}{}
			return nil
		}}, nil
	})

	d := NewDispatcher(st, f, slog.New(slog.DiscardHandler)).WithDigestQueue(st)
	require.NoError(t, st.PushDigestAlert(context.Background(), 301, json.RawMessage(`{"id":1,"monitor_id":1,"monitor_name":"m1","event_name":"e1"}`)))
	require.NoError(t, st.PushDigestAlert(context.Background(), 302, json.RawMessage(`{"id":2,"monitor_id":2,"monitor_name":"m2","event_name":"e2"}`)))

	flushDone := make(chan struct{})
	go func() {
		defer close(flushDone)
		d.FlushDigests(context.Background(), time.Now())
	}()

	<-slowEntered
	select {
	case <-fastDone:
		assert.Equal(t, int64(1), fastCalls.Load(), "ws-fast digest must flush without waiting for ws-slow digest")
	case <-time.After(200 * time.Millisecond):
		t.Fatal("ws-fast digest was blocked by ws-slow digest")
	}
	close(releaseSlow)
	<-flushDone

	// Now verify multi-workspace due escalations also run without head-of-line blocking across workspaces.
	releaseSlow = make(chan struct{})
	now := time.Now()
	st.policies[1] = &store.EscalationPolicy{ID: 1, MonitorID: 3, Steps: []store.EscalationStep{{ChannelIDs: []int64{401}}}}
	st.policies[2] = &store.EscalationPolicy{ID: 2, MonitorID: 4, Steps: []store.EscalationStep{{ChannelIDs: []int64{402}}}}
	st.alerts[10] = &store.Alert{ID: 10, MonitorID: 3}
	st.alerts[20] = &store.Alert{ID: 20, MonitorID: 4}
	require.NoError(t, st.ScheduleEscalation(context.Background(), 10, 1, json.RawMessage(`{"id":10,"monitor_id":3}`), 0, now.Add(-time.Minute)))
	require.NoError(t, st.ScheduleEscalation(context.Background(), 20, 2, json.RawMessage(`{"id":20,"monitor_id":4}`), 0, now.Add(-time.Minute)))

	escDone := make(chan struct{})
	go func() {
		defer close(escDone)
		_, _ = d.ProcessDueEscalations(context.Background(), now)
	}()

	<-slowEntered
	select {
	case <-fastDone:
		assert.Equal(t, int64(2), fastCalls.Load(), "ws-fast escalation step must fire without waiting for ws-slow escalation step")
	case <-time.After(200 * time.Millisecond):
		t.Fatal("ws-fast escalation was blocked by ws-slow escalation")
	}
	close(releaseSlow)
	<-escDone
}
