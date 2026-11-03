package notify

import (
	"errors"
	"net/textproto"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/store"
)

func TestChannelHealth_OutcomeClassification(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected OutcomeClass
	}{
		{
			name:     "nil is success",
			err:      nil,
			expected: OutcomeSuccess,
		},
		{
			name:     "429 is rate limited",
			err:      &HTTPStatusError{StatusCode: 429, Body: "Too Many Requests"},
			expected: OutcomeRateLimited,
		},
		{
			name:     "408 is rate limited",
			err:      &HTTPStatusError{StatusCode: 408, Body: "Request Timeout"},
			expected: OutcomeRateLimited,
		},
		{
			name:     "401 is auth failure",
			err:      &HTTPStatusError{StatusCode: 401, Body: "Unauthorized"},
			expected: OutcomeAuthFailure,
		},
		{
			name:     "SMTP 535 is auth failure",
			err:      &textproto.Error{Code: 535, Msg: "Authentication credentials invalid"},
			expected: OutcomeAuthFailure,
		},
		{
			name:     "403 is permanent rejection",
			err:      &HTTPStatusError{StatusCode: 403, Body: "Forbidden"},
			expected: OutcomePermanent,
		},
		{
			name:     "404 is permanent rejection",
			err:      &HTTPStatusError{StatusCode: 404, Body: "Not Found"},
			expected: OutcomePermanent,
		},
		{
			name:     "500 is transient server error",
			err:      &HTTPStatusError{StatusCode: 500, Body: "Internal Server Error"},
			expected: OutcomeTransientServer,
		},
		{
			name:     "503 is transient server error",
			err:      &HTTPStatusError{StatusCode: 503, Body: "Service Unavailable"},
			expected: OutcomeTransientServer,
		},
		{
			name:     "context deadline exceeded is timeout",
			err:      errors.New("post: context deadline exceeded"),
			expected: OutcomeTimeout,
		},
		{
			name:     "i/o timeout is timeout",
			err:      errors.New("dial tcp: i/o timeout"),
			expected: OutcomeTimeout,
		},
		{
			name:     "400 bad request is client side issue",
			err:      &HTTPStatusError{StatusCode: 400, Body: "Bad Request"},
			expected: OutcomeUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, ClassifyOutcome(tt.err))
		})
	}
}

func TestChannelHealth_RateLimitingNeverDisables(t *testing.T) {
	mgr := NewChannelHealthManager()
	now := time.Now()
	channelID := int64(12)

	// Simulate 20 consecutive 429 rate-limited responses
	err429 := &HTTPStatusError{StatusCode: 429, Body: "rate limited"}
	for i := 0; i < 20; i++ {
		score, disabled, outcome := mgr.RecordOutcome(channelID, err429, now.Add(time.Duration(i)*time.Second))
		assert.False(t, disabled, "rate limiting must never auto-disable a channel")
		assert.Equal(t, OutcomeRateLimited, outcome)
		assert.Equal(t, 100, score, "rate limiting must never decrease health score")
	}

	health := mgr.GetHealth(channelID)
	assert.NotEqual(t, ChannelStatusDisabled, health.Status)
	assert.Equal(t, 100, health.Score)
}

func TestChannelHealth_AuthFailureNonRetryableAndHumanAttention(t *testing.T) {
	authErr := &HTTPStatusError{StatusCode: 401, Body: "Token Revoked"}
	assert.False(t, Retryable(authErr), "auth failure must not be retried indefinitely")

	mgr := NewChannelHealthManager()
	now := time.Now()
	channelID := int64(34)

	// First auth failure: score drops by 50, flags NeedsHumanAttention
	score1, disabled1, outcome1 := mgr.RecordOutcome(channelID, authErr, now)
	assert.Equal(t, OutcomeAuthFailure, outcome1)
	assert.Equal(t, 50, score1)
	assert.False(t, disabled1)

	h1 := mgr.GetHealth(channelID)
	assert.True(t, h1.NeedsHumanAttention, "auth failure must be surfaced for human attention")
	assert.Equal(t, ChannelStatusDegraded, h1.Status)

	// Second auth failure: score drops to 0, triggers auto-disable!
	score2, disabled2, outcome2 := mgr.RecordOutcome(channelID, authErr, now.Add(1*time.Second))
	assert.Equal(t, OutcomeAuthFailure, outcome2)
	assert.Equal(t, 0, score2)
	assert.True(t, disabled2, "second auth failure must trigger auto-disable")

	h2 := mgr.GetHealth(channelID)
	assert.Equal(t, ChannelStatusDisabled, h2.Status)
}

func TestChannelHealth_AbsenceOfDeliveriesIsUnknown(t *testing.T) {
	mgr := NewChannelHealthManager()
	// Channel that has never received a delivery
	health := mgr.GetHealth(999)
	assert.Equal(t, ChannelStatusUnknown, health.Status, "absence of failure is not evidence of health")
	assert.Equal(t, 0, health.Deliveries)
}

func TestChannelHealth_AutomaticRecoveryViaSyntheticProbe(t *testing.T) {
	mgr := NewChannelHealthManager()
	now := time.Now()
	channelID := int64(56)

	// Disable channel via permanent errors
	permErr := &HTTPStatusError{StatusCode: 404, Body: "Not Found"}
	mgr.RecordOutcome(channelID, permErr, now)
	_, disabled, _ := mgr.RecordOutcome(channelID, permErr, now.Add(1*time.Second))
	require.True(t, disabled)
	assert.Equal(t, ChannelStatusDisabled, mgr.GetHealth(channelID).Status)

	// Synthetic diagnostic probe succeeds!
	mgr.ProbeSuccess(channelID, now.Add(10*time.Minute))

	recovered := mgr.GetHealth(channelID)
	assert.Equal(t, ChannelStatusHealthy, recovered.Status)
	assert.Equal(t, 100, recovered.Score)
	assert.Equal(t, 0, recovered.ConsecutiveFailures)
	assert.False(t, recovered.NeedsHumanAttention)
}

func TestChannelHealth_HistoryVisibility(t *testing.T) {
	mgr := NewChannelHealthManager()
	now := time.Now()
	channelID := int64(78)

	// Record sequence of mixed events
	mgr.RecordOutcome(channelID, nil, now)                                                         // +25 -> 100
	mgr.RecordOutcome(channelID, errors.New("timeout: context deadline exceeded"), now.Add(1*time.Minute)) // -15 -> 85
	mgr.RecordOutcome(channelID, &HTTPStatusError{StatusCode: 500}, now.Add(2*time.Minute))                // -20 -> 65

	h := mgr.GetHealth(channelID)
	require.Len(t, h.History, 3)
	assert.Equal(t, OutcomeSuccess, h.History[0].Outcome)
	assert.Equal(t, OutcomeTimeout, h.History[1].Outcome)
	assert.Equal(t, OutcomeTransientServer, h.History[2].Outcome)
	assert.Equal(t, 65, h.Score)
}

func TestChannelHealth_LoudDisableNotification(t *testing.T) {
	disabledCh := store.Channel{
		ID:   42,
		Name: "ops-webhook",
		Type: "webhook",
	}

	alert := BuildDisabledNotification(7, "treasury-monitor", disabledCh, "401 Unauthorized (token revoked)")
	assert.Equal(t, int64(7), alert.MonitorID)
	assert.Equal(t, "critical", alert.Severity)
	assert.Contains(t, alert.Digest, "ops-webhook")
	assert.Contains(t, alert.Digest, "auto-disabled")
	assert.Contains(t, alert.Digest, "401 Unauthorized")
}
