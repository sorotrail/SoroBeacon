package notify

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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

func TestParseRetryAfter_Seconds(t *testing.T) {
	d, ok := ParseRetryAfter("120")
	assert.True(t, ok)
	assert.Equal(t, 120*time.Second, d)
}

func TestParseRetryAfter_Empty(t *testing.T) {
	_, ok := ParseRetryAfter("")
	assert.False(t, ok)
}
