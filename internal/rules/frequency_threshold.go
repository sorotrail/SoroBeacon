package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// frequencyThresholdParams configure the frequency_threshold rule.
//
//	{
//	  "event_name": "transfer",   // optional: only count these events
//	  "count": 50,                // required: fire at this many matches
//	  "window": "5m"              // required: inside this rolling window
//	}
//
// Where value_threshold judges a single event, frequency_threshold judges a
// rolling-window aggregate: "more than N matching events within M minutes",
// the shape a mint storm, a drain attack or oracle flapping actually has.
type frequencyThresholdParams struct {
	EventName string `json:"event_name"`
	Count     int    `json:"count"`
	Window    string `json:"window"`
}

// MatchRecord is one stored match, used to rebuild a frequency rule's window
// after a restart.
type MatchRecord struct {
	At      time.Time
	EventID string
}

// MatchLog is the durable record a frequency_threshold rule consults once per
// rule to rebuild its in-memory window after a restart. Without it a restart
// would forget that the rule had already fired and alert again for the same
// episode; with it, the count and firing state survive the process.
//
// Implementations return the stored matches for one rule at or after since.
// Order is irrelevant — the rule sorts them.
type MatchLog interface {
	RecentMatches(ctx context.Context, ruleID int64, since time.Time) ([]MatchRecord, error)
}

// MatchLogFunc adapts a function to MatchLog.
type MatchLogFunc func(ctx context.Context, ruleID int64, since time.Time) ([]MatchRecord, error)

// RecentMatches calls f.
func (f MatchLogFunc) RecentMatches(ctx context.Context, ruleID int64, since time.Time) ([]MatchRecord, error) {
	return f(ctx, ruleID, since)
}

// FrequencyThreshold fires when the number of matching events inside a rolling
// window reaches count.
//
// Re-arm semantics: the rule fires once on each crossing of the threshold and
// then stays quiet for one full window. If the count is still being met when
// that window has elapsed it fires again — so a genuinely sustained condition
// alerts at most once per window rather than per event. A crossing that has
// passed produces no further alerts: as matches age out of the window a slow
// trickle never accumulates into a false positive.
//
// Unlike the other evaluators this one is stateful: it keeps a per-rule
// rolling window in memory (keyed by rules.RuleID from the context the poller
// sets) and rebuilds it from the MatchLog on the first event after a restart,
// so the count and the firing state are not silently reset.
type FrequencyThreshold struct {
	mu     sync.Mutex
	states map[int64]*freqState
	// now is injectable so tests can move the clock without sleeping.
	now func() time.Time
	// matchLog rebuilds state after a restart; nil disables the rebuild.
	matchLog MatchLog
}

// freqState is one rule's rolling window and firing state.
type freqState struct {
	// matches are the match times still inside the window, oldest first.
	matches []time.Time
	// armed is true between a crossing and the end of its window: while
	// armed the rule does not fire again.
	armed bool
	// episode is the window start of the current firing. It is the synthetic
	// event id, stable for the episode, so the store's (rule_id, event_id)
	// dedup guard collapses a replay into the alert already stored.
	episode time.Time
	// seeded records that the state was rebuilt (or attempted) once.
	seeded bool
}

// frequencyEventPrefix marks the synthetic alert id a frequency rule stores
// its alert under. It keeps the id recognisable in the alerts table and lets
// a restart recover the episode start from the stored row.
const frequencyEventPrefix = "frequency:"

// NewFrequencyThreshold returns a frequency rule with a real clock and no
// restart rebuild; wire a MatchLog with WithMatchLog to persist across
// restarts.
func NewFrequencyThreshold() *FrequencyThreshold {
	return &FrequencyThreshold{states: map[int64]*freqState{}, now: time.Now}
}

// WithClock overrides the clock, for tests.
func (f *FrequencyThreshold) WithClock(now func() time.Time) *FrequencyThreshold {
	f.now = now
	return f
}

// WithMatchLog attaches the durable record used to rebuild state after a
// restart.
func (f *FrequencyThreshold) WithMatchLog(l MatchLog) *FrequencyThreshold {
	f.matchLog = l
	return f
}

func (f *FrequencyThreshold) clock() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Now()
}

// state returns the per-rule state, creating it lazily so the zero value works.
func (f *FrequencyThreshold) state(ruleID int64) *freqState {
	if f.states == nil {
		f.states = map[int64]*freqState{}
	}
	s := f.states[ruleID]
	if s == nil {
		s = &freqState{}
		f.states[ruleID] = s
	}
	return s
}

func (*FrequencyThreshold) Validate(params json.RawMessage) error {
	p, err := parseFrequencyThreshold(params)
	if err != nil {
		return err
	}
	var details FieldErrors
	if p.Count <= 0 {
		details = append(details, FieldError{
			Field:  "count",
			Reason: "frequency_threshold: count must be a positive integer",
		})
	}
	switch d, err := time.ParseDuration(p.Window); {
	case p.Window == "":
		details = append(details, FieldError{
			Field:  "window",
			Reason: `frequency_threshold: window is required (a Go duration such as "5m")`,
		})
	case err != nil:
		details = append(details, FieldError{
			Field:  "window",
			Reason: fmt.Sprintf(`frequency_threshold: invalid window %q (want a Go duration such as "5m")`, p.Window),
		})
	case d <= 0:
		details = append(details, FieldError{
			Field:  "window",
			Reason: "frequency_threshold: window must be positive",
		})
	}
	if len(details) > 0 {
		return details
	}
	return nil
}

// EventNames reports the event this rule counts. A rule with no event_name
// counts every event, so its contract cannot be narrowed server-side.
func (*FrequencyThreshold) EventNames(params json.RawMessage) ([]string, bool) {
	p, err := parseFrequencyThreshold(params)
	if err != nil || p.EventName == "" {
		return nil, false
	}
	return []string{p.EventName}, true
}

func (f *FrequencyThreshold) Evaluate(ctx context.Context, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error) {
	p, err := parseFrequencyThreshold(params)
	if err != nil {
		return false, err
	}
	window, err := frequencyWindow(p)
	if err != nil {
		return false, err
	}
	if p.EventName != "" && ev.EventName() != p.EventName {
		return false, nil
	}

	now := f.clock()
	ruleID := RuleID(ctx)

	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.state(ruleID)
	if !s.seeded {
		if !f.seed(ctx, s, ruleID, now, window) {
			// The durable state could not be read, so firing now could
			// duplicate an alert the rule already sent. Skip this event and
			// retry the rebuild on the next one.
			return false, nil
		}
	}

	// Old matches leave the window as new ones arrive, so a slow trickle
	// never accumulates into a false positive.
	s.matches = pruneBefore(s.matches, now.Add(-window))
	s.matches = append(s.matches, now)

	// Re-arm only once the window that produced the last alert has fully
	// elapsed: a sustained condition alerts at most once per window, and a
	// restart cannot fire again inside the episode it already alerted for. The
	// comparison is strict so the episode's own window start has aged out by
	// the time the next episode begins — otherwise the next fire would reuse
	// the previous synthetic id and be dropped as a duplicate.
	if s.armed && now.After(s.episode.Add(window)) {
		s.armed = false
	}

	if !s.armed && len(s.matches) >= p.Count {
		s.armed = true
		s.episode = s.matches[0]
		return true, nil
	}
	return false, nil
}

// AlertEventID implements AlertEventIDer: while an episode is active it returns
// the episode's stable synthetic id (the window start), so a replay of the
// crossing is deduplicated by the store rather than alerting twice.
func (f *FrequencyThreshold) AlertEventID(ctx context.Context, _ *stellar.DecodedEvent, _ json.RawMessage) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.states[RuleID(ctx)]
	if s == nil || !s.armed || s.episode.IsZero() {
		return ""
	}
	return frequencyEventPrefix + s.episode.UTC().Format(time.RFC3339Nano)
}

// seed rebuilds a rule's window from the MatchLog on the first event after a
// restart. It reports false when the durable state could not be read, leaving
// the state unseeded so the next event tries again rather than silently
// starting from an empty count (and firing an alert the rule already sent).
func (f *FrequencyThreshold) seed(ctx context.Context, s *freqState, ruleID int64, now time.Time, window time.Duration) bool {
	if f.matchLog == nil || ruleID == 0 {
		s.seeded = true
		return true
	}
	records, err := f.matchLog.RecentMatches(ctx, ruleID, now.Add(-window))
	if err != nil {
		return false
	}
	s.seeded = true
	episodeAt := time.Time{}
	for _, r := range records {
		s.matches = append(s.matches, r.At)
		if !r.At.Before(now.Add(-window)) && r.At.After(episodeAt) {
			// The newest stored alert inside the window is this rule's most
			// recent episode; recover its start so re-arm timing is right.
			episodeAt = r.At
		}
	}
	sort.Slice(s.matches, func(i, j int) bool { return s.matches[i].Before(s.matches[j]) })
	if episodeAt.IsZero() {
		return true
	}
	s.armed = true
	for _, r := range records {
		if r.At.Equal(episodeAt) {
			if start, ok := decodeEpisode(r.EventID); ok {
				episodeAt = start
			}
			break
		}
	}
	s.episode = episodeAt
	return true
}

func parseFrequencyThreshold(params json.RawMessage) (frequencyThresholdParams, error) {
	var p frequencyThresholdParams
	if err := json.Unmarshal(params, &p); err != nil {
		return p, fmt.Errorf("frequency_threshold: invalid params: %w", err)
	}
	return p, nil
}

// frequencyWindow parses and validates the window at evaluation time, so a
// rule that somehow reached storage malformed reports an error instead of
// comparing against a zero duration.
func frequencyWindow(p frequencyThresholdParams) (time.Duration, error) {
	if p.Count <= 0 {
		return 0, fmt.Errorf("frequency_threshold: count must be positive")
	}
	d, err := time.ParseDuration(p.Window)
	if err != nil {
		return 0, fmt.Errorf("frequency_threshold: invalid window %q", p.Window)
	}
	if d <= 0 {
		return 0, fmt.Errorf("frequency_threshold: window must be positive")
	}
	return d, nil
}

// decodeEpisode recovers the window start encoded in a synthetic alert id.
func decodeEpisode(id string) (time.Time, bool) {
	s, ok := strings.CutPrefix(id, frequencyEventPrefix)
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// pruneBefore drops the leading match times older than cutoff. matches is kept
// sorted (a new time is always appended and is the newest), so a single scan
// suffices.
func pruneBefore(matches []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(matches) && matches[i].Before(cutoff) {
		i++
	}
	if i == 0 {
		return matches
	}
	return matches[i:]
}
