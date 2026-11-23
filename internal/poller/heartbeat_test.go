package poller

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePosition is the race-free position snapshot the heartbeat watches. A
// test drives it directly rather than running a real poller, so the watcher
// sees exactly the states that matter: no poll yet, polls that keep
// completing (healthy), and a poller that has stopped cycling (wedged).
type fakePosition struct {
	mu  sync.Mutex
	pos Position
}

func (f *fakePosition) Position() Position {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pos
}

// advance marks a successful poll completing right now, with the cursor
// having moved — the state a healthy ingest loop produces.
func (f *fakePosition) advance() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pos.LastProcessedLedger++
	f.pos.LatestChainLedger++
	f.pos.LastSuccessfulPoll = time.Now()
	f.pos.BackingOff = false
}

// setLag opens a gap between the chain tip and the processed ledger, as a
// poller that is cycling but falling behind would produce.
func (f *fakePosition) setLag(gap int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pos.LatestChainLedger = uint32(int64(f.pos.LastProcessedLedger) + gap)
}

// startAdvancing simulates a poll loop that keeps completing cycles: it
// stamps a fresh successful poll every interval until stopped. Stopping it
// is how a test wedges the poller without touching the watcher.
func startAdvancing(pos *fakePosition, every time.Duration) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				pos.advance()
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// pingServer records the pings an external dead-man's-switch service would
// receive. Each ping is one GET, so the path identifies which endpoint was
// hit: the success URL or its failure suffix.
type pingServer struct {
	mu    sync.Mutex
	paths []string
	srv   *httptest.Server
}

func newPingServer() *pingServer {
	p := &pingServer{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.paths = append(p.paths, r.URL.Path)
		p.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	return p
}

func (p *pingServer) hits(path string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, got := range p.paths {
		if got == path {
			n++
		}
	}
	return n
}

func (p *pingServer) total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.paths)
}

// runHeartbeat starts the watcher and returns a stop function that waits for
// Run to return, so a test never leaves a goroutine running past its end.
func runHeartbeat(t *testing.T, h *Heartbeat) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Run(ctx)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("heartbeat did not stop after its context was cancelled")
		}
	}
}

func newTestHeartbeat(cfg HeartbeatConfig, pos PositionReader, pingStore func(context.Context) error) *Heartbeat {
	return NewHeartbeat(cfg, pos, pingStore, nil, slog.New(slog.DiscardHandler))
}

// A poller that keeps completing cycles pings on every check, and the
// success ping goes to the configured URL. This is the base case the failure
// tests are read against.
func TestHeartbeatPingsWhileHealthy(t *testing.T) {
	ping := newPingServer()
	defer ping.srv.Close()
	pos := &fakePosition{}
	stopAdv := startAdvancing(pos, 5*time.Millisecond)
	defer stopAdv()

	stop := runHeartbeat(t, newTestHeartbeat(HeartbeatConfig{
		URL:      ping.srv.URL + "/ok",
		Interval: 20 * time.Millisecond,
	}, pos, nil))
	defer stop()

	assert.Eventually(t, func() bool { return ping.hits("/ok") >= 3 }, time.Second, 5*time.Millisecond,
		"a healthy poller should keep pinging on each check")
	assert.Zero(t, ping.hits("/ok/fail"), "a healthy poller must not send a failure ping")
}

// The watcher must ping when it starts, not after waiting one full interval:
// on a leadership hand-over that interval would be an unexplained gap where
// the external service hears nothing either way.
func TestHeartbeatPingsImmediatelyOnStart(t *testing.T) {
	ping := newPingServer()
	defer ping.srv.Close()
	pos := &fakePosition{}
	pos.advance()

	stop := runHeartbeat(t, newTestHeartbeat(HeartbeatConfig{
		URL:      ping.srv.URL + "/ok",
		Interval: time.Hour, // far longer than the test: only the immediate check can fire
	}, pos, nil))
	defer stop()

	assert.Eventually(t, func() bool { return ping.hits("/ok") == 1 }, time.Second, 5*time.Millisecond,
		"the first ping should not wait for the first interval")
}

// The whole point of tying the ping to ingest rather than to a timer: a
// poller that stops completing cycles stops the pings, so the silence is
// what tells the external service something is wrong. This is the
// regression test for the timer-only ping the issue calls out.
func TestHeartbeatStopsPingingWhenPollerWedges(t *testing.T) {
	ping := newPingServer()
	defer ping.srv.Close()
	pos := &fakePosition{}
	stopAdv := startAdvancing(pos, 5*time.Millisecond)

	stop := runHeartbeat(t, newTestHeartbeat(HeartbeatConfig{
		URL:      ping.srv.URL + "/ok",
		Interval: 20 * time.Millisecond,
	}, pos, nil))
	defer stop()
	defer stopAdv()

	require.Eventually(t, func() bool { return ping.hits("/ok") >= 3 }, time.Second, 5*time.Millisecond,
		"pings should flow while the poller keeps cycling")

	stopAdv() // the poller wedges: no further cycle completes
	time.Sleep(30 * time.Millisecond)
	before := ping.hits("/ok")

	time.Sleep(100 * time.Millisecond) // several intervals pass
	assert.Equal(t, before, ping.hits("/ok"),
		"a wedged poller must stop pinging, not keep proving only that the process is alive")
}

// Once pings have flowed and then the poller wedges, the watcher sends one
// explicit failure ping so the external service does not have to infer the
// outage from silence alone — and only one, however long the outage runs.
func TestHeartbeatSendsFailurePingAfterWedgedPoller(t *testing.T) {
	ping := newPingServer()
	defer ping.srv.Close()
	pos := &fakePosition{}
	stopAdv := startAdvancing(pos, 5*time.Millisecond)

	stop := runHeartbeat(t, newTestHeartbeat(HeartbeatConfig{
		URL:           ping.srv.URL + "/ok",
		Interval:      20 * time.Millisecond,
		FailureSuffix: "/fail",
	}, pos, nil))
	defer stop()
	defer stopAdv()

	require.Eventually(t, func() bool { return ping.hits("/ok") >= 2 }, time.Second, 5*time.Millisecond,
		"pings should flow while the poller keeps cycling")

	stopAdv() // the poller wedges
	require.Eventually(t, func() bool { return ping.hits("/ok/fail") == 1 }, time.Second, 5*time.Millisecond,
		"a wedged poller should send exactly one failure ping")

	// It is one ping per outage, not one per check: a long outage must not
	// hammer the external service.
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 1, ping.hits("/ok/fail"), "an outage should produce one failure ping, not one per interval")
}

// A poller that recovers resumes the success pings and clears the outage
// state, so a later wedge sends a fresh failure ping.
func TestHeartbeatResumesAfterRecovery(t *testing.T) {
	ping := newPingServer()
	defer ping.srv.Close()
	pos := &fakePosition{}
	stopAdv := startAdvancing(pos, 5*time.Millisecond)

	stop := runHeartbeat(t, newTestHeartbeat(HeartbeatConfig{
		URL:           ping.srv.URL + "/ok",
		Interval:      20 * time.Millisecond,
		FailureSuffix: "/fail",
	}, pos, nil))
	defer stop()
	defer stopAdv()

	require.Eventually(t, func() bool { return ping.hits("/ok") >= 2 }, time.Second, 5*time.Millisecond)

	stopAdv() // outage
	require.Eventually(t, func() bool { return ping.hits("/ok/fail") == 1 }, time.Second, 5*time.Millisecond)

	// Recovery: the poller starts completing cycles again. The count is
	// taken after the outage so only a genuinely new success ping can
	// satisfy the condition below — an absolute count could already have
	// been reached while phase one was still healthy.
	okBeforeRecovery := ping.hits("/ok")
	resume := startAdvancing(pos, 5*time.Millisecond)
	defer resume()
	require.Eventually(t, func() bool { return ping.hits("/ok") > okBeforeRecovery }, time.Second, 5*time.Millisecond,
		"a recovered poller should resume the success pings")

	resume() // a second outage
	require.Eventually(t, func() bool { return ping.hits("/ok/fail") == 2 }, time.Second, 5*time.Millisecond,
		"a second outage should send its own failure ping")
}

// Ledger lag past the readiness threshold means the chain is not being kept
// up with, so ingest is not "flowing" even though polls keep succeeding.
// The watcher reuses READYZ_LAG_THRESHOLD for the same cut-off. Polls keep
// advancing throughout, so lag is the only thing that can stop the pings.
func TestHeartbeatFailsWhenLagExceedsThreshold(t *testing.T) {
	ping := newPingServer()
	defer ping.srv.Close()
	pos := &fakePosition{}
	stopAdv := startAdvancing(pos, 5*time.Millisecond)

	stop := runHeartbeat(t, newTestHeartbeat(HeartbeatConfig{
		URL:           ping.srv.URL + "/ok",
		Interval:      20 * time.Millisecond,
		FailureSuffix: "/fail",
		LagThreshold:  100,
	}, pos, nil))
	defer stop()
	defer stopAdv()

	require.Eventually(t, func() bool { return ping.hits("/ok") >= 2 }, time.Second, 5*time.Millisecond,
		"a poller that is keeping up should ping")

	pos.setLag(1000)
	require.Eventually(t, func() bool { return ping.hits("/ok/fail") == 1 }, time.Second, 5*time.Millisecond,
		"lag past the readiness threshold should stop the success pings and send a failure ping")
}

// Lag within the threshold is still healthy: a busy-but-keeping-up chain
// keeps pinging rather than flapping on a momentary backlog.
func TestHeartbeatWithinLagThresholdStaysHealthy(t *testing.T) {
	ping := newPingServer()
	defer ping.srv.Close()
	pos := &fakePosition{}
	stopAdv := startAdvancing(pos, 5*time.Millisecond)

	stop := runHeartbeat(t, newTestHeartbeat(HeartbeatConfig{
		URL:          ping.srv.URL + "/ok",
		Interval:     20 * time.Millisecond,
		LagThreshold: 1000,
	}, pos, nil))
	defer stop()
	defer stopAdv()

	pos.setLag(500)
	assert.Eventually(t, func() bool { return ping.hits("/ok") >= 3 }, time.Second, 5*time.Millisecond,
		"lag within the threshold should stay healthy")
	assert.Zero(t, ping.hits("/ok/fail"), "lag within the threshold must not send a failure ping")
}

// With no threshold configured the lag check is disabled, exactly as it is
// for /readyz, so an existing deployment is not silently held to a cut-off
// it never opted into.
func TestHeartbeatLagCheckDisabledWithoutThreshold(t *testing.T) {
	ping := newPingServer()
	defer ping.srv.Close()
	pos := &fakePosition{}
	stopAdv := startAdvancing(pos, 5*time.Millisecond)

	stop := runHeartbeat(t, newTestHeartbeat(HeartbeatConfig{
		URL:      ping.srv.URL + "/ok",
		Interval: 20 * time.Millisecond,
	}, pos, nil))
	defer stop()
	defer stopAdv()

	pos.setLag(100000)
	assert.Eventually(t, func() bool { return ping.hits("/ok") >= 3 }, time.Second, 5*time.Millisecond,
		"with no threshold the lag check is off, so a lagging poller that is still cycling stays healthy")
	assert.Zero(t, ping.hits("/ok/fail"))
}

// An unreachable store means alerts cannot be written, so ingest is not
// healthy even while polls keep succeeding. Polls advance throughout, so
// the store is the only thing that can stop the pings — the same question
// /readyz asks of the database.
func TestHeartbeatFailsWhenStoreUnreachable(t *testing.T) {
	ping := newPingServer()
	defer ping.srv.Close()
	pos := &fakePosition{}
	stopAdv := startAdvancing(pos, 5*time.Millisecond)

	var mu sync.Mutex
	storeOK := true
	pingStore := func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if !storeOK {
			return context.DeadlineExceeded
		}
		return nil
	}

	stop := runHeartbeat(t, newTestHeartbeat(HeartbeatConfig{
		URL:           ping.srv.URL + "/ok",
		Interval:      20 * time.Millisecond,
		FailureSuffix: "/fail",
	}, pos, pingStore))
	defer stop()
	defer stopAdv()

	require.Eventually(t, func() bool { return ping.hits("/ok") >= 2 }, time.Second, 5*time.Millisecond,
		"a reachable store should let the poller ping")

	mu.Lock()
	storeOK = false
	mu.Unlock()

	require.Eventually(t, func() bool { return ping.hits("/ok/fail") == 1 }, time.Second, 5*time.Millisecond,
		"an unreachable store should stop the success pings and send a failure ping")
}

// With no failure suffix configured the watcher sends success pings only:
// the external service notices the silence on its own timeout rather than
// being told explicitly.
func TestHeartbeatNoFailurePingWithoutSuffix(t *testing.T) {
	ping := newPingServer()
	defer ping.srv.Close()
	pos := &fakePosition{}
	stopAdv := startAdvancing(pos, 5*time.Millisecond)

	stop := runHeartbeat(t, newTestHeartbeat(HeartbeatConfig{
		URL:      ping.srv.URL + "/ok",
		Interval: 20 * time.Millisecond,
	}, pos, nil))
	defer stop()
	defer stopAdv()

	require.Eventually(t, func() bool { return ping.hits("/ok") >= 2 }, time.Second, 5*time.Millisecond)
	stopAdv()

	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, ping.hits("/ok"), ping.total(),
		"with no failure suffix only the success endpoint should ever be hit")
}

// Before the first success there is nothing to fail: a watcher that starts
// on a not-yet-ready poller must not announce a failure, because the
// external service already treats the absence of pings as "no data yet".
func TestHeartbeatNoFailurePingBeforeFirstSuccess(t *testing.T) {
	ping := newPingServer()
	defer ping.srv.Close()
	pos := &fakePosition{} // no successful poll yet

	stop := runHeartbeat(t, newTestHeartbeat(HeartbeatConfig{
		URL:           ping.srv.URL + "/ok",
		Interval:      20 * time.Millisecond,
		FailureSuffix: "/fail",
	}, pos, nil))
	defer stop()

	time.Sleep(100 * time.Millisecond)
	assert.Zero(t, ping.total(), "a watcher that has never succeeded should stay silent, not announce a failure")
}

// An empty URL disables the watcher entirely, so a deployment that never
// configured a heartbeat falls back to no heartbeat rather than pinging
// nothing or erroring.
func TestHeartbeatDisabledWithEmptyURL(t *testing.T) {
	ping := newPingServer()
	defer ping.srv.Close()
	pos := &fakePosition{}
	stopAdv := startAdvancing(pos, 5*time.Millisecond)
	defer stopAdv()

	stop := runHeartbeat(t, newTestHeartbeat(HeartbeatConfig{URL: "", Interval: 20 * time.Millisecond}, pos, nil))
	defer stop()

	time.Sleep(100 * time.Millisecond)
	assert.Zero(t, ping.total(), "an empty heartbeat URL must send nothing")
}

// The URL is a credential: its path identifies the monitor to the external
// service, so it must never reach a log line. A transport error is the
// likeliest way it would leak, because net/url errors embed the target.
func TestHeartbeatNeverLogsTheURL(t *testing.T) {
	pos := &fakePosition{}
	pos.advance()

	var buf heartbeatLogBuffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	h := NewHeartbeat(HeartbeatConfig{
		URL:           "http://127.0.0.1:1/deadbeef-secret-token/ok",
		Interval:      time.Hour,
		FailureSuffix: "/fail",
	}, pos, nil, nil, log)

	// Drive a few checks and pings directly so the transport-error path
	// runs: port 1 refuses every connection.
	ctx := context.Background()
	for range 3 {
		h.check(ctx, time.Now().Add(-time.Hour))
		h.ping(ctx, h.url, true)
	}

	assert.NotContains(t, buf.String(), "deadbeef-secret-token",
		"the heartbeat URL is a credential and must never be logged")
	assert.NotContains(t, buf.String(), h.url, "the full heartbeat URL must never be logged")
	assert.Contains(t, buf.String(), "err_class", "the failure should still be logged, just without the URL")
}

// classifyPingErr turns a transport error into a category whose text can
// never contain the URL, while still saying something useful about what
// went wrong.
func TestClassifyPingErr(t *testing.T) {
	assert.Equal(t, "none", classifyPingErr(nil))
	assert.Equal(t, "timeout", classifyPingErr(context.DeadlineExceeded))
	assert.Equal(t, "connection_refused", classifyPingErr(&fakeNetErr{msg: "dial tcp: connection refused"}))
	assert.Equal(t, "dns_error", classifyPingErr(&fakeNetErr{msg: "lookup host: no such host"}))
	assert.Equal(t, "tls_error", classifyPingErr(&fakeNetErr{msg: "tls: certificate verify failed"}))
	assert.Equal(t, "transport_error", classifyPingErr(&fakeNetErr{msg: "something else"}))
}

type fakeNetErr struct{ msg string }

func (e *fakeNetErr) Error() string { return e.msg }

// heartbeatLogBuffer is a strings.Builder that is safe to write to from the
// watcher's goroutine while the test reads it.
type heartbeatLogBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *heartbeatLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *heartbeatLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
