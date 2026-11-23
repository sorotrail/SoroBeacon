package metrics

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	channelUnackRateName    = "sorobeacon_channel_unacknowledged_delivery_rate"
	databaseUnavailableName = "sorobeacon_database_unavailable_total"
)

func TestSetChannelUnacknowledgedRate(t *testing.T) {
	type set struct {
		channelID   string
		channelType string
		rate        float64
	}
	tests := []struct {
		name   string
		sets   []set
		labels map[string]string
		want   float64
	}{
		{
			name:   "records the rate for a channel",
			sets:   []set{{"7", "webhook", 0.25}},
			labels: map[string]string{"channel_id": "7", "channel_type": "webhook"},
			want:   0.25,
		},
		{
			name:   "later value replaces earlier one",
			sets:   []set{{"7", "webhook", 0.9}, {"7", "webhook", 0.1}},
			labels: map[string]string{"channel_id": "7", "channel_type": "webhook"},
			want:   0.1,
		},
		{
			name:   "series are kept per channel",
			sets:   []set{{"7", "webhook", 0.5}, {"8", "slack", 1}},
			labels: map[string]string{"channel_id": "8", "channel_type": "slack"},
			want:   1,
		},
		{
			name:   "zero rate is reported, not dropped",
			sets:   []set{{"9", "email", 0}},
			labels: map[string]string{"channel_id": "9", "channel_type": "email"},
			want:   0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := New()
			for _, s := range tt.sets {
				m.SetChannelUnacknowledgedRate(s.channelID, s.channelType, s.rate)
			}
			assert.Equal(t, tt.want, m.sampleValue(t, channelUnackRateName, tt.labels))
		})
	}
}

func TestSetChannelUnacknowledgedRateNilReceiver(t *testing.T) {
	var m *Metrics
	assert.NotPanics(t, func() {
		m.SetChannelUnacknowledgedRate("7", "webhook", 0.5)
	})
}

func TestRecordDatabaseUnavailable(t *testing.T) {
	tests := []struct {
		name  string
		calls int
		want  float64
	}{
		{name: "starts at zero", calls: 0, want: 0},
		{name: "single failure", calls: 1, want: 1},
		{name: "accumulates across failures", calls: 3, want: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := New()
			for i := 0; i < tt.calls; i++ {
				m.RecordDatabaseUnavailable()
			}
			assert.Equal(t, tt.want, m.sampleValue(t, databaseUnavailableName, nil))
		})
	}
}

func TestRecordDatabaseUnavailableNilSafe(t *testing.T) {
	tests := []struct {
		name string
		m    *Metrics
	}{
		{name: "nil receiver", m: nil},
		{name: "unset counter", m: &Metrics{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.NotPanics(t, tt.m.RecordDatabaseUnavailable)
		})
	}
}
