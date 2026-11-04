package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

const (
	// TypeBaselineDeviation is the rule type name registered for baseline
	// anomaly detection.
	TypeBaselineDeviation = "baseline_deviation"

	// DefaultBaselineMinSamples is the default minimum number of historical
	// observations required in the active bucket before a rule may fire.
	DefaultBaselineMinSamples = 5

	// MaxBaselineSamplesPerBucket bounds the in-memory sliding window per
	// seasonality bucket so memory and per-event evaluation cost remain O(1).
	MaxBaselineSamplesPerBucket = 256

	// madToSigma scales Median Absolute Deviation (MAD) to the equivalent
	// normal standard deviation (1 / Phi^-1(0.75) ≈ 1.4826).
	madToSigma = 1.4826

	// minRelativeSpreadFloor prevents microscopic floating-point jitter around
	// a nearly constant baseline from producing huge synthetic deviation ratios.
	minRelativeSpreadFloor = 0.01
)

type baselineDirection string

const (
	directionBoth  baselineDirection = "both"
	directionAbove baselineDirection = "above"
	directionBelow baselineDirection = "below"
)

type baselineSeasonality string

const (
	seasonalityNone       baselineSeasonality = "none"
	seasonalityHourOfDay  baselineSeasonality = "hour_of_day"
	seasonalityDayOfWeek  baselineSeasonality = "day_of_week"
)

// baselineDeviationParams configure the baseline_deviation rule.
//
//	{
//	  "event_name": "transfer",        // optional: only consider events with this name
//	  "value_path": "amount",          // optional: dot path into the event value
//	  "window": "24h",                 // required: rolling baseline window duration
//	  "warmup": "10m",                 // optional: initial learning period with no alerts
//	  "min_samples": 5,                // optional: minimum observations before alerting (default 5)
//	  "sensitivity": 3.0,              // required: deviation multiplier threshold (> 0)
//	  "direction": "both",             // optional: "both" (default) | "above" | "below"
//	  "seasonality": "none"            // optional: "none" (default) | "hour_of_day" | "day_of_week"
//	}
type baselineDeviationParams struct {
	EventName   string  `json:"event_name"`
	ValuePath   string  `json:"value_path"`
	Window      string  `json:"window"`
	Warmup      string  `json:"warmup"`
	MinSamples  int     `json:"min_samples"`
	Sensitivity float64 `json:"sensitivity"`
	Direction   string  `json:"direction"`
	Seasonality string  `json:"seasonality"`
}

type baselineSuppressedCtxKey struct{}

// WithBaselineSuppressed marks ctx as occurring inside an active maintenance
// window, silence, or inhibition period. When set, BaselineDeviation neither
// fires nor folds the event into the learned baseline, preventing outages or
// maintenance traffic from teaching the baseline that abnormal behaviour is normal.
func WithBaselineSuppressed(ctx context.Context, suppressed bool) context.Context {
	return context.WithValue(ctx, baselineSuppressedCtxKey{}, suppressed)
}

// IsBaselineSuppressed reports whether ctx is marked as suppressed or inside a
// maintenance window for baseline learning.
func IsBaselineSuppressed(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(baselineSuppressedCtxKey{}).(bool)
	return v
}

// BaselineDecision captures the explainable statistics of a single
// baseline_deviation evaluation so operators and tests can inspect why a rule
// did or did not fire.
type BaselineDecision struct {
	Matched           bool          `json:"matched"`
	WarmupActive      bool          `json:"warmup_active"`
	WarmupRemaining   time.Duration `json:"warmup_remaining"`
	Suppressed        bool          `json:"suppressed"`
	SampleCount       int           `json:"sample_count"`
	MinSamples        int           `json:"min_samples"`
	Baseline          float64       `json:"baseline"`
	Mean              float64       `json:"mean"`
	Spread            float64       `json:"spread"`
	Observed          float64       `json:"observed"`
	Deviation         float64       `json:"deviation"`
	Sensitivity       float64       `json:"sensitivity"`
	Direction         string        `json:"direction"`
	SeasonalityBucket string        `json:"seasonality_bucket"`
	Reason            string        `json:"reason"`
}

type baselineObservation struct {
	at    time.Time
	value float64
}

// baselineBucketState maintains a bounded sliding window of observations plus
// incremental Welford running statistics for one seasonality bucket.
type baselineBucketState struct {
	samples []baselineObservation
	count   int
	mean    float64
	m2      float64
}

func (b *baselineBucketState) clone() *baselineBucketState {
	if b == nil {
		return nil
	}
	cp := *b
	if len(b.samples) > 0 {
		cp.samples = make([]baselineObservation, len(b.samples))
		copy(cp.samples, b.samples)
	}
	return &cp
}

func (b *baselineBucketState) pruneExpired(cutoff time.Time) {
	if len(b.samples) == 0 {
		return
	}
	drop := 0
	for drop < len(b.samples) && b.samples[drop].at.Before(cutoff) {
		drop++
	}
	if drop == 0 {
		return
	}
	if drop >= len(b.samples) {
		b.samples = nil
		b.count = 0
		b.mean = 0
		b.m2 = 0
		return
	}
	b.samples = append([]baselineObservation(nil), b.samples[drop:]...)
	b.recomputeWelford()
}

func (b *baselineBucketState) add(at time.Time, val float64) {
	if len(b.samples) >= MaxBaselineSamplesPerBucket {
		b.samples = append(b.samples[1:], baselineObservation{at: at, value: val})
		b.recomputeWelford()
		return
	}
	b.samples = append(b.samples, baselineObservation{at: at, value: val})
	b.count++
	delta := val - b.mean
	b.mean += delta / float64(b.count)
	delta2 := val - b.mean
	b.m2 += delta * delta2
}

func (b *baselineBucketState) recomputeWelford() {
	b.count = 0
	b.mean = 0
	b.m2 = 0
	for _, s := range b.samples {
		b.count++
		delta := s.value - b.mean
		b.mean += delta / float64(b.count)
		delta2 := s.value - b.mean
		b.m2 += delta * delta2
	}
}

// stats computes the spike-resistant median and MAD-derived spread over the
// bucket's active window, falling back to a relative scale floor when history
// is constant so a single spike or zero-variance series never produces NaN or
// false positives.
func (b *baselineBucketState) stats() (median, mean, spread float64, count int) {
	count = len(b.samples)
	if count == 0 {
		return 0, 0, 0, 0
	}
	vals := make([]float64, count)
	for i, s := range b.samples {
		vals[i] = s.value
	}
	sort.Float64s(vals)
	median = percentile50(vals)

	absDevs := make([]float64, count)
	for i, v := range vals {
		absDevs[i] = math.Abs(v - median)
	}
	sort.Float64s(absDevs)
	mad := percentile50(absDevs)
	robustSigma := mad * madToSigma

	floor := minRelativeSpreadFloor * math.Max(math.Abs(median), 1.0)
	switch {
	case robustSigma == 0:
		// Constant history (all samples identical, or >50% identical with a
		// single outlier spike): measure deviation relative to the baseline
		// magnitude itself (or 1.0 when baseline is 0) so a sensitivity of
		// 3.0 means a 3x change from the constant level.
		spread = math.Max(math.Abs(median), 1.0)
	case robustSigma < floor:
		spread = floor
	default:
		spread = robustSigma
	}
	return median, b.mean, spread, count
}

func percentile50(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	mid := n / 2
	if n%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2.0
}

type baselineRuleState struct {
	startedAt    time.Time
	lastSeenAt   time.Time
	version      int
	paramsKey    string
	buckets      map[string]*baselineBucketState
	lastDecision BaselineDecision
	hasDecision  bool
}

func (s *baselineRuleState) clone() *baselineRuleState {
	if s == nil {
		return nil
	}
	cp := *s
	if len(s.buckets) > 0 {
		cp.buckets = make(map[string]*baselineBucketState, len(s.buckets))
		for k, b := range s.buckets {
			cp.buckets[k] = b.clone()
		}
	}
	return &cp
}

// BaselineDeviation learns a rolling, spike-resistant baseline per rule (and
// optional seasonality bucket) and fires when an event's numeric value
// deviates from that baseline by at least sensitivity * spread.
type BaselineDeviation struct {
	mu               sync.Mutex
	states           map[int64]*baselineRuleState
	now              func() time.Time
	suppressionCheck func(ctx context.Context, ev *stellar.DecodedEvent) bool
}

// NewBaselineDeviation constructs a stateful baseline_deviation evaluator.
func NewBaselineDeviation() *BaselineDeviation {
	return &BaselineDeviation{
		states: make(map[int64]*baselineRuleState),
		now:    time.Now,
	}
}

// WithClock overrides the clock used for rolling windows, warm-up tracking,
// and seasonality bucketing (for tests).
func (b *BaselineDeviation) WithClock(now func() time.Time) *BaselineDeviation {
	if now != nil {
		b.now = now
	}
	return b
}

// WithSuppressionCheck attaches a predicate that reports whether an arriving
// event falls inside an active maintenance window or suppression rule. Events
// matching the predicate are excluded from baseline learning and do not fire.
func (b *BaselineDeviation) WithSuppressionCheck(fn func(ctx context.Context, ev *stellar.DecodedEvent) bool) *BaselineDeviation {
	b.suppressionCheck = fn
	return b
}

func (b *BaselineDeviation) clock() time.Time {
	if b.now != nil {
		return b.now().UTC()
	}
	return time.Now().UTC()
}

// ResetRule clears cached baseline state for ruleID when a rule is updated,
// disabled, or deleted.
func (b *BaselineDeviation) ResetRule(ruleID int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.states, ruleID)
}

// SnapshotRule returns a deep copy of ruleID's state so isolated evaluation
// can roll back partial mutations if evaluation is aborted.
func (b *BaselineDeviation) SnapshotRule(ruleID int64) any {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.states == nil {
		return (*baselineRuleState)(nil)
	}
	return b.states[ruleID].clone()
}

// RestoreRule atomically restores ruleID's state from a snapshot created by
// SnapshotRule.
func (b *BaselineDeviation) RestoreRule(ruleID int64, snapshot any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.states == nil {
		b.states = make(map[int64]*baselineRuleState)
	}
	snap, ok := snapshot.(*baselineRuleState)
	if !ok || snap == nil {
		delete(b.states, ruleID)
		return
	}
	b.states[ruleID] = snap.clone()
}

// LastDecision returns the most recent explainable evaluation decision for
// ruleID.
func (b *BaselineDeviation) LastDecision(ruleID int64) (BaselineDecision, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st, ok := b.states[ruleID]
	if !ok || st == nil || !st.hasDecision {
		return BaselineDecision{}, false
	}
	return st.lastDecision, true
}

// ParamSchema describes baseline_deviation parameters for the dashboard rule
// builder and API schema endpoint.
func (*BaselineDeviation) ParamSchema() []FieldSchema {
	return []FieldSchema{
		{Name: "event_name", Type: "string", Description: "Only consider events with this name (first topic)"},
		{Name: "value_path", Type: "string", Description: "Dot path into the event value (omit when the event value itself is numeric)"},
		{Name: "window", Type: "string", Required: true, Description: "Rolling baseline history window (Go duration, e.g. 1h, 24h)"},
		{Name: "warmup", Type: "string", Description: "Warm-up duration before alerting is enabled (Go duration, e.g. 10m)", Default: "10m"},
		{Name: "min_samples", Type: "number", Description: "Minimum baseline observations before alerting (default 5)", Default: "5"},
		{Name: "sensitivity", Type: "number", Required: true, Description: "Deviation multiplier threshold (e.g. 3 fires at >= 3x baseline spread)"},
		{Name: "direction", Type: "select", Description: "Anomaly direction to alert on", Options: []string{string(directionBoth), string(directionAbove), string(directionBelow)}, Default: string(directionBoth)},
		{Name: "seasonality", Type: "select", Description: "Partition baseline by time bucket to account for daily/weekly cycles", Options: []string{string(seasonalityNone), string(seasonalityHourOfDay), string(seasonalityDayOfWeek)}, Default: string(seasonalityNone)},
		{Name: CooldownParam, Type: "string", Description: "Suppress repeat alerts for this duration"},
	}
}

// EventNames narrows server-side event filtering when event_name is set.
func (*BaselineDeviation) EventNames(params json.RawMessage) ([]string, bool) {
	p, err := parseBaselineDeviationParams(params)
	if err != nil || p.EventName == "" {
		return nil, false
	}
	return []string{p.EventName}, true
}

// Validate checks baseline_deviation parameters at rule create/update time.
func (*BaselineDeviation) Validate(params json.RawMessage) error {
	p, err := parseBaselineDeviationParams(params)
	if err != nil {
		return err
	}
	var details FieldErrors
	switch d, err := time.ParseDuration(p.Window); {
	case strings.TrimSpace(p.Window) == "":
		details = append(details, FieldError{
			Field:  "window",
			Reason: `baseline_deviation: window is required (a Go duration such as "1h")`,
		})
	case err != nil:
		details = append(details, FieldError{
			Field:  "window",
			Reason: fmt.Sprintf(`baseline_deviation: invalid window %q (want a Go duration such as "1h")`, p.Window),
		})
	case d <= 0:
		details = append(details, FieldError{
			Field:  "window",
			Reason: "baseline_deviation: window must be positive",
		})
	}

	if strings.TrimSpace(p.Warmup) != "" {
		if d, err := time.ParseDuration(p.Warmup); err != nil {
			details = append(details, FieldError{
				Field:  "warmup",
				Reason: fmt.Sprintf(`baseline_deviation: invalid warmup %q (want a Go duration such as "10m")`, p.Warmup),
			})
		} else if d < 0 {
			details = append(details, FieldError{
				Field:  "warmup",
				Reason: "baseline_deviation: warmup must not be negative",
			})
		}
	}

	if p.MinSamples < 0 {
		details = append(details, FieldError{
			Field:  "min_samples",
			Reason: "baseline_deviation: min_samples must not be negative",
		})
	}

	if math.IsNaN(p.Sensitivity) || math.IsInf(p.Sensitivity, 0) || p.Sensitivity <= 0 {
		details = append(details, FieldError{
			Field:  "sensitivity",
			Reason: "baseline_deviation: sensitivity must be a positive number",
		})
	}

	if p.Direction != "" {
		switch baselineDirection(p.Direction) {
		case directionBoth, directionAbove, directionBelow:
		default:
			details = append(details, FieldError{
				Field:  "direction",
				Reason: fmt.Sprintf("baseline_deviation: invalid direction %q (want both|above|below)", p.Direction),
			})
		}
	}

	if p.Seasonality != "" {
		switch baselineSeasonality(p.Seasonality) {
		case seasonalityNone, seasonalityHourOfDay, seasonalityDayOfWeek:
		default:
			details = append(details, FieldError{
				Field:  "seasonality",
				Reason: fmt.Sprintf("baseline_deviation: invalid seasonality %q (want none|hour_of_day|day_of_week)", p.Seasonality),
			})
		}
	}

	if len(details) > 0 {
		return details
	}
	return nil
}

// Evaluate updates the rule's rolling baseline (unless suppressed or firing an
// anomaly) and reports whether ev deviates beyond the configured sensitivity.
func (b *BaselineDeviation) Evaluate(ctx context.Context, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := b.Validate(params); err != nil {
		return false, err
	}
	p, err := parseBaselineDeviationParams(params)
	if err != nil {
		return false, err
	}
	if p.EventName != "" && ev.EventName() != p.EventName {
		return false, nil
	}

	observed, ok := extractBaselineValue(ev, p.ValuePath)
	if !ok {
		return false, nil
	}

	now := b.clock()
	ruleID := RuleID(ctx)
	ruleVersion := RuleVersion(ctx)
	suppressed := IsBaselineSuppressed(ctx) || (b.suppressionCheck != nil && b.suppressionCheck(ctx, ev))

	b.mu.Lock()
	defer b.mu.Unlock()

	decision := b.evaluateLocked(ruleID, ruleVersion, p, now, observed, suppressed, true)
	return decision.Matched, nil
}

// Explain evaluates ev against the rule's current baseline without mutating
// the learned state and returns a structured, operator-readable explanation.
func (b *BaselineDeviation) Explain(ctx context.Context, ev *stellar.DecodedEvent, params json.RawMessage) (*RuleExplanation, error) {
	if err := b.Validate(params); err != nil {
		return &RuleExplanation{Error: err.Error()}, err
	}
	p, err := parseBaselineDeviationParams(params)
	if err != nil {
		return &RuleExplanation{Error: err.Error()}, err
	}

	var conditions []ConditionExplanation
	if p.EventName != "" {
		actual := ev.EventName()
		passed := actual == p.EventName
		conditions = append(conditions, ConditionExplanation{
			Name:     "event_name",
			Expected: p.EventName,
			Actual:   actual,
			Passed:   passed,
			Reason:   fmt.Sprintf("event name %q %s expected %q", actual, ternary(passed, "matches", "does not match"), p.EventName),
		})
		if !passed {
			return &RuleExplanation{Matched: false, Conditions: conditions}, nil
		}
	}

	observed, ok := extractBaselineValue(ev, p.ValuePath)
	if !ok {
		conditions = append(conditions, ConditionExplanation{
			Name:     "value_path",
			Expected: "a finite numeric value",
			Actual:   nil,
			Passed:   false,
			Reason:   fmt.Sprintf("value path %q not found or not numeric in event", p.ValuePath),
		})
		return &RuleExplanation{Matched: false, Conditions: conditions}, nil
	}

	now := b.clock()
	ruleID := RuleID(ctx)
	ruleVersion := RuleVersion(ctx)
	suppressed := IsBaselineSuppressed(ctx) || (b.suppressionCheck != nil && b.suppressionCheck(ctx, ev))

	b.mu.Lock()
	decision := b.evaluateLocked(ruleID, ruleVersion, p, now, observed, suppressed, false)
	b.mu.Unlock()

	if decision.Suppressed {
		conditions = append(conditions, ConditionExplanation{
			Name:     "maintenance_window",
			Expected: "not suppressed",
			Actual:   "suppressed",
			Passed:   false,
			Reason:   decision.Reason,
		})
		return &RuleExplanation{Matched: false, Conditions: conditions}, nil
	}

	warmupPassed := !decision.WarmupActive
	conditions = append(conditions, ConditionExplanation{
		Name:     "warmup",
		Expected: fmt.Sprintf("at least %d samples and warm-up elapsed", decision.MinSamples),
		Actual:   fmt.Sprintf("%d samples (warmup_remaining=%s)", decision.SampleCount, decision.WarmupRemaining),
		Passed:   warmupPassed,
		Reason:   ternary(warmupPassed, fmt.Sprintf("baseline ready with %d samples in bucket %q", decision.SampleCount, decision.SeasonalityBucket), decision.Reason),
	})

	conditions = append(conditions, ConditionExplanation{
		Name:     "baseline_deviation",
		Expected: fmt.Sprintf("deviation >= %.2fx (%s) from baseline %.4g (spread %.4g)", decision.Sensitivity, decision.Direction, decision.Baseline, decision.Spread),
		Actual:   fmt.Sprintf("observed %.4g (deviation %.2fx)", decision.Observed, decision.Deviation),
		Passed:   decision.Matched,
		Reason:   decision.Reason,
	})

	return &RuleExplanation{
		Matched:    decision.Matched,
		Conditions: conditions,
	}, nil
}

func (b *BaselineDeviation) evaluateLocked(
	ruleID int64,
	ruleVersion int,
	p baselineDeviationParams,
	now time.Time,
	observed float64,
	suppressed bool,
	mutate bool,
) BaselineDecision {
	window, _ := time.ParseDuration(p.Window)
	var warmup time.Duration
	if strings.TrimSpace(p.Warmup) != "" {
		warmup, _ = time.ParseDuration(p.Warmup)
	}
	minSamples := p.MinSamples
	if minSamples <= 0 {
		minSamples = DefaultBaselineMinSamples
	}
	dir := baselineDirection(p.Direction)
	if dir == "" {
		dir = directionBoth
	}
	seasonality := baselineSeasonality(p.Seasonality)
	if seasonality == "" {
		seasonality = seasonalityNone
	}
	bucketKey := seasonalityBucketKey(seasonality, now)
	pKey := canonicalBaselineParamsKey(p)

	st := b.stateLocked(ruleID, mutate)
	if st.startedAt.IsZero() || (ruleVersion > 0 && st.version > 0 && st.version != ruleVersion) || (st.paramsKey != "" && st.paramsKey != pKey) {
		if mutate {
			st.startedAt = now
			st.version = ruleVersion
			st.paramsKey = pKey
			st.buckets = make(map[string]*baselineBucketState)
		} else {
			st = &baselineRuleState{
				startedAt: now,
				version:   ruleVersion,
				paramsKey: pKey,
				buckets:   make(map[string]*baselineBucketState),
			}
		}
	} else if mutate {
		st.version = ruleVersion
		st.paramsKey = pKey
	}

	if suppressed {
		decision := BaselineDecision{
			Matched:           false,
			Suppressed:        true,
			MinSamples:        minSamples,
			Observed:          observed,
			Sensitivity:       p.Sensitivity,
			Direction:         string(dir),
			SeasonalityBucket: bucketKey,
			Reason:            "event occurred during an active maintenance or suppression window; excluded from baseline learning and alerting",
		}
		if mutate {
			st.lastDecision = decision
			st.hasDecision = true
		}
		return decision
	}

	bucket := st.buckets[bucketKey]
	if bucket == nil {
		bucket = &baselineBucketState{}
		if mutate {
			st.buckets[bucketKey] = bucket
		}
	} else if !mutate {
		bucket = bucket.clone()
	}

	bucket.pruneExpired(now.Add(-window))
	median, mean, spread, count := bucket.stats()

	var warmupRemaining time.Duration
	if warmup > 0 {
		elapsed := now.Sub(st.startedAt)
		if elapsed < warmup {
			warmupRemaining = warmup - elapsed
		}
	}

	if warmupRemaining > 0 || count < minSamples {
		if mutate {
			bucket.add(now, observed)
			st.lastSeenAt = now
		}
		newCount := count
		if mutate {
			newCount = len(bucket.samples)
		}
		reason := fmt.Sprintf("warm-up active: %d/%d samples collected in bucket %q", newCount, minSamples, bucketKey)
		if warmupRemaining > 0 {
			reason = fmt.Sprintf("warm-up active: %s remaining (%d/%d samples in bucket %q)", warmupRemaining, newCount, minSamples, bucketKey)
		}
		decision := BaselineDecision{
			Matched:           false,
			WarmupActive:      true,
			WarmupRemaining:   warmupRemaining,
			SampleCount:       newCount,
			MinSamples:        minSamples,
			Baseline:          median,
			Mean:              mean,
			Spread:            spread,
			Observed:          observed,
			Sensitivity:       p.Sensitivity,
			Direction:         string(dir),
			SeasonalityBucket: bucketKey,
			Reason:            reason,
		}
		if mutate {
			st.lastDecision = decision
			st.hasDecision = true
		}
		return decision
	}

	signedDiff := observed - median
	absDiff := math.Abs(signedDiff)
	deviation := absDiff / spread

	directionOK := false
	switch dir {
	case directionAbove:
		directionOK = signedDiff > 0
	case directionBelow:
		directionOK = signedDiff < 0
	default:
		directionOK = true
	}

	matched := directionOK && deviation >= p.Sensitivity
	var reason string
	if matched {
		reason = fmt.Sprintf("observed %.4g deviates by %.2fx from baseline %.4g (spread %.4g, threshold %.2fx, direction %s, samples %d, bucket %q)",
			observed, deviation, median, spread, p.Sensitivity, dir, count, bucketKey)
	} else {
		// Fold normal (non-anomalous) observations into the rolling baseline so
		// gradual organic drift is tracked while anomalous spikes do not poison
		// the baseline.
		if mutate {
			bucket.add(now, observed)
			st.lastSeenAt = now
		}
		reason = fmt.Sprintf("observed %.4g is within %.2fx of baseline %.4g (deviation %.2fx < threshold %.2fx, spread %.4g, samples %d, bucket %q)",
			observed, p.Sensitivity, median, deviation, p.Sensitivity, spread, count, bucketKey)
	}

	decision := BaselineDecision{
		Matched:           matched,
		WarmupActive:      false,
		WarmupRemaining:   0,
		SampleCount:       count,
		MinSamples:        minSamples,
		Baseline:          median,
		Mean:              mean,
		Spread:            spread,
		Observed:          observed,
		Deviation:         deviation,
		Sensitivity:       p.Sensitivity,
		Direction:         string(dir),
		SeasonalityBucket: bucketKey,
		Reason:            reason,
	}
	if mutate {
		st.lastDecision = decision
		st.hasDecision = true
	}
	return decision
}

func (b *BaselineDeviation) stateLocked(ruleID int64, create bool) *baselineRuleState {
	if b.states == nil {
		b.states = make(map[int64]*baselineRuleState)
	}
	st := b.states[ruleID]
	if st != nil {
		return st
	}
	if !create {
		return &baselineRuleState{buckets: make(map[string]*baselineBucketState)}
	}
	if len(b.states) >= MaxTrackedStatefulRules {
		b.evictOldestRuleLocked()
	}
	st = &baselineRuleState{
		buckets: make(map[string]*baselineBucketState),
	}
	b.states[ruleID] = st
	return st
}

func (b *BaselineDeviation) evictOldestRuleLocked() {
	var (
		victimID   int64
		victimTime time.Time
		found      bool
	)
	for id, st := range b.states {
		if st == nil || st.lastSeenAt.IsZero() {
			delete(b.states, id)
			return
		}
		if !found || st.lastSeenAt.Before(victimTime) {
			victimID = id
			victimTime = st.lastSeenAt
			found = true
		}
	}
	if found {
		delete(b.states, victimID)
	}
}

func seasonalityBucketKey(s baselineSeasonality, at time.Time) string {
	u := at.UTC()
	switch s {
	case seasonalityHourOfDay:
		return fmt.Sprintf("hour:%02d", u.Hour())
	case seasonalityDayOfWeek:
		return fmt.Sprintf("weekday:%s", strings.ToLower(u.Weekday().String()[:3]))
	default:
		return "all"
	}
}

func canonicalBaselineParamsKey(p baselineDeviationParams) string {
	return fmt.Sprintf("%s|%s|%s|%s|%d|%.6g|%s|%s",
		p.EventName, p.ValuePath, p.Window, p.Warmup, p.MinSamples, p.Sensitivity, p.Direction, p.Seasonality)
}

func parseBaselineDeviationParams(params json.RawMessage) (baselineDeviationParams, error) {
	var p baselineDeviationParams
	if err := json.Unmarshal(params, &p); err != nil {
		return p, fmt.Errorf("baseline_deviation: invalid params: %w", err)
	}
	return p, nil
}

func extractBaselineValue(ev *stellar.DecodedEvent, valuePath string) (float64, bool) {
	if ev == nil {
		return 0, false
	}
	var raw any
	var found bool
	if ev.Fields != nil && valuePath != "" {
		raw, found = stellar.Lookup(ev.Fields, valuePath)
	}
	if !found {
		raw, found = stellar.Lookup(ev.Value, valuePath)
	}
	if !found {
		return 0, false
	}
	bf, ok := stellar.ToBigFloat(raw)
	if !ok || bf == nil {
		return 0, false
	}
	f, _ := bf.Float64()
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}
