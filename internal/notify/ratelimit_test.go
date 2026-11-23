package notify

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/config"
)

func TestChannelRateLimiter_IndependentChannels(t *testing.T) {
	defaults := map[string]float64{
		"slack": 1.0,
	}
	man := NewChannelRateLimiter(defaults)

	lim1 := man.GetLimiter(1, "slack", 0)
	lim2 := man.GetLimiter(2, "slack", 0)

	assert.NotNil(t, lim1)
	assert.NotNil(t, lim2)
	assert.NotSame(t, lim1, lim2)
}

func TestChannelRateLimiter_Wait(t *testing.T) {
	defaults := map[string]float64{
		"webhook": 100.0,
	}
	man := NewChannelRateLimiter(defaults)

	ctx := context.Background()
	err := man.Wait(ctx, 10, "webhook", 0)
	assert.NoError(t, err)
}

// TestDispatcherUsesConfiguredRateLimits exercises the same env -> config ->
// dispatcher path main uses. Each variable must move only its own channel type;
// DEFAULT_RPS covers types without a dedicated setting.
func TestDispatcherUsesConfiguredRateLimits(t *testing.T) {
	t.Setenv("CONFIG_FILE", "")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("NOTIFY_RATE_LIMIT_SLACK_RPS", "7.5")
	t.Setenv("NOTIFY_RATE_LIMIT_TELEGRAM_RPS", "42")
	t.Setenv("NOTIFY_RATE_LIMIT_PAGERDUTY_RPS", "3.5")
	t.Setenv("NOTIFY_RATE_LIMIT_DEFAULT_RPS", "11")

	cfg, err := config.Load()
	require.NoError(t, err)

	d := NewDispatcher(nil, nil, slog.New(slog.DiscardHandler)).
		WithRateLimits(RateLimits{
			SlackRPS:     cfg.NotifyRateLimitSlackRPS,
			TelegramRPS:  cfg.NotifyRateLimitTelegramRPS,
			PagerDutyRPS: cfg.NotifyRateLimitPagerDutyRPS,
			DefaultRPS:   cfg.NotifyRateLimitDefaultRPS,
		})

	for channelID, tc := range map[int64]struct {
		channelType string
		want        float64
	}{
		1: {"slack", 7.5},
		2: {"telegram", 42},
		3: {"pagerduty", 3.5},
		4: {"discord", 11},
		5: {"webhook", 11},
	} {
		lim := d.rateLimiter.GetLimiter(channelID, tc.channelType, 0)
		assert.Equal(t, tc.want, float64(lim.Limit()), tc.channelType)
	}
}

func TestRateLimitsZeroUsesBuiltInDefaults(t *testing.T) {
	d := NewDispatcher(nil, nil, slog.New(slog.DiscardHandler)).WithRateLimits(RateLimits{})

	for channelID, tc := range map[int64]struct {
		channelType string
		want        float64
	}{
		1: {"slack", DefaultSlackRPS},
		2: {"telegram", DefaultTelegramRPS},
		3: {"pagerduty", DefaultPagerDutyRPS},
		4: {"unknown", DefaultFallbackRPS},
	} {
		lim := d.rateLimiter.GetLimiter(channelID, tc.channelType, 0)
		assert.Equal(t, tc.want, float64(lim.Limit()), tc.channelType)
	}
}

func TestParseRetryAfter_Seconds(t *testing.T) {
	d, ok := ParseRetryAfter("120")
	assert.True(t, ok)
	assert.Equal(t, 120*time.Second, d)
}

func TestParseRetryAfter_Empty(t *testing.T) {
	_, ok := ParseRetryAfter("")
	assert.False(t, ok)
}
