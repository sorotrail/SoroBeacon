package notify

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/textproto"
	"strings"
	"sync"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// OutcomeClass categorizes delivery outcomes into weighted classes.
type OutcomeClass string

const (
	// OutcomeSuccess is a successful delivery.
	OutcomeSuccess OutcomeClass = "success"
	// OutcomeRateLimited is a 429, 408, or 425 response asking to slow down.
	// Rate limiting NEVER counts toward auto-disabling.
	OutcomeRateLimited OutcomeClass = "rate_limited"
	// OutcomeTimeout indicates network or context timeouts.
	OutcomeTimeout OutcomeClass = "timeout"
	// OutcomeTransientServer represents 5xx errors from the provider.
	OutcomeTransientServer OutcomeClass = "transient_server"
	// OutcomeAuthFailure is a credential revocation (401 or SMTP 535) that
	// cannot heal on its own and requires human intervention.
	OutcomeAuthFailure OutcomeClass = "auth_failure"
	// OutcomePermanent is a permanent rejection (403, 404, SMTP 550) where
	// repeating the request will never succeed.
	OutcomePermanent OutcomeClass = "permanent_rejection"
	// OutcomeUnknown is an unclassified error, treated defensively.
	OutcomeUnknown OutcomeClass = "unknown"
)

// ChannelStatus indicates the observed health state of a channel.
type ChannelStatus string

const (
	// ChannelStatusUnknown marks a channel with no delivery attempts.
	// Absence of failure is explicitly not evidence of health.
	ChannelStatusUnknown  ChannelStatus = "unknown"
	ChannelStatusHealthy  ChannelStatus = "healthy"
	ChannelStatusDegraded ChannelStatus = "degraded"
	ChannelStatusDisabled ChannelStatus = "disabled"
)

// HealthEvent records one point in a channel's health history for explainability.
type HealthEvent struct {
	At      time.Time    `json:"at"`
	Outcome OutcomeClass `json:"outcome"`
	Score   int          `json:"score"`
	Error   string       `json:"error,omitempty"`
}

// ChannelHealthScore maintains a channel's health score (0-100), history, and auto-disable status.
type ChannelHealthScore struct {
	ChannelID           int64
	Score               int
	Status              ChannelStatus
	Deliveries          int
	ConsecutiveFailures int
	LastOutcome         OutcomeClass
	LastOutcomeAt       time.Time
	LastSuccessAt       time.Time
	LastErrorAt         time.Time
	NeedsHumanAttention bool
	History             []HealthEvent
}

// DefaultInitialHealthScore is the score assigned to a newly active channel.
const DefaultInitialHealthScore = 100

// DefaultMaxHealthHistory is the number of historical health events retained.
const DefaultMaxHealthHistory = 20

// ChannelHealthManager coordinates outcome-based health scoring and loud disabling.
type ChannelHealthManager struct {
	mu         sync.Mutex
	scores     map[int64]*ChannelHealthScore
	maxHistory int
	threshold  int // Score at or below which auto-disable occurs (default 0)
}

// NewChannelHealthManager creates a channel health manager.
func NewChannelHealthManager() *ChannelHealthManager {
	return &ChannelHealthManager{
		scores:     make(map[int64]*ChannelHealthScore),
		maxHistory: DefaultMaxHealthHistory,
		threshold:  0,
	}
}

// GetHealth returns the current health score state for channelID.
func (m *ChannelHealthManager) GetHealth(channelID int64) ChannelHealthScore {
	m.mu.Lock()
	defer m.mu.Unlock()

	if hs, ok := m.scores[channelID]; ok {
		cp := *hs
		cp.History = append([]HealthEvent(nil), hs.History...)
		return cp
	}

	return ChannelHealthScore{
		ChannelID: channelID,
		Score:     DefaultInitialHealthScore,
		Status:    ChannelStatusUnknown,
	}
}

// RecordOutcome folds one delivery result into the channel's weighted health score.
// Returns the updated score, whether auto-disable was triggered, and the outcome class.
func (m *ChannelHealthManager) RecordOutcome(channelID int64, err error, at time.Time) (score int, disabled bool, outcome OutcomeClass) {
	if at.IsZero() {
		at = time.Now()
	}

	outcome = ClassifyOutcome(err)

	m.mu.Lock()
	defer m.mu.Unlock()

	hs, ok := m.scores[channelID]
	if !ok {
		hs = &ChannelHealthScore{
			ChannelID: channelID,
			Score:     DefaultInitialHealthScore,
			Status:    ChannelStatusUnknown,
		}
		m.scores[channelID] = hs
	}

	hs.Deliveries++
	hs.LastOutcome = outcome
	hs.LastOutcomeAt = at

	switch outcome {
	case OutcomeSuccess:
		hs.Score += 25
		if hs.Score > 100 {
			hs.Score = 100
		}
		hs.ConsecutiveFailures = 0
		hs.LastSuccessAt = at
		hs.Status = ChannelStatusHealthy
		hs.NeedsHumanAttention = false

	case OutcomeRateLimited:
		// Rate limiting NEVER counts toward disabling.
		// A channel asking to be slowed down is healthy; treating it as failing is backwards.
		if hs.Status == ChannelStatusUnknown {
			hs.Status = ChannelStatusHealthy
		}

	case OutcomeTimeout:
		hs.Score -= 15
		hs.ConsecutiveFailures++
		hs.LastErrorAt = at
		if hs.Score > m.threshold {
			hs.Status = ChannelStatusDegraded
		}

	case OutcomeTransientServer:
		hs.Score -= 20
		hs.ConsecutiveFailures++
		hs.LastErrorAt = at
		if hs.Score > m.threshold {
			hs.Status = ChannelStatusDegraded
		}

	case OutcomeAuthFailure:
		// Authentication failures (expired tokens / rotated credentials) are heavily penalised
		// and flagged for immediate human attention since retries cannot fix them.
		hs.Score -= 50
		hs.ConsecutiveFailures++
		hs.LastErrorAt = at
		hs.NeedsHumanAttention = true
		if hs.Score > m.threshold {
			hs.Status = ChannelStatusDegraded
		}

	case OutcomePermanent:
		// 403, 404, or rejected recipient
		hs.Score -= 50
		hs.ConsecutiveFailures++
		hs.LastErrorAt = at
		if hs.Score > m.threshold {
			hs.Status = ChannelStatusDegraded
		}

	case OutcomeUnknown:
		hs.Score -= 10
		hs.ConsecutiveFailures++
		hs.LastErrorAt = at
		if hs.Score > m.threshold {
			hs.Status = ChannelStatusDegraded
		}
	}

	if hs.Score < 0 {
		hs.Score = 0
	}

	// Trigger auto-disable if score reaches or falls below threshold
	if hs.Score <= m.threshold {
		hs.Status = ChannelStatusDisabled
		disabled = true
	}

	errMsg := ""
	if err != nil {
		errMsg = sanitizeDeliveryError(redactURLError(err)).Error()
	}

	event := HealthEvent{
		At:      at,
		Outcome: outcome,
		Score:   hs.Score,
		Error:   errMsg,
	}

	hs.History = append(hs.History, event)
	if len(hs.History) > m.maxHistory {
		hs.History = hs.History[len(hs.History)-m.maxHistory:]
	}

	return hs.Score, disabled, outcome
}

// ProbeSuccess records a successful synthetic health probe on a disabled or degraded channel,
// re-enabling it and restoring healthy score.
func (m *ChannelHealthManager) ProbeSuccess(channelID int64, at time.Time) {
	if at.IsZero() {
		at = time.Now()
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	hs, ok := m.scores[channelID]
	if !ok {
		hs = &ChannelHealthScore{ChannelID: channelID}
		m.scores[channelID] = hs
	}

	hs.Score = 100
	hs.Status = ChannelStatusHealthy
	hs.ConsecutiveFailures = 0
	hs.LastSuccessAt = at
	hs.LastOutcome = OutcomeSuccess
	hs.LastOutcomeAt = at
	hs.NeedsHumanAttention = false

	hs.History = append(hs.History, HealthEvent{
		At:      at,
		Outcome: OutcomeSuccess,
		Score:   100,
	})
	if len(hs.History) > m.maxHistory {
		hs.History = hs.History[len(hs.History)-m.maxHistory:]
	}
}

// ClassifyOutcome categorizes an error into a precise OutcomeClass.
func ClassifyOutcome(err error) OutcomeClass {
	if err == nil {
		return OutcomeSuccess
	}

	var httpErr *HTTPStatusError
	if errors.As(err, &httpErr) {
		return classifyHTTPStatus(httpErr.StatusCode)
	}

	var statusErr *StatusError
	if errors.As(err, &statusErr) {
		return classifyHTTPStatus(statusErr.Code)
	}

	var smtpErr *textproto.Error
	if errors.As(err, &smtpErr) {
		if smtpErr.Code == 535 {
			return OutcomeAuthFailure
		}
		if smtpErr.Code >= 500 {
			return OutcomePermanent
		}
		return OutcomeTransientServer
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return OutcomeTimeout
	}

	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "timeout") {
		return OutcomeTimeout
	}

	return OutcomeUnknown
}

func classifyHTTPStatus(code int) OutcomeClass {
	switch code {
	case http.StatusUnauthorized:
		return OutcomeAuthFailure
	case http.StatusForbidden, http.StatusNotFound:
		return OutcomePermanent
	case http.StatusTooManyRequests, http.StatusRequestTimeout, http.StatusTooEarly:
		return OutcomeRateLimited
	case http.StatusBadRequest:
		// Bad request is a client-side payload issue, not held against channel health
		return OutcomeUnknown
	default:
		if code >= 500 && code <= 599 {
			return OutcomeTransientServer
		}
	}
	return OutcomeUnknown
}

// TestHealthUpdate maps the outcome of a "send test" click onto the health
// update that records it, so the API and the dashboard report the same thing
// about the same button.
func TestHealthUpdate(sendErr error, at time.Time) store.ChannelHealthUpdate {
	u := store.ChannelHealthUpdate{At: at}
	if sendErr == nil {
		u.Success = true
		return u
	}
	u.Error = sanitizeDeliveryError(redactURLError(sendErr)).Error()
	u.Permanent = IsPermanent(sendErr)
	return u
}

// IsPermanent reports whether err is a failure the channel will not recover
// from on its own, as opposed to one that heals by itself.
func IsPermanent(err error) bool {
	if err == nil {
		return false
	}
	outcome := ClassifyOutcome(err)
	return outcome == OutcomePermanent || outcome == OutcomeAuthFailure
}

// Retryable reports whether another delivery attempt could plausibly succeed.
//
// A 4xx that is not a "come back later" answer (408, 425, 429) is the receiver
// rejecting the request itself or our payload, and repeating it unchanged changes
// nothing. Authentication failures (401 or SMTP 535) are likewise non-retryable
// because expired credentials require human attention rather than retrying.
func Retryable(err error) bool {
	if err == nil {
		return true
	}
	var httpErr *HTTPStatusError
	if errors.As(err, &httpErr) {
		return retryableStatus(httpErr.StatusCode)
	}
	var statusErr *StatusError
	if errors.As(err, &statusErr) {
		return retryableStatus(statusErr.Code)
	}
	var smtpErr *textproto.Error
	if errors.As(err, &smtpErr) {
		// 535 authentication failure must not be retried indefinitely
		if smtpErr.Code == 535 {
			return false
		}
	}
	return true
}

func retryableStatus(code int) bool {
	if code < 400 || code > 499 {
		return true
	}
	switch code {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	}
	return false
}

// BuildDisabledNotification creates a loud summary notification to be delivered
// through an alternate channel when a primary channel is auto-disabled.
func BuildDisabledNotification(monitorID int64, monitorName string, disabledCh store.Channel, reason string) Alert {
	return Alert{
		MonitorID:   monitorID,
		MonitorName: monitorName,
		Severity:    "critical",
		Digest: fmt.Sprintf("CRITICAL: Notification channel %q (type: %s, ID: %d) was auto-disabled due to health failure (%s). Immediate attention required.",
			disabledCh.Name, disabledCh.Type, disabledCh.ID, reason),
	}
}
