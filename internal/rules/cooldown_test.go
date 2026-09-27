package rules

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCooldown(t *testing.T) {
	tests := []struct {
		name    string
		params  string
		want    time.Duration
		wantErr bool
	}{
		{"absent", `{"event_name": "transfer"}`, 0, false},
		{"empty object", `{}`, 0, false},
		{"empty string means unset", `{"cooldown": ""}`, 0, false},
		{"valid minutes", `{"cooldown": "5m"}`, 5 * time.Minute, false},
		{"valid seconds", `{"cooldown": "90s"}`, 90 * time.Second, false},
		{"zero", `{"cooldown": "0"}`, 0, false},
		{"malformed params are the evaluator's problem", `"nope"`, 0, false},
		{"invalid duration", `{"cooldown": "5 minutes"}`, 0, true},
		{"negative", `{"cooldown": "-1m"}`, 0, true},
		{"a bare number is rejected", `{"cooldown": 300}`, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseCooldown(json.RawMessage(tt.params))
			if tt.wantErr {
				require.Error(t, err)
				var fe FieldError
				require.ErrorAs(t, err, &fe)
				assert.Equal(t, CooldownParam, fe.Field)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestRegistryValidatesCooldown pins the cross-cutting part: cooldown is
// checked for every rule type even though no evaluator owns it, so the API
// rejects a typo at create time.
func TestRegistryValidatesCooldown(t *testing.T) {
	r := NewRegistry()

	err := r.Validate(TypeEventEmitted, json.RawMessage(`{"event_name": "transfer", "cooldown": "nope"}`))
	require.Error(t, err)
	var fe FieldError
	require.ErrorAs(t, err, &fe)
	assert.Equal(t, CooldownParam, fe.Field)

	require.NoError(t, r.Validate(TypeValueThreshold,
		json.RawMessage(`{"comparison": "gt", "threshold": 1, "cooldown": "5m"}`)))
}
