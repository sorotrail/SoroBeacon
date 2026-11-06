package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// Fairness queue saturation errors. Callers can test errors.Is(err, ErrFairnessQueueFull)
// to detect any queue bound overflow.
var (
	ErrFairnessQueueFull  = errors.New("delivery fairness queue full")
	ErrWorkspaceQueueFull = fmt.Errorf("%w: workspace delivery queue saturated", ErrFairnessQueueFull)
	ErrRetryQueueFull     = fmt.Errorf("%w: workspace retry queue saturated", ErrFairnessQueueFull)
	ErrChannelQueueFull   = fmt.Errorf("%w: channel delivery queue saturated", ErrFairnessQueueFull)
)

// Default fairness bounds applied when a Dispatcher is constructed or when
// zero fields are passed in FairnessConfig.
const (
	DefaultWorkspaceMaxInFlight      = 8
	DefaultWorkspaceMaxQueue         = 64
	DefaultWorkspaceMaxRetryInFlight = 2
	DefaultWorkspaceMaxRetryQueue    = 16
	DefaultChannelMaxInFlight        = 1
	DefaultChannelMaxQueue           = 32
)

// FairnessConfig tunes per-workspace and per-channel concurrency and queue
// bounds, retry lane reservations, and optional non-blocking async dispatch.
type FairnessConfig struct {
	// MaxWorkspaceInFlight bounds concurrent first-attempt deliveries for a
	// single workspace (default DefaultWorkspaceMaxInFlight).
	MaxWorkspaceInFlight int
	// MaxWorkspaceQueue bounds how many first-attempt deliveries may wait for
	// a workspace slot before excess alerts overflow to dead letters
	// (default DefaultWorkspaceMaxQueue).
	MaxWorkspaceQueue int
	// MaxWorkspaceRetryInFlight bounds concurrent retry attempts (attempt >= 2)
	// for a single workspace so retry storms cannot starve first-attempt
	// deliveries (default DefaultWorkspaceMaxRetryInFlight).
	MaxWorkspaceRetryInFlight int
	// MaxWorkspaceRetryQueue bounds queued retry attempts per workspace before
	// excess retries overflow to dead letters (default DefaultWorkspaceMaxRetryQueue).
	MaxWorkspaceRetryQueue int
	// MaxChannelInFlight bounds concurrent delivery attempts to a single
	// channel (default DefaultChannelMaxInFlight = 1, preserving head-of-line
	// ordering per channel).
	MaxChannelInFlight int
	// MaxChannelQueue bounds how many delivery attempts may wait behind an
	// active delivery on a single channel (default DefaultChannelMaxQueue).
	MaxChannelQueue int
	// AsyncDispatch, when true, makes Dispatch enqueue alerts onto the
	// workspace's bounded submission queue and return immediately so ingestion
	// and rule evaluation never block on downstream channel I/O.
	AsyncDispatch bool
}

// Normalized returns a copy of c with non-positive bounds replaced by defaults.
func (c FairnessConfig) Normalized() FairnessConfig {
	out := c
	if out.MaxWorkspaceInFlight <= 0 {
		out.MaxWorkspaceInFlight = DefaultWorkspaceMaxInFlight
	}
	if out.MaxWorkspaceQueue <= 0 {
		out.MaxWorkspaceQueue = DefaultWorkspaceMaxQueue
	}
	if out.MaxWorkspaceRetryInFlight <= 0 {
		if c.MaxWorkspaceInFlight > 0 {
			half := out.MaxWorkspaceInFlight / 2
			if half < 1 {
				half = 1
			}
			if half > DefaultWorkspaceMaxRetryInFlight {
				half = DefaultWorkspaceMaxRetryInFlight
			}
			out.MaxWorkspaceRetryInFlight = half
		} else {
			out.MaxWorkspaceRetryInFlight = DefaultWorkspaceMaxRetryInFlight
		}
	}
	if out.MaxWorkspaceRetryQueue <= 0 {
		out.MaxWorkspaceRetryQueue = DefaultWorkspaceMaxRetryQueue
	}
	if out.MaxChannelInFlight <= 0 {
		out.MaxChannelInFlight = DefaultChannelMaxInFlight
	}
	if out.MaxChannelQueue <= 0 {
		out.MaxChannelQueue = DefaultChannelMaxQueue
	}
	return out
}

// WorkspaceBacklog reports live delivery concurrency and queue depth for one workspace.
type WorkspaceBacklog struct {
	WorkspaceID     string
	InFlight        int
	QueueDepth      int
	RetryInFlight   int
	RetryQueueDepth int
	OverflowTotal   int64
}

// ChannelBacklog reports live delivery concurrency and queue depth for one channel.
type ChannelBacklog struct {
	ChannelID  int64
	InFlight   int
	QueueDepth int
}

// FairnessSnapshot captures point-in-time backlog across all tracked workspaces and channels.
type FairnessSnapshot struct {
	Workspaces map[string]WorkspaceBacklog
	Channels   map[int64]ChannelBacklog
}

type laneGate struct {
	mu         sync.Mutex
	maxActive  int
	maxQueue   int
	inFlight   int
	waiters    []chan struct{}
	extraQueue func() int
	onChange   func()
}

func newLaneGate(maxActive, maxQueue int, extraQueue func() int, onChange func()) *laneGate {
	return &laneGate{
		maxActive:  maxActive,
		maxQueue:   maxQueue,
		extraQueue: extraQueue,
		onChange:   onChange,
	}
}

func (g *laneGate) counts() (inFlight, queued int) {
	g.mu.Lock()
	inFlight = g.inFlight
	queued = len(g.waiters)
	g.mu.Unlock()
	return inFlight, queued
}

func (g *laneGate) acquire(ctx context.Context, fullErr error) (func(), error) {
	g.mu.Lock()
	if g.inFlight < g.maxActive && len(g.waiters) == 0 {
		g.inFlight++
		g.mu.Unlock()
		if g.onChange != nil {
			g.onChange()
		}
		var once sync.Once
		return func() { once.Do(g.release) }, nil
	}

	extra := 0
	if g.extraQueue != nil {
		extra = g.extraQueue()
	}
	if len(g.waiters)+extra >= g.maxQueue {
		g.mu.Unlock()
		return nil, fullErr
	}

	waitCh := make(chan struct{})
	g.waiters = append(g.waiters, waitCh)
	g.mu.Unlock()
	if g.onChange != nil {
		g.onChange()
	}

	select {
	case <-waitCh:
		if g.onChange != nil {
			g.onChange()
		}
		var once sync.Once
		return func() { once.Do(g.release) }, nil
	case <-ctx.Done():
		g.mu.Lock()
		if removeWaiter(&g.waiters, waitCh) {
			g.mu.Unlock()
			if g.onChange != nil {
				g.onChange()
			}
			return nil, ctx.Err()
		}
		g.releaseLocked()
		g.mu.Unlock()
		if g.onChange != nil {
			g.onChange()
		}
		return nil, ctx.Err()
	}
}

func (g *laneGate) release() {
	g.mu.Lock()
	g.releaseLocked()
	g.mu.Unlock()
	if g.onChange != nil {
		g.onChange()
	}
}

func (g *laneGate) releaseLocked() {
	if len(g.waiters) > 0 {
		next := g.waiters[0]
		g.waiters[0] = nil
		g.waiters = g.waiters[1:]
		close(next)
		return
	}
	if g.inFlight > 0 {
		g.inFlight--
	}
}

type channelGate struct {
	mu           sync.Mutex
	maxActive    int
	maxQueue     int
	inFlight     int
	firstWaiters []chan struct{}
	retryWaiters []chan struct{}
	onChange     func()
}

func newChannelGate(maxActive, maxQueue int, onChange func()) *channelGate {
	return &channelGate{
		maxActive: maxActive,
		maxQueue:  maxQueue,
		onChange:  onChange,
	}
}

func (g *channelGate) counts() (inFlight, queued int) {
	g.mu.Lock()
	inFlight = g.inFlight
	queued = len(g.firstWaiters) + len(g.retryWaiters)
	g.mu.Unlock()
	return inFlight, queued
}

func (g *channelGate) acquire(ctx context.Context, isRetry bool) (func(), error) {
	g.mu.Lock()
	totalWaiters := len(g.firstWaiters) + len(g.retryWaiters)
	if g.inFlight < g.maxActive && totalWaiters == 0 {
		g.inFlight++
		g.mu.Unlock()
		if g.onChange != nil {
			g.onChange()
		}
		var once sync.Once
		return func() { once.Do(g.release) }, nil
	}
	if totalWaiters >= g.maxQueue {
		g.mu.Unlock()
		return nil, ErrChannelQueueFull
	}

	waitCh := make(chan struct{})
	if isRetry {
		g.retryWaiters = append(g.retryWaiters, waitCh)
	} else {
		g.firstWaiters = append(g.firstWaiters, waitCh)
	}
	g.mu.Unlock()
	if g.onChange != nil {
		g.onChange()
	}

	select {
	case <-waitCh:
		if g.onChange != nil {
			g.onChange()
		}
		var once sync.Once
		return func() { once.Do(g.release) }, nil
	case <-ctx.Done():
		g.mu.Lock()
		var removed bool
		if isRetry {
			removed = removeWaiter(&g.retryWaiters, waitCh)
		} else {
			removed = removeWaiter(&g.firstWaiters, waitCh)
		}
		if removed {
			g.mu.Unlock()
			if g.onChange != nil {
				g.onChange()
			}
			return nil, ctx.Err()
		}
		g.releaseLocked()
		g.mu.Unlock()
		if g.onChange != nil {
			g.onChange()
		}
		return nil, ctx.Err()
	}
}

func (g *channelGate) release() {
	g.mu.Lock()
	g.releaseLocked()
	g.mu.Unlock()
	if g.onChange != nil {
		g.onChange()
	}
}

func (g *channelGate) releaseLocked() {
	if len(g.firstWaiters) > 0 {
		next := g.firstWaiters[0]
		g.firstWaiters[0] = nil
		g.firstWaiters = g.firstWaiters[1:]
		close(next)
		return
	}
	if len(g.retryWaiters) > 0 {
		next := g.retryWaiters[0]
		g.retryWaiters[0] = nil
		g.retryWaiters = g.retryWaiters[1:]
		close(next)
		return
	}
	if g.inFlight > 0 {
		g.inFlight--
	}
}

func removeWaiter(slice *[]chan struct{}, target chan struct{}) bool {
	s := *slice
	for i, ch := range s {
		if ch == target {
			copy(s[i:], s[i+1:])
			s[len(s)-1] = nil
			*slice = s[:len(s)-1]
			return true
		}
	}
	return false
}

type submittedAlert struct {
	ctx   context.Context
	alert Alert
}

type workspaceSlotPool struct {
	workspaceID      string
	firstAttemptGate *laneGate
	retryGate        *laneGate
	overflowTotal    atomic.Int64

	submitMu      sync.Mutex
	submitQueue   []submittedAlert
	activeWorkers int
	maxWorkers    int
	maxQueue      int
	onChange      func()
}

func newWorkspaceSlotPool(wsID string, cfg FairnessConfig, onChange func(*workspaceSlotPool)) *workspaceSlotPool {
	p := &workspaceSlotPool{
		workspaceID: wsID,
		maxWorkers:  cfg.MaxWorkspaceInFlight,
		maxQueue:    cfg.MaxWorkspaceQueue,
	}
	notifyChange := func() {
		if onChange != nil {
			onChange(p)
		}
	}
	p.onChange = notifyChange
	p.firstAttemptGate = newLaneGate(cfg.MaxWorkspaceInFlight, cfg.MaxWorkspaceQueue, p.submitQueueLen, notifyChange)
	p.retryGate = newLaneGate(cfg.MaxWorkspaceRetryInFlight, cfg.MaxWorkspaceRetryQueue, nil, notifyChange)
	return p
}

func (p *workspaceSlotPool) submitQueueLen() int {
	p.submitMu.Lock()
	n := len(p.submitQueue)
	p.submitMu.Unlock()
	return n
}

func (p *workspaceSlotPool) backlog() WorkspaceBacklog {
	firstInFlight, firstWaiters := p.firstAttemptGate.counts()
	retryInFlight, retryWaiters := p.retryGate.counts()
	subQueued := p.submitQueueLen()
	return WorkspaceBacklog{
		WorkspaceID:     p.workspaceID,
		InFlight:        firstInFlight,
		QueueDepth:      firstWaiters + subQueued,
		RetryInFlight:   retryInFlight,
		RetryQueueDepth: retryWaiters,
		OverflowTotal:   p.overflowTotal.Load(),
	}
}

type channelSlotPool struct {
	channelID int64
	gate      *channelGate
}

func newChannelSlotPool(channelID int64, cfg FairnessConfig, onChange func(*channelSlotPool)) *channelSlotPool {
	p := &channelSlotPool{channelID: channelID}
	p.gate = newChannelGate(cfg.MaxChannelInFlight, cfg.MaxChannelQueue, func() {
		if onChange != nil {
			onChange(p)
		}
	})
	return p
}

func (p *channelSlotPool) backlog() ChannelBacklog {
	inFlight, queued := p.gate.counts()
	return ChannelBacklog{
		ChannelID:  p.channelID,
		InFlight:   inFlight,
		QueueDepth: queued,
	}
}

// FairnessScheduler coordinates per-workspace and per-channel delivery slots,
// retry lane isolation, and non-blocking per-workspace background submission.
type FairnessScheduler struct {
	cfg        FairnessConfig
	mu         sync.RWMutex
	workspaces map[string]*workspaceSlotPool
	channels   map[int64]*channelSlotPool
	metrics    *metrics.Metrics
	wg         sync.WaitGroup
}

// NewFairnessScheduler constructs a scheduler with normalized bounds.
func NewFairnessScheduler(cfg FairnessConfig, m *metrics.Metrics) *FairnessScheduler {
	return &FairnessScheduler{
		cfg:        cfg.Normalized(),
		workspaces: make(map[string]*workspaceSlotPool),
		channels:   make(map[int64]*channelSlotPool),
		metrics:    m,
	}
}

func normalizeWorkspaceID(wsID string) string {
	ws := strings.TrimSpace(wsID)
	if ws == "" {
		return "default"
	}
	return ws
}

func resolveWorkspaceID(ctx context.Context, ch store.Channel) string {
	if ws := strings.TrimSpace(ch.WorkspaceID); ws != "" {
		return ws
	}
	if ws, ok := workspace.From(ctx); ok && !workspace.System(ctx) {
		if trimmed := strings.TrimSpace(string(ws)); trimmed != "" {
			return trimmed
		}
	}
	return "default"
}

func (s *FairnessScheduler) setMetrics(m *metrics.Metrics) {
	s.mu.Lock()
	s.metrics = m
	s.mu.Unlock()
}

func (s *FairnessScheduler) getMetrics() *metrics.Metrics {
	s.mu.RLock()
	m := s.metrics
	s.mu.RUnlock()
	return m
}

func (s *FairnessScheduler) workspacePool(wsID string) *workspaceSlotPool {
	ws := normalizeWorkspaceID(wsID)
	s.mu.RLock()
	if p, ok := s.workspaces[ws]; ok {
		s.mu.RUnlock()
		return p
	}
	s.mu.RUnlock()

	s.mu.Lock()
	if p, ok := s.workspaces[ws]; ok {
		s.mu.Unlock()
		return p
	}
	p := newWorkspaceSlotPool(ws, s.cfg, s.syncWorkspaceMetrics)
	s.workspaces[ws] = p
	s.mu.Unlock()
	s.syncWorkspaceMetrics(p)
	return p
}

func (s *FairnessScheduler) channelPool(channelID int64) *channelSlotPool {
	s.mu.RLock()
	if p, ok := s.channels[channelID]; ok {
		s.mu.RUnlock()
		return p
	}
	s.mu.RUnlock()

	s.mu.Lock()
	if p, ok := s.channels[channelID]; ok {
		s.mu.Unlock()
		return p
	}
	p := newChannelSlotPool(channelID, s.cfg, s.syncChannelMetrics)
	s.channels[channelID] = p
	s.mu.Unlock()
	s.syncChannelMetrics(p)
	return p
}

func (s *FairnessScheduler) syncWorkspaceMetrics(p *workspaceSlotPool) {
	m := s.getMetrics()
	if m == nil || p == nil {
		return
	}
	b := p.backlog()
	m.SetNotifyWorkspaceBacklog(b.WorkspaceID, "first_attempt", b.InFlight, b.QueueDepth)
	m.SetNotifyWorkspaceBacklog(b.WorkspaceID, "retry", b.RetryInFlight, b.RetryQueueDepth)
}

func (s *FairnessScheduler) syncChannelMetrics(p *channelSlotPool) {
	m := s.getMetrics()
	if m == nil || p == nil {
		return
	}
	b := p.backlog()
	m.SetNotifyChannelBacklog(b.ChannelID, b.InFlight, b.QueueDepth)
}

func (s *FairnessScheduler) recordOverflow(wsID, reason string) {
	ws := s.workspacePool(wsID)
	ws.overflowTotal.Add(1)
	if m := s.getMetrics(); m != nil {
		m.RecordNotifyFairnessOverflow(ws.workspaceID, reason)
	}
}

// AcquireSlot acquires both the workspace lane slot (first-attempt for
// attempt <= 1, retry lane for attempt >= 2) and the channel slot. The
// returned release function MUST be called as soon as the single delivery
// attempt finishes (before any retry backoff sleep).
func (s *FairnessScheduler) AcquireSlot(ctx context.Context, wsID string, channelID int64, attempt int) (release func(), overflowReason string, err error) {
	wsPool := s.workspacePool(wsID)
	chPool := s.channelPool(channelID)

	isRetry := attempt >= 2
	var releaseWS func()
	if isRetry {
		releaseWS, err = wsPool.retryGate.acquire(ctx, ErrRetryQueueFull)
		if err != nil {
			if errors.Is(err, ErrFairnessQueueFull) {
				s.recordOverflow(wsID, "retry_queue_full")
			}
			return nil, "retry_queue_full", err
		}
	} else {
		releaseWS, err = wsPool.firstAttemptGate.acquire(ctx, ErrWorkspaceQueueFull)
		if err != nil {
			if errors.Is(err, ErrFairnessQueueFull) {
				s.recordOverflow(wsID, "workspace_queue_full")
			}
			return nil, "workspace_queue_full", err
		}
	}

	releaseCh, chErr := chPool.gate.acquire(ctx, isRetry)
	if chErr != nil {
		releaseWS()
		if errors.Is(chErr, ErrFairnessQueueFull) {
			s.recordOverflow(wsID, "channel_queue_full")
		}
		return nil, "channel_queue_full", chErr
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			releaseCh()
			releaseWS()
		})
	}, "", nil
}

// Submit enqueues an alert onto the workspace's bounded async queue and spawns
// a per-workspace worker if fewer than MaxWorkspaceInFlight workers are active.
// Returns false with ErrWorkspaceQueueFull if the workspace queue is saturated.
func (s *FairnessScheduler) Submit(ctx context.Context, wsID string, a Alert, handler func(context.Context, Alert)) error {
	wsPool := s.workspacePool(wsID)
	_, gateWaiters := wsPool.firstAttemptGate.counts()

	wsPool.submitMu.Lock()
	if wsPool.activeWorkers < wsPool.maxWorkers && len(wsPool.submitQueue) == 0 {
		wsPool.activeWorkers++
		s.wg.Add(1)
		wsPool.submitMu.Unlock()
		go s.runWorkspaceWorker(wsPool, submittedAlert{ctx: ctx, alert: a}, handler)
		return nil
	}
	if len(wsPool.submitQueue)+gateWaiters >= wsPool.maxQueue {
		wsPool.submitMu.Unlock()
		s.recordOverflow(wsID, "workspace_queue_full")
		return ErrWorkspaceQueueFull
	}
	wsPool.submitQueue = append(wsPool.submitQueue, submittedAlert{ctx: ctx, alert: a})
	spawn := false
	if wsPool.activeWorkers < wsPool.maxWorkers {
		wsPool.activeWorkers++
		s.wg.Add(1)
		spawn = true
	}
	wsPool.submitMu.Unlock()
	if wsPool.onChange != nil {
		wsPool.onChange()
	}
	if spawn {
		go s.runWorkspaceWorker(wsPool, submittedAlert{}, handler)
	}
	return nil
}

func (s *FairnessScheduler) runWorkspaceWorker(wsPool *workspaceSlotPool, initial submittedAlert, handler func(context.Context, Alert)) {
	defer s.wg.Done()
	if initial.ctx != nil {
		handler(initial.ctx, initial.alert)
	}
	for {
		wsPool.submitMu.Lock()
		if len(wsPool.submitQueue) == 0 {
			wsPool.activeWorkers--
			wsPool.submitMu.Unlock()
			if wsPool.onChange != nil {
				wsPool.onChange()
			}
			return
		}
		next := wsPool.submitQueue[0]
		wsPool.submitQueue[0] = submittedAlert{}
		wsPool.submitQueue = wsPool.submitQueue[1:]
		wsPool.submitMu.Unlock()
		if wsPool.onChange != nil {
			wsPool.onChange()
		}
		handler(next.ctx, next.alert)
	}
}

// Wait blocks until all background Submit workers have drained or ctx is cancelled.
func (s *FairnessScheduler) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Snapshot returns a point-in-time copy of all workspace and channel backlogs.
func (s *FairnessScheduler) Snapshot() FairnessSnapshot {
	s.mu.RLock()
	wsPools := make([]*workspaceSlotPool, 0, len(s.workspaces))
	for _, p := range s.workspaces {
		wsPools = append(wsPools, p)
	}
	chPools := make([]*channelSlotPool, 0, len(s.channels))
	for _, p := range s.channels {
		chPools = append(chPools, p)
	}
	s.mu.RUnlock()

	snap := FairnessSnapshot{
		Workspaces: make(map[string]WorkspaceBacklog, len(wsPools)),
		Channels:   make(map[int64]ChannelBacklog, len(chPools)),
	}
	for _, p := range wsPools {
		snap.Workspaces[p.workspaceID] = p.backlog()
	}
	for _, p := range chPools {
		snap.Channels[p.channelID] = p.backlog()
	}
	return snap
}

// WorkspaceBacklog returns the current backlog for a workspace.
func (s *FairnessScheduler) WorkspaceBacklog(wsID string) WorkspaceBacklog {
	ws := normalizeWorkspaceID(wsID)
	s.mu.RLock()
	p, ok := s.workspaces[ws]
	s.mu.RUnlock()
	if !ok {
		return WorkspaceBacklog{WorkspaceID: ws}
	}
	return p.backlog()
}

// ChannelBacklog returns the current backlog for a channel.
func (s *FairnessScheduler) ChannelBacklog(channelID int64) ChannelBacklog {
	s.mu.RLock()
	p, ok := s.channels[channelID]
	s.mu.RUnlock()
	if !ok {
		return ChannelBacklog{ChannelID: channelID}
	}
	return p.backlog()
}

// StopWorkspace purges any queued asynchronous submissions for wsID and removes
// the workspace and channel slot pools so no queued work for a deleting
// workspace can start after teardown begins.
func (s *FairnessScheduler) StopWorkspace(wsID string, channelIDs []int64) int {
	if s == nil {
		return 0
	}
	ws := normalizeWorkspaceID(wsID)
	s.mu.Lock()
	wsPool := s.workspaces[ws]
	delete(s.workspaces, ws)
	for _, chID := range channelIDs {
		delete(s.channels, chID)
	}
	s.mu.Unlock()

	dropped := 0
	if wsPool != nil {
		wsPool.submitMu.Lock()
		dropped = len(wsPool.submitQueue)
		wsPool.submitQueue = nil
		wsPool.submitMu.Unlock()
	}
	if m := s.getMetrics(); m != nil {
		m.SetNotifyWorkspaceBacklog(ws, "first_attempt", 0, 0)
		m.SetNotifyWorkspaceBacklog(ws, "retry", 0, 0)
		for _, chID := range channelIDs {
			m.SetNotifyChannelBacklog(chID, 0, 0)
		}
	}
	return dropped
}
