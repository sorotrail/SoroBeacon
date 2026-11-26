package poller

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/rules"
)

// WithSlowRuleThreshold decides whether a rule evaluation is slow enough to
// warn about. Both a configured threshold and the documented zero (disabled)
// value are checked through a real evaluation, because the log line is the
// only observable output of the setting.
func TestWithSlowRuleThreshold(t *testing.T) {
	tests := []struct {
		name      string
		threshold time.Duration
		wantWarn  bool
	}{
		{
			name:      "evaluation over the threshold is logged",
			threshold: 10 * time.Millisecond,
			wantWarn:  true,
		},
		{
			name:      "zero disables slow rule logging",
			threshold: 0,
			wantWarn:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&logs, nil))
			reg := rules.NewRegistry()
			reg.Register("slow_eval", &controllableRuleEvaluator{delay: 30 * time.Millisecond})

			p := New(nil, nil, reg, nil, time.Second, log)
			// The option is fluent: callers chain it onto New.
			assert.Same(t, p, p.WithSlowRuleThreshold(tc.threshold))
			assert.Equal(t, tc.threshold, p.slowRuleThreshold)

			_, err := reg.Evaluate(context.Background(), "slow_eval", decodedTransfer(t, "ev-1"), json.RawMessage(`{}`))
			require.NoError(t, err)

			if tc.wantWarn {
				assert.Contains(t, logs.String(), "slow rule evaluation")
				return
			}
			assert.NotContains(t, logs.String(), "slow rule evaluation")
		})
	}
}
