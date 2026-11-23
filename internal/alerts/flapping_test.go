package alerts

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDetectFlapping(t *testing.T) {
	base := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	config := FlappingConfig{Episodes: 3, Window: 15 * time.Minute, QuietAfter: 2 * time.Minute}
	tests := []struct {
		name               string
		offsets            []time.Duration
		at                 time.Duration
		wantFlapping       bool
		wantEpisodes       int
		wantCurrentEpisode int
	}{
		{
			name:               "fewer than threshold",
			offsets:            []time.Duration{0, 5 * time.Minute},
			at:                 5 * time.Minute,
			wantEpisodes:       2,
			wantCurrentEpisode: 1,
		},
		{
			name:               "exactly threshold",
			offsets:            []time.Duration{0, 5 * time.Minute, 10 * time.Minute},
			at:                 10 * time.Minute,
			wantFlapping:       true,
			wantEpisodes:       3,
			wantCurrentEpisode: 1,
		},
		{
			name:               "exact quiet gap stays one episode",
			offsets:            []time.Duration{0, 2 * time.Minute, 4 * time.Minute},
			at:                 4 * time.Minute,
			wantEpisodes:       1,
			wantCurrentEpisode: 3,
		},
		{
			name:               "alert beyond quiet gap starts episode",
			offsets:            []time.Duration{0, 2*time.Minute + time.Second},
			at:                 2*time.Minute + time.Second,
			wantEpisodes:       2,
			wantCurrentEpisode: 1,
		},
		{
			name:               "settles as old episodes leave window",
			offsets:            []time.Duration{0, 5 * time.Minute, 10 * time.Minute},
			at:                 16 * time.Minute,
			wantEpisodes:       2,
			wantCurrentEpisode: 1,
		},
		{
			name:               "episode straddling window edge counts once",
			offsets:            []time.Duration{-16 * time.Minute, -14 * time.Minute, -13 * time.Minute},
			at:                 0,
			wantEpisodes:       1,
			wantCurrentEpisode: 2,
		},
		{
			name:               "future alerts are ignored and input is sorted",
			offsets:            []time.Duration{10 * time.Minute, 0, 20 * time.Minute},
			at:                 10 * time.Minute,
			wantEpisodes:       2,
			wantCurrentEpisode: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			times := make([]time.Time, 0, len(tt.offsets))
			for _, offset := range tt.offsets {
				times = append(times, base.Add(offset))
			}
			state := DetectFlapping(times, base.Add(tt.at), config)
			require.Equal(t, tt.wantFlapping, state.Flapping)
			require.Equal(t, tt.wantEpisodes, state.Episodes)
			require.Equal(t, tt.wantCurrentEpisode, state.CurrentEpisodeAlerts)
		})
	}
}
