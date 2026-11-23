package alerts

import (
	"sort"
	"time"
)

const (
	DefaultFlappingEpisodes   = 3
	DefaultFlappingWindow     = 15 * time.Minute
	DefaultFlappingQuietAfter = 2 * time.Minute
)

// FlappingConfig controls how alert timestamps are grouped into episodes.
type FlappingConfig struct {
	Episodes   int
	Window     time.Duration
	QuietAfter time.Duration
}

// FlappingState is the result of evaluating a rule's alert history.
type FlappingState struct {
	Flapping             bool
	Episodes             int
	AlertCount           int
	CurrentEpisodeAlerts int
}

// DetectFlapping groups alerts no older than the configured window into
// episodes. A gap exactly equal to QuietAfter remains in the same episode.
// Alert times may be unsorted; entries after at are ignored.
func DetectFlapping(alertTimes []time.Time, at time.Time, config FlappingConfig) FlappingState {
	if config.Episodes <= 0 {
		config.Episodes = DefaultFlappingEpisodes
	}
	if config.Window <= 0 {
		config.Window = DefaultFlappingWindow
	}
	if config.QuietAfter <= 0 {
		config.QuietAfter = DefaultFlappingQuietAfter
	}

	cutoff := at.Add(-config.Window)
	// Keep one quiet period of history to recognize an episode that started
	// just before the window and continues inside it.
	contextCutoff := cutoff.Add(-config.QuietAfter)
	times := make([]time.Time, 0, len(alertTimes))
	for _, alertTime := range alertTimes {
		if alertTime.Before(contextCutoff) || alertTime.After(at) {
			continue
		}
		times = append(times, alertTime)
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })

	state := FlappingState{}
	currentEpisodeInWindow := false
	for i, alertTime := range times {
		startsEpisode := i == 0 || alertTime.Sub(times[i-1]) > config.QuietAfter
		if startsEpisode {
			currentEpisodeInWindow = false
			state.CurrentEpisodeAlerts = 0
		}
		if !alertTime.Before(cutoff) {
			state.AlertCount++
			if startsEpisode || !currentEpisodeInWindow {
				state.Episodes++
			}
			currentEpisodeInWindow = true
		}
		if currentEpisodeInWindow {
			state.CurrentEpisodeAlerts++
		}
	}
	state.Flapping = state.Episodes >= config.Episodes
	return state
}
