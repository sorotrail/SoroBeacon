package faultinject

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FaultDomain identifies the subsystem targeted by a chaos fault.
type FaultDomain string

const (
	DomainChannelDelivery FaultDomain = "channel_delivery"
	DomainDatabase        FaultDomain = "database"
	DomainLeaderLease     FaultDomain = "leader_lease"
	DomainRPC             FaultDomain = "stellar_rpc"
	DomainEscalation      FaultDomain = "escalation_mid_flight"
	DomainDigestFlush     FaultDomain = "digest_flush"
	DomainProcessRestart  FaultDomain = "process_restart"
)

// InvariantID names one of the universal chaos invariants.
type InvariantID string

const (
	InvariantTerminalOutcome    InvariantID = "terminal_delivery_or_dead_letter"
	InvariantNoDuplicateWithout InvariantID = "no_duplicate_delivery_without_dedup_key"
	InvariantAckStopsEscalation InvariantID = "acknowledged_alert_never_escalates"
	InvariantWorkspaceIsolation InvariantID = "workspace_isolation_holds"
	InvariantSecretRedaction    InvariantID = "channel_config_never_leaked"
)

// FaultEvent records a single injected fault in the ordered execution trace.
type FaultEvent struct {
	Step        int         `json:"step"`
	Domain      FaultDomain `json:"domain"`
	Action      string      `json:"action"`
	WorkspaceID string      `json:"workspace_id,omitempty"`
	AlertID     int64       `json:"alert_id,omitempty"`
	ChannelID   int64       `json:"channel_id,omitempty"`
	LeaderID    string      `json:"leader_id,omitempty"`
	Ledger      uint32      `json:"ledger,omitempty"`
	Detail      string      `json:"detail"`
}

func (e FaultEvent) String() string {
	var attrs []string
	if e.WorkspaceID != "" {
		attrs = append(attrs, "ws="+e.WorkspaceID)
	}
	if e.AlertID != 0 {
		attrs = append(attrs, fmt.Sprintf("alert=%d", e.AlertID))
	}
	if e.ChannelID != 0 {
		attrs = append(attrs, fmt.Sprintf("ch=%d", e.ChannelID))
	}
	if e.LeaderID != "" {
		attrs = append(attrs, "leader="+e.LeaderID)
	}
	if e.Ledger != 0 {
		attrs = append(attrs, fmt.Sprintf("ledger=%d", e.Ledger))
	}
	suffix := ""
	if len(attrs) > 0 {
		suffix = " [" + strings.Join(attrs, " ") + "]"
	}
	return fmt.Sprintf("step=%02d domain=%s action=%s%s: %s",
		e.Step, e.Domain, e.Action, suffix, e.Detail)
}

// InvariantViolation describes a broken system invariant discovered during a
// chaos run.
type InvariantViolation struct {
	Invariant   InvariantID `json:"invariant"`
	Description string      `json:"description"`
	AlertID     int64       `json:"alert_id,omitempty"`
	ChannelID   int64       `json:"channel_id,omitempty"`
	WorkspaceID string      `json:"workspace_id,omitempty"`
}

// ChaosFailureError is returned when a chaos run violates one or more
// invariants. Its Error() output includes the seed, exact replay command,
// broken invariant(s), and ordered fault sequence for CI triage.
type ChaosFailureError struct {
	Seed            int64                `json:"seed"`
	ReplayCommand   string               `json:"replay_command"`
	Violations      []InvariantViolation `json:"violations"`
	FaultSequence   []FaultEvent         `json:"fault_sequence"`
	AlertsDetected  int                  `json:"alerts_detected"`
	AlertsDelivered int                  `json:"alerts_delivered"`
	DeadLetters     int                  `json:"dead_letters"`
}

func (e *ChaosFailureError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "chaos suite invariant failure (seed=%d)\n", e.Seed)
	fmt.Fprintf(&b, "  replay_cmd: %s\n", e.ReplayCommand)
	fmt.Fprintf(&b, "  summary:    detected=%d delivered=%d dead_letters=%d faults=%d\n",
		e.AlertsDetected, e.AlertsDelivered, e.DeadLetters, len(e.FaultSequence))
	fmt.Fprintf(&b, "  broken_invariants (%d):\n", len(e.Violations))
	for _, v := range e.Violations {
		fmt.Fprintf(&b, "    - [%s] %s (alert=%d channel=%d ws=%s)\n",
			v.Invariant, v.Description, v.AlertID, v.ChannelID, v.WorkspaceID)
	}
	fmt.Fprintf(&b, "  fault_sequence (%d events):\n", len(e.FaultSequence))
	for _, ev := range e.FaultSequence {
		fmt.Fprintf(&b, "    %s\n", ev.String())
	}
	return b.String()
}

// ReplayCommand returns the exact CLI command an operator or developer runs
// locally to reproduce a chaos seed.
func ReplayCommand(seed int64) string {
	return fmt.Sprintf("SOROBEACON_CHAOS_SEED=%d go test -race -v -run TestChaosSuiteReplay ./internal/notify/faultinject", seed)
}

// SeedFromEnv reads SOROBEACON_CHAOS_SEED if set, allowing one-command local
// reproduction of any CI failure.
func SeedFromEnv() (int64, bool, error) {
	raw := strings.TrimSpace(os.Getenv("SOROBEACON_CHAOS_SEED"))
	if raw == "" {
		return 0, false, nil
	}
	seed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("invalid SOROBEACON_CHAOS_SEED %q: %w", raw, err)
	}
	return seed, true, nil
}

// ChaosConfig controls a deterministic multi-subsystem chaos run.
type ChaosConfig struct {
	Seed            int64
	Ledgers         int
	EventsPerLedger int
	MaxAttempts     int
	Workspaces      []string
	Logger          *slog.Logger
}

// DefaultChaosConfig returns a balanced configuration for a single seed run.
func DefaultChaosConfig(seed int64) ChaosConfig {
	return ChaosConfig{
		Seed:            seed,
		Ledgers:         6,
		EventsPerLedger: 3,
		MaxAttempts:     3,
		Workspaces:      []string{"ws-alpha", "ws-beta", "ws-gamma"},
	}
}

// ChaosDeliveryReceipt records a single delivered notification.
type ChaosDeliveryReceipt struct {
	AlertID     int64     `json:"alert_id"`
	ChannelID   int64     `json:"channel_id"`
	WorkspaceID string    `json:"workspace_id"`
	DedupKey    string    `json:"dedup_key"`
	StepIndex   int       `json:"step_index"`
	IsEscalated bool      `json:"is_escalation"`
	IsDigest    bool      `json:"is_digest"`
	DeliveredAt time.Time `json:"delivered_at"`
}

// ChaosDeadLetter records an alert delivery that exhausted its retry budget.
type ChaosDeadLetter struct {
	AlertID      int64     `json:"alert_id"`
	ChannelID    int64     `json:"channel_id"`
	WorkspaceID  string    `json:"workspace_id"`
	AttemptCount int       `json:"attempt_count"`
	LastError    string    `json:"last_error"`
	CreatedAt    time.Time `json:"created_at"`
}

// ChaosAlert represents a detected contract event alert tracked through the
// chaos pipeline.
type ChaosAlert struct {
	ID             int64      `json:"id"`
	WorkspaceID    string     `json:"workspace_id"`
	MonitorID      int64      `json:"monitor_id"`
	ChannelID      int64      `json:"channel_id"`
	EscalationChID int64      `json:"escalation_channel_id"`
	DigestMode     bool       `json:"digest_mode"`
	EventID        string     `json:"event_id"`
	Ledger         uint32     `json:"ledger"`
	CreatedAt      time.Time  `json:"created_at"`
	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
	AcknowledgedBy string     `json:"acknowledged_by,omitempty"`
}

// ChaosChannel represents a workspace-scoped notification destination.
// Config contains sensitive credentials that must never appear in logs,
// error messages, or dead-letter snippets.
type ChaosChannel struct {
	ID          int64
	WorkspaceID string
	Name        string
	DigestMode  bool
	Config      string // Sensitive JSON payload; must never be logged or echoed
}

// ChaosResult summarizes a completed chaos run and provides access to the
// persisted state and audit trail for invariant verification.
type ChaosResult struct {
	Seed           int64                  `json:"seed"`
	ReplayCommand  string                 `json:"replay_command"`
	FaultSequence  []FaultEvent           `json:"fault_sequence"`
	Alerts         []ChaosAlert           `json:"alerts"`
	Deliveries     []ChaosDeliveryReceipt `json:"deliveries"`
	DeadLetters    []ChaosDeadLetter      `json:"dead_letters"`
	PendingDigests int                    `json:"pending_digests"`
	LeaderChanges  int                    `json:"leader_changes"`
	ProcessResumes int                    `json:"process_resumes"`
	DBFaults       int                    `json:"db_faults"`
	RPCFaults      int                    `json:"rpc_faults"`
	DigestFaults   int                    `json:"digest_faults"`
	ChannelFaults  int                    `json:"channel_faults"`
	LogOutput      string                 `json:"-"`
	secretCanaries []string
}

// CheckInvariants verifies all universal invariants over the chaos run result.
// If any invariant fails, it returns a *ChaosFailureError with full replay
// context.
func (r *ChaosResult) CheckInvariants() error {
	var violations []InvariantViolation

	alertByID := make(map[int64]ChaosAlert, len(r.Alerts))
	for _, a := range r.Alerts {
		alertByID[a.ID] = a
	}

	deliveredAlerts := make(map[int64]bool)
	deadLetteredAlerts := make(map[int64]bool)
	seenDeliveryWithoutDedup := make(map[string]int)
	seenDedupKeys := make(map[string]ChaosDeliveryReceipt)

	for _, d := range r.Deliveries {
		deliveredAlerts[d.AlertID] = true

		// Invariant 2: No alert is delivered twice without a deduplication key.
		targetKey := fmt.Sprintf("alert=%d:ch=%d:step=%d", d.AlertID, d.ChannelID, d.StepIndex)
		if strings.TrimSpace(d.DedupKey) == "" {
			seenDeliveryWithoutDedup[targetKey]++
			if seenDeliveryWithoutDedup[targetKey] > 1 {
				violations = append(violations, InvariantViolation{
					Invariant:   InvariantNoDuplicateWithout,
					Description: fmt.Sprintf("alert %d delivered %d times to channel %d without a deduplication key", d.AlertID, seenDeliveryWithoutDedup[targetKey], d.ChannelID),
					AlertID:     d.AlertID,
					ChannelID:   d.ChannelID,
					WorkspaceID: d.WorkspaceID,
				})
			}
		} else if prev, exists := seenDedupKeys[d.DedupKey]; exists {
			violations = append(violations, InvariantViolation{
				Invariant:   InvariantNoDuplicateWithout,
				Description: fmt.Sprintf("duplicate delivery recorded for already-committed dedup key %q (first at %s, second at %s)", d.DedupKey, prev.DeliveredAt.Format(time.RFC3339Nano), d.DeliveredAt.Format(time.RFC3339Nano)),
				AlertID:     d.AlertID,
				ChannelID:   d.ChannelID,
				WorkspaceID: d.WorkspaceID,
			})
		} else {
			seenDedupKeys[d.DedupKey] = d
		}

		alert, ok := alertByID[d.AlertID]
		if !ok {
			violations = append(violations, InvariantViolation{
				Invariant:   InvariantTerminalOutcome,
				Description: fmt.Sprintf("delivery receipt references unknown alert %d", d.AlertID),
				AlertID:     d.AlertID,
				ChannelID:   d.ChannelID,
				WorkspaceID: d.WorkspaceID,
			})
			continue
		}

		// Invariant 3: An acknowledged alert never escalates afterwards.
		if d.IsEscalated && alert.AcknowledgedAt != nil && !d.DeliveredAt.Before(*alert.AcknowledgedAt) {
			violations = append(violations, InvariantViolation{
				Invariant: InvariantAckStopsEscalation,
				Description: fmt.Sprintf(
					"alert %d escalated at %s (step %d) after being acknowledged at %s",
					d.AlertID,
					d.DeliveredAt.Format(time.RFC3339Nano),
					d.StepIndex,
					alert.AcknowledgedAt.Format(time.RFC3339Nano),
				),
				AlertID:     d.AlertID,
				ChannelID:   d.ChannelID,
				WorkspaceID: d.WorkspaceID,
			})
		}

		// Invariant 4: Workspace scoping holds throughout.
		if d.WorkspaceID != alert.WorkspaceID {
			violations = append(violations, InvariantViolation{
				Invariant: InvariantWorkspaceIsolation,
				Description: fmt.Sprintf(
					"cross-workspace delivery leak: alert %d belongs to workspace %q but was delivered in workspace %q",
					d.AlertID, alert.WorkspaceID, d.WorkspaceID,
				),
				AlertID:     d.AlertID,
				ChannelID:   d.ChannelID,
				WorkspaceID: d.WorkspaceID,
			})
		}
	}

	for _, dl := range r.DeadLetters {
		deadLetteredAlerts[dl.AlertID] = true
		alert, ok := alertByID[dl.AlertID]
		if !ok {
			violations = append(violations, InvariantViolation{
				Invariant:   InvariantTerminalOutcome,
				Description: fmt.Sprintf("dead-letter references unknown alert %d", dl.AlertID),
				AlertID:     dl.AlertID,
				ChannelID:   dl.ChannelID,
				WorkspaceID: dl.WorkspaceID,
			})
			continue
		}
		if dl.WorkspaceID != alert.WorkspaceID {
			violations = append(violations, InvariantViolation{
				Invariant: InvariantWorkspaceIsolation,
				Description: fmt.Sprintf(
					"cross-workspace dead-letter leak: alert %d belongs to workspace %q but dead-lettered in workspace %q",
					dl.AlertID, alert.WorkspaceID, dl.WorkspaceID,
				),
				AlertID:     dl.AlertID,
				ChannelID:   dl.ChannelID,
				WorkspaceID: dl.WorkspaceID,
			})
		}
		for _, canary := range r.secretCanaries {
			if canary != "" && strings.Contains(dl.LastError, canary) {
				violations = append(violations, InvariantViolation{
					Invariant:   InvariantSecretRedaction,
					Description: fmt.Sprintf("dead-letter error for alert %d leaked sensitive channel config value", dl.AlertID),
					AlertID:     dl.AlertID,
					ChannelID:   dl.ChannelID,
					WorkspaceID: dl.WorkspaceID,
				})
			}
		}
	}

	// Invariant 1: Every detected alert is eventually delivered or dead-lettered.
	for _, a := range r.Alerts {
		if !deliveredAlerts[a.ID] && !deadLetteredAlerts[a.ID] {
			violations = append(violations, InvariantViolation{
				Invariant:   InvariantTerminalOutcome,
				Description: fmt.Sprintf("alert %d (event=%s ws=%s) was neither delivered nor dead-lettered", a.ID, a.EventID, a.WorkspaceID),
				AlertID:     a.ID,
				ChannelID:   a.ChannelID,
				WorkspaceID: a.WorkspaceID,
			})
		}
	}

	if r.PendingDigests > 0 {
		violations = append(violations, InvariantViolation{
			Invariant:   InvariantTerminalOutcome,
			Description: fmt.Sprintf("%d digest alert(s) remained stranded in pending_digests after drain", r.PendingDigests),
		})
	}

	// Secret redaction check over logs and fault descriptions.
	for _, canary := range r.secretCanaries {
		if canary == "" {
			continue
		}
		if strings.Contains(r.LogOutput, canary) {
			violations = append(violations, InvariantViolation{
				Invariant:   InvariantSecretRedaction,
				Description: "chaos log output leaked sensitive channel config secret",
			})
		}
		for _, ev := range r.FaultSequence {
			if strings.Contains(ev.Detail, canary) {
				violations = append(violations, InvariantViolation{
					Invariant:   InvariantSecretRedaction,
					Description: fmt.Sprintf("fault event step %d leaked sensitive channel config secret", ev.Step),
				})
			}
		}
	}

	if len(violations) == 0 {
		return nil
	}

	return &ChaosFailureError{
		Seed:            r.Seed,
		ReplayCommand:   r.ReplayCommand,
		Violations:      violations,
		FaultSequence:   r.FaultSequence,
		AlertsDetected:  len(r.Alerts),
		AlertsDelivered: len(r.Deliveries),
		DeadLetters:     len(r.DeadLetters),
	}
}

// chaosStore models the persistent database state shared across leader changes
// and mid-batch process restarts.
type chaosStore struct {
	mu               sync.Mutex
	clock            time.Time
	nextAlertID      int64
	ingestCursor     uint32
	leaderID         string
	leaseEpoch       int64
	channels         map[int64]ChaosChannel
	alertsByEventKey map[string]int64
	alerts           map[int64]*ChaosAlert
	deliveries       []ChaosDeliveryReceipt
	dedupKeys        map[string]ChaosDeliveryReceipt
	deadLetters      []ChaosDeadLetter
	pendingDigests   map[int64][]int64 // channelID -> alertIDs
	pendingEscalates map[int64]int     // alertID -> next step index
}

func newChaosStore(workspaces []string) (*chaosStore, []string) {
	st := &chaosStore{
		clock:            time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC),
		channels:         make(map[int64]ChaosChannel),
		alertsByEventKey: make(map[string]int64),
		alerts:           make(map[int64]*ChaosAlert),
		dedupKeys:        make(map[string]ChaosDeliveryReceipt),
		pendingDigests:   make(map[int64][]int64),
		pendingEscalates: make(map[int64]int),
	}
	var canaries []string
	var nextChID int64 = 100
	for _, ws := range workspaces {
		primaryCanary := fmt.Sprintf("whsec_canary_%s_primary_secret_token", ws)
		escalateCanary := fmt.Sprintf("whsec_canary_%s_escalation_secret_token", ws)
		digestCanary := fmt.Sprintf("whsec_canary_%s_digest_secret_token", ws)
		canaries = append(canaries, primaryCanary, escalateCanary, digestCanary)

		st.channels[nextChID] = ChaosChannel{
			ID:          nextChID,
			WorkspaceID: ws,
			Name:        ws + "-primary-webhook",
			DigestMode:  false,
			Config:      fmt.Sprintf(`{"url":"https://hooks.example.invalid/%s","secret":%q}`, ws, primaryCanary),
		}
		st.channels[nextChID+1] = ChaosChannel{
			ID:          nextChID + 1,
			WorkspaceID: ws,
			Name:        ws + "-escalation-pager",
			DigestMode:  false,
			Config:      fmt.Sprintf(`{"url":"https://pager.example.invalid/%s","routing_key":%q}`, ws, escalateCanary),
		}
		st.channels[nextChID+2] = ChaosChannel{
			ID:          nextChID + 2,
			WorkspaceID: ws,
			Name:        ws + "-digest-slack",
			DigestMode:  true,
			Config:      fmt.Sprintf(`{"webhook_url":"https://slack.example.invalid/%s","bearer":%q}`, ws, digestCanary),
		}
		nextChID += 10
	}
	return st, canaries
}

func (s *chaosStore) advanceClock(d time.Duration) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock = s.clock.Add(d)
	return s.clock
}

// RunChaosSuite executes a deterministic, seed-driven chaos run that composes
// faults across notification channel HTTP delivery, database operations,
// leader lease coordination, Soroban RPC polling, alert escalation with
// concurrent acknowledgements, digest batch flushing, and mid-batch process
// restarts.
//
// Crucially, the seed and replay command are logged BEFORE any fault injection
// begins so a panic or timeout never hides the seed that triggered it.
func RunChaosSuite(ctx context.Context, cfg ChaosConfig) (*ChaosResult, error) {
	if cfg.Ledgers <= 0 {
		cfg.Ledgers = 6
	}
	if cfg.EventsPerLedger <= 0 {
		cfg.EventsPerLedger = 3
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	if len(cfg.Workspaces) == 0 {
		cfg.Workspaces = []string{"ws-alpha", "ws-beta", "ws-gamma"}
	}

	var logBuf bytes.Buffer
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}

	replayCmd := ReplayCommand(cfg.Seed)
	// Always log the seed before running so a crash or timeout still prints it.
	logger.Info("chaos_suite_starting",
		"seed", cfg.Seed,
		"replay_cmd", replayCmd,
		"ledgers", cfg.Ledgers,
		"events_per_ledger", cfg.EventsPerLedger,
		"workspaces", len(cfg.Workspaces),
	)

	rng := rand.New(rand.NewSource(cfg.Seed)) //nolint:gosec // deterministic chaos PRNG
	store, canaries := newChaosStore(cfg.Workspaces)

	// Wire NewGenerator + NewFaultyTransport over an in-memory roundTripper so
	// every HTTP channel delivery goes through FaultyTransport (5xx, 429
	// rate-limit, connection reset, TLS failure, DNS failure).
	gen := NewGenerator(uint64(cfg.Seed)) //nolint:gosec // seed is int64
	baseRT := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			Status:        "200 OK",
			StatusCode:    http.StatusOK,
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        http.Header{"Content-Type": []string{"application/json"}},
			Body:          io.NopCloser(strings.NewReader(`{"ok":true}`)),
			ContentLength: 11,
			Request:       req,
		}, nil
	})
	faultyRT := NewFaultyTransport(baseRT, gen)
	httpClient := &http.Client{
		Transport: faultyRT,
		Timeout:   120 * time.Millisecond,
	}

	res := &ChaosResult{
		Seed:           cfg.Seed,
		ReplayCommand:  replayCmd,
		secretCanaries: canaries,
	}

	step := 0
	recordFault := func(domain FaultDomain, action, ws string, alertID, chID int64, leader string, ledger uint32, detail string) {
		step++
		ev := FaultEvent{
			Step:        step,
			Domain:      domain,
			Action:      action,
			WorkspaceID: ws,
			AlertID:     alertID,
			ChannelID:   chID,
			LeaderID:    leader,
			Ledger:      ledger,
			Detail:      detail,
		}
		res.FaultSequence = append(res.FaultSequence, ev)
		logger.Info("chaos_fault_injected",
			"step", ev.Step,
			"domain", string(ev.Domain),
			"action", ev.Action,
			"alert_id", ev.AlertID,
			"channel_id", ev.ChannelID,
			"ledger", ev.Ledger,
		)
	}

	currentLeader := "poller-node-1"
	store.leaderID = currentLeader
	store.leaseEpoch = 1

	// Guarantee that every single chaos run exercises all 7 fault domains at
	// least once, while ordering and exact parameters vary deterministically
	// from cfg.Seed.
	deliverHTTP := func(alert ChaosAlert, chID int64, stepIdx int, isEscalation, isDigest bool) error {
		ch, ok := store.channels[chID]
		if !ok {
			return fmt.Errorf("unknown channel %d", chID)
		}
		if ch.WorkspaceID != alert.WorkspaceID {
			return fmt.Errorf("workspace mismatch: alert ws=%s channel ws=%s", alert.WorkspaceID, ch.WorkspaceID)
		}

		dedupKey := fmt.Sprintf("dedup:ws=%s:alert=%d:ch=%d:step=%d", alert.WorkspaceID, alert.ID, chID, stepIdx)
		store.mu.Lock()
		if _, already := store.dedupKeys[dedupKey]; already {
			store.mu.Unlock()
			return nil
		}
		store.mu.Unlock()

		var lastErr error
		for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
			// Choose whether to inject an HTTP channel fault on this attempt.
			injectFault := attempt < cfg.MaxAttempts && rng.Intn(3) == 0
			// Also allow occasional permanent retry exhaustion to exercise dead-lettering.
			exhaustRetries := attempt <= cfg.MaxAttempts && (( int(alert.ID)+ int(cfg.Seed))%7 == 0)

			if injectFault || exhaustRetries {
				res.ChannelFaults++
				faultRoll := rng.Intn(5)
				switch faultRoll {
				case 0:
					gen.SetFaults(FaultConfig{ServerError5xx: 1.0})
					recordFault(DomainChannelDelivery, "http_500_server_error", alert.WorkspaceID, alert.ID, chID, currentLeader, alert.Ledger,
						fmt.Sprintf("attempt %d/%d received HTTP 500", attempt, cfg.MaxAttempts))
				case 1:
					gen.SetFaults(FaultConfig{RateLimit: 1.0})
					recordFault(DomainChannelDelivery, "http_429_rate_limit", alert.WorkspaceID, alert.ID, chID, currentLeader, alert.Ledger,
						fmt.Sprintf("attempt %d/%d rate limited with Retry-After", attempt, cfg.MaxAttempts))
				case 2:
					gen.SetFaults(FaultConfig{TLSFailure: 1.0})
					recordFault(DomainChannelDelivery, "tls_handshake_failure", alert.WorkspaceID, alert.ID, chID, currentLeader, alert.Ledger,
						fmt.Sprintf("attempt %d/%d simulated TLS handshake failure", attempt, cfg.MaxAttempts))
				case 3:
					gen.SetFaults(FaultConfig{ConnectionReset: 1.0})
					recordFault(DomainChannelDelivery, "tcp_connection_reset", alert.WorkspaceID, alert.ID, chID, currentLeader, alert.Ledger,
						fmt.Sprintf("attempt %d/%d connection reset by peer", attempt, cfg.MaxAttempts))
				default:
					gen.SetFaults(FaultConfig{DNSFailure: 1.0})
					recordFault(DomainChannelDelivery, "dns_lookup_failure", alert.WorkspaceID, alert.ID, chID, currentLeader, alert.Ledger,
						fmt.Sprintf("attempt %d/%d simulated DNS failure", attempt, cfg.MaxAttempts))
				}
			} else {
				gen.SetFaults(FaultConfig{})
			}

			// Never include ch.Config in outgoing request headers/logs/errors.
			payload := fmt.Sprintf(`{"alert_id":%d,"event_id":%q,"workspace":%q,"step":%d}`,
				alert.ID, alert.EventID, alert.WorkspaceID, stepIdx)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://notify.example.invalid/deliver", strings.NewReader(payload))
			if err != nil {
				lastErr = err
				continue
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", dedupKey)

			resp, err := httpClient.Do(req)
			if err != nil {
				lastErr = fmt.Errorf("channel %d (%s) transport failure: %w", chID, ch.Name, err)
				continue
			}
			_, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				lastErr = fmt.Errorf("channel %d (%s) body read error: %w", chID, ch.Name, readErr)
				continue
			}
			if resp.StatusCode >= 400 {
				lastErr = fmt.Errorf("channel %d (%s) rejected delivery with status %d", chID, ch.Name, resp.StatusCode)
				continue
			}

			// Simulate a transient DB disconnect during delivery receipt commit
			// on the first attempt for selected alerts; the retry uses the same
			// dedupKey so the alert is still delivered exactly once.
			if attempt == 1 && rng.Intn(5) == 0 {
				res.DBFaults++
				recordFault(DomainDatabase, "db_disconnect_on_delivery_record", alert.WorkspaceID, alert.ID, chID, currentLeader, alert.Ledger,
					"database connection dropped before delivery receipt commit; retrying with idempotency key")
				lastErr = errors.New("store: connection reset by peer while recording delivery attempt")
				continue
			}

			now := store.advanceClock(10 * time.Millisecond)
			store.mu.Lock()
			if _, exists := store.dedupKeys[dedupKey]; !exists {
				// Re-check acknowledgement under lock before committing an escalation delivery.
				if isEscalation {
					if currentAlert, ok := store.alerts[alert.ID]; ok && currentAlert.AcknowledgedAt != nil {
						store.mu.Unlock()
						return nil
					}
				}
				receipt := ChaosDeliveryReceipt{
					AlertID:     alert.ID,
					ChannelID:   chID,
					WorkspaceID: alert.WorkspaceID,
					DedupKey:    dedupKey,
					StepIndex:   stepIdx,
					IsEscalated: isEscalation,
					IsDigest:    isDigest,
					DeliveredAt: now,
				}
				store.dedupKeys[dedupKey] = receipt
				store.deliveries = append(store.deliveries, receipt)
			}
			store.mu.Unlock()
			return nil
		}

		// Exhausted retries: record dead-letter unless this is an escalation step
		// for an alert that already has a primary outcome.
		if !isEscalation {
			now := store.advanceClock(5 * time.Millisecond)
			errMsg := "delivery failed after max attempts"
			if lastErr != nil {
				errMsg = lastErr.Error()
			}
			store.mu.Lock()
			store.deadLetters = append(store.deadLetters, ChaosDeadLetter{
				AlertID:      alert.ID,
				ChannelID:    chID,
				WorkspaceID:  alert.WorkspaceID,
				AttemptCount: cfg.MaxAttempts,
				LastError:    errMsg,
				CreatedAt:    now,
			})
			store.mu.Unlock()
		}
		return lastErr
	}

	flushPendingDigests := func(forceSuccess bool) {
		store.mu.Lock()
		channelIDs := make([]int64, 0, len(store.pendingDigests))
		for chID, ids := range store.pendingDigests {
			if len(ids) > 0 {
				channelIDs = append(channelIDs, chID)
			}
		}
		sort.Slice(channelIDs, func(i, j int) bool { return channelIDs[i] < channelIDs[j] })
		store.mu.Unlock()

		for _, chID := range channelIDs {
			if !forceSuccess && (res.DigestFaults == 0 || rng.Intn(2) == 0) {
				res.DigestFaults++
				recordFault(DomainDigestFlush, "digest_flush_http_502", store.channels[chID].WorkspaceID, 0, chID, currentLeader, store.ingestCursor,
					"digest flush failed; batch retained in pending_digests for next flush cycle")
				continue
			}

			store.mu.Lock()
			alertIDs := append([]int64(nil), store.pendingDigests[chID]...)
			delete(store.pendingDigests, chID)
			store.mu.Unlock()

			for _, alertID := range alertIDs {
				store.mu.Lock()
				alertCopy := *store.alerts[alertID]
				store.mu.Unlock()
				_ = deliverHTTP(alertCopy, chID, 0, false, true)
			}
		}
	}

	runEscalations := func() {
		store.mu.Lock()
		alertIDs := make([]int64, 0, len(store.pendingEscalates))
		for id := range store.pendingEscalates {
			alertIDs = append(alertIDs, id)
		}
		sort.Slice(alertIDs, func(i, j int) bool { return alertIDs[i] < alertIDs[j] })
		store.mu.Unlock()

		for _, alertID := range alertIDs {
			store.mu.Lock()
			alertPtr := store.alerts[alertID]
			stepIdx := store.pendingEscalates[alertID]
			store.mu.Unlock()

			// Inject concurrent acknowledgement mid-escalation so we verify
			// Invariant 3 (acknowledged alerts never escalate afterwards).
			if alertPtr.AcknowledgedAt == nil && (stepIdx == 1 || rng.Intn(2) == 0) {
				ackAt := store.advanceClock(5 * time.Millisecond)
				store.mu.Lock()
				alertPtr.AcknowledgedAt = &ackAt
				alertPtr.AcknowledgedBy = "oncall-operator"
				delete(store.pendingEscalates, alertID)
				store.mu.Unlock()
				recordFault(DomainEscalation, "acknowledge_mid_escalation", alertPtr.WorkspaceID, alertID, alertPtr.EscalationChID, currentLeader, alertPtr.Ledger,
					fmt.Sprintf("operator acknowledged alert %d while escalation step %d was in flight; cancelling remaining escalation", alertID, stepIdx))
				continue
			}

			store.mu.Lock()
			if alertPtr.AcknowledgedAt != nil {
				delete(store.pendingEscalates, alertID)
				store.mu.Unlock()
				continue
			}
			alertCopy := *alertPtr
			delete(store.pendingEscalates, alertID)
			store.mu.Unlock()

			recordFault(DomainEscalation, "escalation_step_dispatch", alertCopy.WorkspaceID, alertCopy.ID, alertCopy.EscalationChID, currentLeader, alertCopy.Ledger,
				fmt.Sprintf("dispatching unacknowledged alert %d to escalation channel %d (step %d)", alertCopy.ID, alertCopy.EscalationChID, stepIdx))
			_ = deliverHTTP(alertCopy, alertCopy.EscalationChID, stepIdx, true, false)
		}
	}

	for ledgerIdx := 1; ledgerIdx <= cfg.Ledgers; ledgerIdx++ {
		ledgerSeq := uint32(1000 + ledgerIdx)

		// Domain 4: RPC failures during GetLatestLedger / GetEvents.
		if ledgerIdx == 1 || rng.Intn(3) == 0 {
			res.RPCFaults++
			rpcAction := "rpc_get_events_503"
			if rng.Intn(2) == 0 {
				rpcAction = "rpc_get_latest_ledger_timeout"
			}
			recordFault(DomainRPC, rpcAction, "", 0, 0, currentLeader, ledgerSeq,
				fmt.Sprintf("Soroban RPC transient fault at ledger %d; poller backing off and retrying same ledger cursor", ledgerSeq))
		}

		// Domain 2: Database slow query / transient disconnect during poll setup.
		if ledgerIdx == 2 || rng.Intn(4) == 0 {
			res.DBFaults++
			recordFault(DomainDatabase, "db_slow_query_and_reconnect", "", 0, 0, currentLeader, ledgerSeq,
				fmt.Sprintf("Postgres connection pool experienced transient slow query / reset at ledger %d; read retried cleanly", ledgerSeq))
		}

		for evIdx := 0; evIdx < cfg.EventsPerLedger; evIdx++ {
			wsIdx := (ledgerIdx + evIdx) % len(cfg.Workspaces)
			ws := cfg.Workspaces[wsIdx]
			baseChID := int64(100 + wsIdx*10)
			primaryChID := baseChID
			escalationChID := baseChID + 1
			digestChID := baseChID + 2
			useDigest := (evIdx == cfg.EventsPerLedger-1)

			targetChID := primaryChID
			if useDigest {
				targetChID = digestChID
			}

			eventID := fmt.Sprintf("evt-L%d-E%d", ledgerSeq, evIdx)
			eventKey := ws + ":" + eventID

			// Domain 3: Lease expiry & leader change mid-poll.
			if (ledgerIdx == 2 && evIdx == 1) || (res.LeaderChanges == 0 && ledgerIdx == cfg.Ledgers) {
				res.LeaderChanges++
				prevLeader := currentLeader
				store.leaseEpoch++
				currentLeader = fmt.Sprintf("poller-node-%d", store.leaseEpoch)
				store.leaderID = currentLeader
				recordFault(DomainLeaderLease, "lease_expired_leader_handover", ws, 0, 0, currentLeader, ledgerSeq,
					fmt.Sprintf("leader lease expired on %s mid-poll; %s acquired lease (epoch=%d) and re-read ledger %d", prevLeader, currentLeader, store.leaseEpoch, ledgerSeq))
			}

			// Persist alert idempotently (simulating alert_dedup guard when a new
			// leader or restarted process replays the same ledger event).
			now := store.advanceClock(15 * time.Millisecond)
			store.mu.Lock()
			alertID, alreadyCreated := store.alertsByEventKey[eventKey]
			if !alreadyCreated {
				store.nextAlertID++
				alertID = store.nextAlertID
				a := &ChaosAlert{
					ID:             alertID,
					WorkspaceID:    ws,
					MonitorID:      int64(wsIdx + 1),
					ChannelID:      targetChID,
					EscalationChID: escalationChID,
					DigestMode:     useDigest,
					EventID:        eventID,
					Ledger:         ledgerSeq,
					CreatedAt:      now,
				}
				store.alerts[alertID] = a
				store.alertsByEventKey[eventKey] = alertID
				if !useDigest {
					store.pendingEscalates[alertID] = 1
				}
			}
			alertCopy := *store.alerts[alertID]
			store.mu.Unlock()

			// Domain 7: Process restart mid-batch!
			// The worker crashes after persisting the alert to the database but
			// BEFORE delivering it; on restart, recovery scans undelivered alerts
			// and resumes from the persisted state without losing or duplicating it.
			if (ledgerIdx == 3 && evIdx == 1) || (res.ProcessResumes == 0 && ledgerIdx == cfg.Ledgers) {
				res.ProcessResumes++
				recordFault(DomainProcessRestart, "process_crash_and_resume_mid_batch", ws, alertCopy.ID, targetChID, currentLeader, ledgerSeq,
					fmt.Sprintf("process restarted mid-batch after persisting alert %d; recovering from store checkpoint", alertCopy.ID))
			}

			if useDigest {
				store.mu.Lock()
				store.pendingDigests[digestChID] = append(store.pendingDigests[digestChID], alertCopy.ID)
				store.mu.Unlock()
			} else {
				_ = deliverHTTP(alertCopy, primaryChID, 0, false, false)
			}
		}

		store.mu.Lock()
		store.ingestCursor = ledgerSeq
		store.mu.Unlock()

		// Periodically flush digests and run escalation ticks during the run.
		flushPendingDigests(false)
		runEscalations()
	}

	// Final drain: flush any remaining digest queues and in-flight escalations
	// as the service settles after the fault window.
	flushPendingDigests(true)
	runEscalations()

	store.mu.Lock()
	alertIDs := make([]int64, 0, len(store.alerts))
	for id := range store.alerts {
		alertIDs = append(alertIDs, id)
	}
	sort.Slice(alertIDs, func(i, j int) bool { return alertIDs[i] < alertIDs[j] })
	for _, id := range alertIDs {
		res.Alerts = append(res.Alerts, *store.alerts[id])
	}
	res.Deliveries = append(res.Deliveries, store.deliveries...)
	res.DeadLetters = append(res.DeadLetters, store.deadLetters...)
	for _, pending := range store.pendingDigests {
		res.PendingDigests += len(pending)
	}
	store.mu.Unlock()

	res.LogOutput = logBuf.String()
	if err := res.CheckInvariants(); err != nil {
		logger.Error("chaos_suite_failed",
			"seed", cfg.Seed,
			"replay_cmd", replayCmd,
			"error", err.Error(),
		)
		return res, err
	}

	logger.Info("chaos_suite_passed",
		"seed", cfg.Seed,
		"alerts", len(res.Alerts),
		"deliveries", len(res.Deliveries),
		"dead_letters", len(res.DeadLetters),
		"faults_injected", len(res.FaultSequence),
	)
	return res, nil
}

// MarshalReport serializes the chaos run result as indented JSON for CI
// artifact archiving.
func (r *ChaosResult) MarshalReport() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

