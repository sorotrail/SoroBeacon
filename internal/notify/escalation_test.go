package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// escalationStateStore is a stateful in-memory DispatchStore for the engine
// tests. The plain fakeDispatchStore above returns nil from every escalation
// method, which is right for the flat-fan-out tests but cannot express an
// escalation in flight. This one owns alerts, policies and runs so a test can
// advance the clock and observe exactly which step would fire.
//
// Every method locks: the concurrent-run test drives two ProcessDueEscalations
// calls at once, and the race detector is part of the contract.
type escalationStateStore struct {
	*fakeDispatchStore
	mu       sync.Mutex
	alerts   map[int64]*store.Alert
	policies map[int64]*store.EscalationPolicy
	runs     map[int64]*store.EscalationRun

	scheduled []scheduledEscalation
	advanced  []advancedEscalation
	completed []int64

	// dueHook, when set, runs after DueEscalations has built its result but
	// before it returns. It is how a test lands an acknowledgement in the
	// window between the due read and the delivery.
	dueHook func()
	// dueGate, when set, holds each DueEscalations caller until the test
	// releases it, so two callers can both read the same due row.
	dueGate *dueGate

	getAlertErr error
}

type scheduledEscalation struct {
	alertID  int64
	policyID int64
	nextStep int
	nextDue  time.Time
}

type advancedEscalation struct {
	alertID  int64
	nextStep int
	nextDue  time.Time
}

type dueGate struct {
	arrived chan struct{}
	release chan struct{}
}

func newEscalationStateStore() *escalationStateStore {
	return &escalationStateStore{
		fakeDispatchStore: &fakeDispatchStore{},
		alerts:            map[int64]*store.Alert{},
		policies:          map[int64]*store.EscalationPolicy{},
		runs:              map[int64]*store.EscalationRun{},
	}
}

func (s *escalationStateStore) RecordDeliveryAttempt(_ context.Context, d *store.DeliveryAttempt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts = append(s.attempts, *d)
	return nil
}

func (s *escalationStateStore) RecordChannelHealth(_ context.Context, channelID int64, u store.ChannelHealthUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.health = append(s.health, channelHealthCall{channelID: channelID, update: u})
	return nil
}

func (s *escalationStateStore) GetAlert(_ context.Context, id int64) (*store.Alert, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getAlertErr != nil {
		return nil, s.getAlertErr
	}
	a, ok := s.alerts[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *a
	return &cp, nil
}

func (s *escalationStateStore) GetEscalationPolicy(_ context.Context, id int64) (*store.EscalationPolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.policies[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return p, nil
}

func (s *escalationStateStore) ScheduleEscalation(_ context.Context, alertID, policyID int64, snapshot json.RawMessage, nextStep int, nextDue time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[alertID] = &store.EscalationRun{
		AlertID: alertID, PolicyID: policyID, NextStep: nextStep,
		NextDueAt: nextDue, Snapshot: snapshot,
	}
	s.scheduled = append(s.scheduled, scheduledEscalation{
		alertID: alertID, policyID: policyID, nextStep: nextStep, nextDue: nextDue,
	})
	return nil
}

func (s *escalationStateStore) DueEscalations(_ context.Context, now time.Time, limit int) ([]store.EscalationRun, error) {
	s.mu.Lock()
	var out []store.EscalationRun
	for _, run := range s.runs {
		// Mirror the SQL predicate: an acknowledged alert is not due.
		if a, ok := s.alerts[run.AlertID]; ok && a.AcknowledgedAt != nil {
			continue
		}
		if run.NextDueAt.After(now) {
			continue
		}
		out = append(out, *run)
	}
	hook := s.dueHook
	gate := s.dueGate
	s.mu.Unlock()

	if gate != nil {
		gate.arrived <- struct{}{}
		<-gate.release
	}
	if hook != nil {
		hook()
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *escalationStateStore) AdvanceEscalation(_ context.Context, alertID int64, nextStep int, nextDue time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if run, ok := s.runs[alertID]; ok {
		run.NextStep = nextStep
		run.NextDueAt = nextDue
	}
	s.advanced = append(s.advanced, advancedEscalation{alertID: alertID, nextStep: nextStep, nextDue: nextDue})
	return nil
}

func (s *escalationStateStore) CompleteEscalation(_ context.Context, alertID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.runs, alertID)
	s.completed = append(s.completed, alertID)
	return nil
}

// seed adds a policy, an unacknowledged alert and (optionally) a run already
// in flight at nextStep.
func (s *escalationStateStore) seed(t *testing.T, policy *store.EscalationPolicy, alertID int64, nextStep int, nextDue time.Time) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policies[policy.ID] = policy
	s.alerts[alertID] = &store.Alert{ID: alertID, MonitorID: policy.MonitorID}
	if nextStep >= 0 {
		snapshot, err := json.Marshal(Alert{ID: alertID, MonitorID: policy.MonitorID, MonitorName: "m"})
		require.NoError(t, err)
		s.runs[alertID] = &store.EscalationRun{
			AlertID: alertID, PolicyID: policy.ID, NextStep: nextStep,
			NextDueAt: nextDue, Snapshot: snapshot,
		}
	}
}

func (s *escalationStateStore) setAcknowledged(alertID int64, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.alerts[alertID]; ok {
		a.AcknowledgedAt = &at
	}
}

func (s *escalationStateStore) attemptsFor(channelID int64) []store.DeliveryAttempt {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.DeliveryAttempt
	for _, a := range s.attempts {
		if a.ChannelID == channelID {
			out = append(out, a)
		}
	}
	return out
}

// scriptedFactory returns one notifier for every channel, which is enough
// because these tests assert on call order, not on which constructor ran.
func scriptedFactory(n Notifier) *Factory {
	f := &Factory{constructors: map[string]Constructor{}}
	f.Register("mock", func(json.RawMessage) (Notifier, error) { return n, nil })
	return f
}

func newEscalationTestDispatcher(st *escalationStateStore, n Notifier) *Dispatcher {
	d := NewDispatcher(st, scriptedFactory(n), slog.New(slog.DiscardHandler))
	d.BaseBackoff = time.Millisecond
	d.EscalationInterval = time.Hour
	return d
}

func escalationPolicy(steps ...store.EscalationStep) *store.EscalationPolicy {
	for i := range steps {
		steps[i].Position = i
	}
	return &store.EscalationPolicy{ID: 7, MonitorID: 1, Steps: steps}
}

// alertSnapshot is the notification the engine replays from a stored run.
func alertSnapshot(t *testing.T, alertID int64) Alert {
	t.Helper()
	return Alert{ID: alertID, MonitorID: 1, MonitorName: "m", EventID: "ev-1"}
}

// TestEscalationOrderDelaysAndFinalStep pins the scheduling contract with an
// injected clock: step 0 fires with the alert, each later step waits its
// configured delay measured from when the previous step actually fired, and
// the final step completes the run so it can never repeat.
func TestEscalationOrderDelaysAndFinalStep(t *testing.T) {
	st := newEscalationStateStore()
	st.channels = []store.Channel{mockChannel(1), mockChannel(2), mockChannel(3)}
	n := &mockNotifier{}
	d := newEscalationTestDispatcher(st, n)

	t0 := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	d.WithClock(func() time.Time { return t0 })
	policy := escalationPolicy(
		store.EscalationStep{ChannelIDs: []int64{1}},
		store.EscalationStep{DelaySeconds: 60, ChannelIDs: []int64{2}},
		store.EscalationStep{DelaySeconds: 300, ChannelIDs: []int64{3}},
	)
	alert := alertSnapshot(t, 10)
	st.seed(t, policy, 10, -1, time.Time{})

	d.startEscalation(context.Background(), alert, policy)
	assert.Equal(t, 1, n.calls, "step 0 fires with the alert")
	require.Len(t, st.scheduled, 1)
	assert.Equal(t, 1, st.scheduled[0].nextStep)
	assert.Equal(t, t0.Add(60*time.Second), st.scheduled[0].nextDue)

	// Before the delay: nothing. At the delay: step 1, and step 2 is measured
	// from that firing instant (t0+60s), not from the alert.
	got, err := d.ProcessDueEscalations(context.Background(), t0.Add(59*time.Second))
	require.NoError(t, err)
	assert.Zero(t, got)
	assert.Equal(t, 1, n.calls)

	got, err = d.ProcessDueEscalations(context.Background(), t0.Add(60*time.Second))
	require.NoError(t, err)
	assert.Equal(t, 1, got)
	assert.Equal(t, 2, n.calls)
	require.Len(t, st.advanced, 1)
	assert.Equal(t, 2, st.advanced[0].nextStep)
	assert.Equal(t, t0.Add(60*time.Second).Add(300*time.Second), st.advanced[0].nextDue,
		"later delays compose from the previous step's actual firing time")

	got, err = d.ProcessDueEscalations(context.Background(), t0.Add(359*time.Second))
	require.NoError(t, err)
	assert.Zero(t, got)

	got, err = d.ProcessDueEscalations(context.Background(), t0.Add(360*time.Second))
	require.NoError(t, err)
	assert.Equal(t, 1, got)
	assert.Equal(t, 3, n.calls)
	assert.Equal(t, []int64{10}, st.completed, "the final step completes the run")

	// A completed run cannot fire again, even with an arbitrarily late clock.
	got, err = d.ProcessDueEscalations(context.Background(), t0.Add(24*time.Hour))
	require.NoError(t, err)
	assert.Zero(t, got)
	assert.Equal(t, 3, n.calls)
}

// TestStartEscalationIgnoresStepZeroDelay pins a behaviour that is easy to
// misread from the API: the first step's delay_seconds is stored but not used.
// Step 0 is relative to the alert, so it fires immediately.
func TestStartEscalationIgnoresStepZeroDelay(t *testing.T) {
	st := newEscalationStateStore()
	st.channels = []store.Channel{mockChannel(1), mockChannel(2)}
	n := &mockNotifier{}
	d := newEscalationTestDispatcher(st, n)
	t0 := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	d.WithClock(func() time.Time { return t0 })

	policy := escalationPolicy(
		store.EscalationStep{DelaySeconds: 600, ChannelIDs: []int64{1}},
		store.EscalationStep{DelaySeconds: 30, ChannelIDs: []int64{2}},
	)
	st.seed(t, policy, 11, -1, time.Time{})

	d.startEscalation(context.Background(), alertSnapshot(t, 11), policy)

	assert.Equal(t, 1, n.calls, "step 0's delay is ignored")
	require.Len(t, st.scheduled, 1)
	assert.Equal(t, t0.Add(30*time.Second), st.scheduled[0].nextDue,
		"step 1's delay is relative to when step 0 fired")
}

// TestEscalationStopsWhenAcknowledgedBeforeDue covers the ordinary stop: the
// due query excludes an acknowledged alert, so the step never reaches delivery.
func TestEscalationStopsWhenAcknowledgedBeforeDue(t *testing.T) {
	st := newEscalationStateStore()
	st.channels = []store.Channel{mockChannel(2)}
	n := &mockNotifier{}
	d := newEscalationTestDispatcher(st, n)
	policy := escalationPolicy(
		store.EscalationStep{ChannelIDs: []int64{1}},
		store.EscalationStep{DelaySeconds: 60, ChannelIDs: []int64{2}},
	)
	st.seed(t, policy, 12, 1, time.Date(2026, 10, 17, 12, 1, 0, 0, time.UTC))
	st.setAcknowledged(12, time.Date(2026, 10, 17, 12, 0, 30, 0, time.UTC))

	got, err := d.ProcessDueEscalations(context.Background(), time.Date(2026, 10, 17, 13, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.Zero(t, got)
	assert.Zero(t, n.calls)
}

// TestEscalationStopsWhenAcknowledgedBetweenDueAndDelivery reproduces the
// race the due query cannot close: the row is read as unacknowledged, the
// operator acknowledges, and only then does the engine deliver. The
// pre-delivery re-read must stop the step.
func TestEscalationStopsWhenAcknowledgedBetweenDueAndDelivery(t *testing.T) {
	st := newEscalationStateStore()
	st.channels = []store.Channel{mockChannel(2)}
	n := &mockNotifier{}
	d := newEscalationTestDispatcher(st, n)
	policy := escalationPolicy(
		store.EscalationStep{ChannelIDs: []int64{1}},
		store.EscalationStep{DelaySeconds: 60, ChannelIDs: []int64{2}},
	)
	due := time.Date(2026, 10, 17, 12, 1, 0, 0, time.UTC)
	st.seed(t, policy, 13, 1, due)
	st.dueHook = func() {
		st.setAcknowledged(13, time.Date(2026, 10, 17, 12, 0, 30, 0, time.UTC))
	}

	got, err := d.ProcessDueEscalations(context.Background(), due)
	require.NoError(t, err)
	assert.Zero(t, got)
	assert.Zero(t, n.calls, "an acknowledgement that lands after the due read must still stop the step")
	assert.Equal(t, []int64{13}, st.completed, "the engine finishes the acknowledged run")
}

// TestEscalationDoesNotStopOnResolvedOrRetracted documents a premise from the
// issue: there is no "resolved" alert state. Neither a retracted (reorged)
// alert nor a suppressed one is an acknowledgement, and neither halts a
// scheduled step. Acknowledgement is the only alert-side stop.
func TestEscalationDoesNotStopOnResolvedOrRetracted(t *testing.T) {
	st := newEscalationStateStore()
	st.channels = []store.Channel{mockChannel(2)}
	n := &mockNotifier{}
	d := newEscalationTestDispatcher(st, n)
	policy := escalationPolicy(
		store.EscalationStep{ChannelIDs: []int64{1}},
		store.EscalationStep{DelaySeconds: 60, ChannelIDs: []int64{2}},
	)
	due := time.Date(2026, 10, 17, 12, 1, 0, 0, time.UTC)
	st.seed(t, policy, 14, 1, due)
	retracted := due.Add(-time.Minute)
	st.mu.Lock()
	st.alerts[14].RetractedAt = &retracted
	st.alerts[14].Suppressed = true
	st.mu.Unlock()

	got, err := d.ProcessDueEscalations(context.Background(), due)
	require.NoError(t, err)
	assert.Equal(t, 1, got)
	assert.Equal(t, 1, n.calls, "only AcknowledgedAt stops an escalation")
}

// TestDeliveryFailureDoesNotHaltEscalation pins the failure policy: each
// channel gets MaxAttempts, failures are recorded, and the step advances
// anyway. A dead first channel must not strand the later steps.
func TestDeliveryFailureDoesNotHaltEscalation(t *testing.T) {
	st := newEscalationStateStore()
	st.channels = []store.Channel{mockChannel(1), mockChannel(2)}
	failure := errors.New("destination refused")
	n := &mockNotifier{errs: []error{failure, failure, nil}}
	d := newEscalationTestDispatcher(st, n)
	d.MaxAttempts = 2
	t0 := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	d.WithClock(func() time.Time { return t0 })
	policy := escalationPolicy(
		store.EscalationStep{ChannelIDs: []int64{1}},
		store.EscalationStep{DelaySeconds: 60, ChannelIDs: []int64{2}},
	)
	st.seed(t, policy, 15, -1, time.Time{})

	d.startEscalation(context.Background(), alertSnapshot(t, 15), policy)
	assert.Equal(t, 2, n.calls, "the failing channel is retried MaxAttempts times")
	require.Len(t, st.scheduled, 1, "the failing step still schedules the next one")
	assert.Len(t, st.attemptsFor(1), 2)
	for _, attempt := range st.attemptsFor(1) {
		assert.Equal(t, store.DeliveryStatusFailed, attempt.Status)
	}

	got, err := d.ProcessDueEscalations(context.Background(), t0.Add(60*time.Second))
	require.NoError(t, err)
	assert.Equal(t, 1, got)
	assert.Equal(t, 3, n.calls)
	assert.Len(t, st.attemptsFor(2), 1)
	assert.Equal(t, store.DeliveryStatusSuccess, st.attemptsFor(2)[0].Status)
}

// TestEmptyChannelStepDoesNotPanic covers a step saved with no channels (or
// whose channels have all been disabled): it is skipped silently and the
// escalation advances, matching the flat fan-out's disabled-channel handling.
func TestEmptyChannelStepDoesNotPanic(t *testing.T) {
	st := newEscalationStateStore()
	st.channels = []store.Channel{mockChannel(2)}
	n := &mockNotifier{}
	d := newEscalationTestDispatcher(st, n)
	t0 := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	d.WithClock(func() time.Time { return t0 })
	policy := escalationPolicy(
		store.EscalationStep{ChannelIDs: nil},
		store.EscalationStep{DelaySeconds: 5, ChannelIDs: []int64{2}},
	)
	st.seed(t, policy, 16, -1, time.Time{})

	require.NotPanics(t, func() {
		d.startEscalation(context.Background(), alertSnapshot(t, 16), policy)
	})
	assert.Zero(t, n.calls)
	require.Len(t, st.scheduled, 1, "an empty step still advances")

	got, err := d.ProcessDueEscalations(context.Background(), t0.Add(5*time.Second))
	require.NoError(t, err)
	assert.Equal(t, 1, got)
	assert.Equal(t, 1, n.calls)
}

// TestProcessDueEscalationsSequentialPassIsIdempotent pins the persisted-state
// guarantee: once a pass advances or completes a run, a second pass with the
// same clock does not deliver it again.
func TestProcessDueEscalationsSequentialPassIsIdempotent(t *testing.T) {
	st := newEscalationStateStore()
	st.channels = []store.Channel{mockChannel(2)}
	n := &mockNotifier{}
	d := newEscalationTestDispatcher(st, n)
	due := time.Date(2026, 10, 17, 12, 1, 0, 0, time.UTC)
	policy := escalationPolicy(
		store.EscalationStep{ChannelIDs: []int64{1}},
		store.EscalationStep{DelaySeconds: 60, ChannelIDs: []int64{2}},
		store.EscalationStep{DelaySeconds: 60, ChannelIDs: []int64{2}},
	)
	st.seed(t, policy, 17, 1, due)

	first, err := d.ProcessDueEscalations(context.Background(), due)
	require.NoError(t, err)
	second, err := d.ProcessDueEscalations(context.Background(), due)
	require.NoError(t, err)

	assert.Equal(t, 1, first)
	assert.Zero(t, second)
	assert.Equal(t, 1, n.calls)
}

// TestProcessDueEscalationsHasNoCrossProcessClaim is deliberately a
// limitation test. The engine has no row claim, so two concurrent schedulers
// that both read the same due row both deliver it. In production that cannot
// happen because only the elected leader runs RunEscalations; this test keeps
// the assumption visible and would fail if someone "fixed" it by accident
// without documenting the new contract.
func TestProcessDueEscalationsHasNoCrossProcessClaim(t *testing.T) {
	st := newEscalationStateStore()
	st.channels = []store.Channel{mockChannel(2)}
	n := &countingNotifier{}
	// Two dispatchers, not one: this models two processes, each with its own
	// breaker and rate limiter, sharing only the database.
	d1 := newEscalationTestDispatcher(st, n)
	d2 := newEscalationTestDispatcher(st, n)
	policy := escalationPolicy(
		store.EscalationStep{ChannelIDs: []int64{1}},
		store.EscalationStep{DelaySeconds: 60, ChannelIDs: []int64{2}},
	)
	due := time.Date(2026, 10, 17, 12, 1, 0, 0, time.UTC)
	st.seed(t, policy, 18, 1, due)
	st.dueGate = &dueGate{arrived: make(chan struct{}, 2), release: make(chan struct{})}

	var wg sync.WaitGroup
	wg.Add(2)
	for _, d := range []*Dispatcher{d1, d2} {
		go func(d *Dispatcher) {
			defer wg.Done()
			_, _ = d.ProcessDueEscalations(context.Background(), due)
		}(d)
	}
	<-st.dueGate.arrived
	<-st.dueGate.arrived
	close(st.dueGate.release)
	wg.Wait()

	assert.EqualValues(t, 2, n.count.Load(),
		"without leader election, two concurrent passes deliver the same step twice")
}

// countingNotifier is safe for the concurrent test; mockNotifier is not.
type countingNotifier struct{ count atomic.Int64 }

func (c *countingNotifier) Send(context.Context, Alert) error {
	c.count.Add(1)
	return nil
}

// TestRunEscalationsReturnsOnCancel pins prompt shutdown: the scheduler must
// not wait for the next tick when the process is stopping.
func TestRunEscalationsReturnsOnCancel(t *testing.T) {
	st := newEscalationStateStore()
	d := newEscalationTestDispatcher(st, &mockNotifier{})
	d.EscalationInterval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.RunEscalations(ctx)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunEscalations did not return after its context was cancelled")
	}
}

// TestEscalationLogsAndAttemptsOmitChannelConfig guards the project-wide rule
// that channel config values (webhook URLs, bot tokens, SMTP credentials)
// never reach logs or delivery records.
func TestEscalationLogsAndAttemptsOmitChannelConfig(t *testing.T) {
	const secret = "SUPER_SECRET_TOKEN_123"
	st := newEscalationStateStore()
	ch := mockChannel(1)
	ch.Config = json.RawMessage(`{"token":"` + secret + `"}`)
	st.channels = []store.Channel{ch}
	n := &mockNotifier{errs: []error{errors.New("send failed"), errors.New("send failed"), errors.New("send failed")}}
	var logs bytes.Buffer
	d := NewDispatcher(st, scriptedFactory(n), slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	d.BaseBackoff = time.Millisecond
	d.MaxAttempts = 2
	policy := escalationPolicy(
		store.EscalationStep{ChannelIDs: []int64{1}},
		store.EscalationStep{DelaySeconds: 60, ChannelIDs: []int64{1}},
	)
	st.seed(t, policy, 19, -1, time.Time{})
	d.WithClock(func() time.Time { return time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC) })

	d.startEscalation(context.Background(), alertSnapshot(t, 19), policy)

	assert.NotContains(t, logs.String(), secret)
	for _, attempt := range st.attemptsFor(1) {
		assert.NotContains(t, attempt.ResponseSnippet, secret)
	}
}

// TestEscalationFailsOpenWhenAckRecheckErrors pins the availability choice:
// an unreadable alert does not drop a page. The ack transaction remains the
// authority, and a later pass re-checks.
func TestEscalationFailsOpenWhenAckRecheckErrors(t *testing.T) {
	st := newEscalationStateStore()
	st.channels = []store.Channel{mockChannel(2)}
	st.getAlertErr = errors.New("transient read failure")
	n := &mockNotifier{}
	d := newEscalationTestDispatcher(st, n)
	policy := escalationPolicy(
		store.EscalationStep{ChannelIDs: []int64{1}},
		store.EscalationStep{DelaySeconds: 60, ChannelIDs: []int64{2}},
	)
	due := time.Date(2026, 10, 17, 12, 1, 0, 0, time.UTC)
	st.seed(t, policy, 20, 1, due)

	got, err := d.ProcessDueEscalations(context.Background(), due)
	require.NoError(t, err)
	assert.Equal(t, 1, got)
	assert.Equal(t, 1, n.calls, "a failed ack re-check must not suppress delivery")
}

func TestSecondsConversion(t *testing.T) {
	const maxSeconds = (1<<63 - 1) / int64(time.Second)
	cases := []struct {
		in   int64
		want time.Duration
	}{
		{0, 0},
		{-1, 0},
		{1, time.Second},
		{90, 90 * time.Second},
		{maxSeconds, time.Duration(maxSeconds) * time.Second},
		{math.MaxInt64, time.Duration(maxSeconds) * time.Second},
	}
	for _, tc := range cases {
		got := seconds(tc.in)
		assert.Equal(t, tc.want, got, "seconds(%d)", tc.in)
		assert.GreaterOrEqual(t, got, time.Duration(0), "seconds(%d) must not overflow negative", tc.in)
	}
}
