// Package store defines SoroBeacon's persistence interfaces and models.
// The Postgres implementation lives in postgres.go; tests and alternative
// backends can implement the narrow per-domain interfaces below.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// Priority ranks a monitor's contracts in the poller's schedule. It is a
// small closed set rather than a free integer so the scheduler, the API
// validation and the database CHECK constraint all agree on the vocabulary.
type Priority string

const (
	// PriorityLow is the last tier served in a poll cycle: high-volume,
	// best-effort monitors whose latency budget is measured in minutes.
	PriorityLow Priority = "low"
	// PriorityNormal is the default, so every monitor that existed before
	// priorities did keeps exactly today's behaviour.
	PriorityNormal Priority = "normal"
	// PriorityHigh is served first within a cycle: contracts where a
	// five-minute alert delay has real cost.
	PriorityHigh Priority = "high"
)

// ParsePriority validates a priority string. The empty string maps to
// PriorityNormal so a caller that never sets one gets today's behaviour,
// and any other unknown value is rejected rather than silently downgraded.
func ParsePriority(s string) (Priority, bool) {
	switch Priority(s) {
	case "":
		return PriorityNormal, true
	case PriorityLow, PriorityNormal, PriorityHigh:
		return Priority(s), true
	default:
		return "", false
	}
}

// Normalized returns p with the empty value mapped to PriorityNormal, so a
// write path that never sets one stores the middle tier rather than an empty
// string the database CHECK constraint would reject.
func (p Priority) Normalized() Priority {
	if p == "" {
		return PriorityNormal
	}
	return p
}

// Rank orders the tiers for scheduling: higher ranks are served sooner. The
// zero value of an unset Priority is normal, matching ParsePriority.
func (p Priority) Rank() int {
	switch p {
	case PriorityHigh:
		return 2
	case PriorityLow:
		return 0
	default:
		return 1
	}
}

// Severity is the alert severity level. It is a small closed set with a
// fixed order so the API, store, and dispatcher all agree on the vocabulary
// and ordering.
type Severity string

const (
	// SeverityInfo is informational: routine matches that do not require
	// immediate attention.
	SeverityInfo Severity = "info"
	// SeverityWarning is the default: notable events that should be seen
	// but do not warrant paging.
	SeverityWarning Severity = "warning"
	// SeverityCritical is for high-impact events that require immediate
	// response (e.g., treasury movements, contract upgrades).
	SeverityCritical Severity = "critical"
)

// ParseSeverity validates a severity string. The empty string maps to
// SeverityWarning so a caller that never sets one gets today's behaviour,
// and any other unknown value is rejected rather than silently downgraded.
func ParseSeverity(s string) (Severity, bool) {
	switch Severity(s) {
	case "":
		return SeverityWarning, true
	case SeverityInfo, SeverityWarning, SeverityCritical:
		return Severity(s), true
	default:
		return "", false
	}
}

// Rank orders the severities for routing: higher ranks are more severe.
func (s Severity) Rank() int {
	switch s {
	case SeverityCritical:
		return 2
	case SeverityInfo:
		return 0
	default:
		return 1
	}
}

// MeetsThreshold reports whether this severity meets or exceeds the given
// minimum severity. An empty minimum means no filter (always true).
func (s Severity) MeetsThreshold(min Severity) bool {
	if min == "" {
		return true
	}
	return s.Rank() >= min.Rank()
}

// ErrChannelInUse is returned when a channel cannot be deleted because an
// escalation policy step still references it. The API maps it to 409 so the
// operator can detach it first rather than be left with a dangling reference.
var ErrChannelInUse = errors.New("channel is referenced by an escalation policy")

// Monitor watches one or more Soroban contracts.
type Monitor struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	ContractIDs []string  `json:"contract_ids"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	// Priority is how soon this monitor's contracts are polled within a
	// cycle. Empty means PriorityNormal, so JSON written before the field
	// existed (and rows migrated from it) stays in the middle tier.
	Priority Priority `json:"priority"`
	// LastMatchedAt is the ledger close time of the most recent event that
	// created an alert for this monitor. Nil means it has never matched —
	// do not backfill a fake timestamp.
	LastMatchedAt *time.Time `json:"last_matched_at"`
	// ChannelIDs are the notification channels this monitor alerts to.
	ChannelIDs []int64 `json:"channel_ids"`
	// Network is the Stellar network this monitor watches: contract ids are
	// only meaningful on one chain, so a monitor belongs to exactly one. The
	// empty string is the pre-multi-network value and is assigned the
	// instance's primary network at startup; a newly created monitor always
	// carries the network it was created for.
	Network string `json:"network"`
}

// Rule is one condition evaluated against every event of its monitor's
// contracts. Type selects a rules.RuleEvaluator; Params are its arguments.
type Rule struct {
	ID        int64           `json:"id"`
	MonitorID int64           `json:"monitor_id"`
	Type      string          `json:"type"`
	Params    json.RawMessage `json:"params"`
	Enabled   bool            `json:"enabled"`
	// Severity is the alert severity this rule produces. Empty means
	// SeverityWarning, so rules created before the field existed keep
	// today's behaviour. It is validated at the API boundary.
	Severity Severity `json:"severity"`
}

// DefaultChannelTimeout is the timeout applied to channels when unset (15s),
// matching the previous package-level HTTP client timeout.
const DefaultChannelTimeout = 15 * time.Second

// MinChannelTimeout bounds how short a channel timeout can be: anything less
// is practically guaranteed to fail spuriously over real networks.
const MinChannelTimeout = 1 * time.Second

// MaxChannelTimeout is the ceiling on a channel's HTTP timeout: an operator
// setting a ten-minute timeout will wedge delivery workers.
const MaxChannelTimeout = 60 * time.Second

// Channel is a configured notification destination. Config holds
// channel-specific settings including secrets (webhook URLs, bot tokens,
// SMTP credentials) — never log it and never return it from the API.
// When a ConfigCipher is configured, Config is encrypted at rest and the
// store returns it decrypted (see crypto.go).
type Channel struct {
	ID     int64           `json:"id"`
	Name   string          `json:"name"`
	Type   string          `json:"type"`
	Config json.RawMessage `json:"-"`
	// Enabled controls whether the channel receives alerts at all.
	Enabled bool `json:"enabled"`
	// DigestMode selects batched delivery: "" (the default) sends every
	// alert immediately, exactly as before digesting existed; "window"
	// accumulates alerts and sends one summary per DigestWindowSeconds.
	DigestMode string `json:"digest_mode"`
	// DigestWindowSeconds is the accumulation window when DigestMode is
	// "window". Zero leaves digesting off even when a mode is set, so a
	// half-filled form cannot silently batch forever.
	DigestWindowSeconds int64 `json:"digest_window_seconds"`
	// MinSeverity is the minimum alert severity this channel will receive.
	// Empty means no filter (receive all severities), so channels created
	// before the field existed keep today's behaviour. It is validated at
	// the API boundary.
	MinSeverity Severity      `json:"min_severity"`
	CreatedAt   time.Time     `json:"created_at"`
	Timeout     time.Duration `json:"timeout"`

	// The fields below are delivery health, derived from outcomes rather than
	// configured (see RecordChannelHealth and migration 0025). They answer
	// "is this channel still working?", which delivery_attempts could only
	// answer one alert at a time.
	//
	// ConsecutiveFailures counts failed deliveries since the last success.
	ConsecutiveFailures int64 `json:"consecutive_failures"`
	// ConsecutivePermanentFailures counts permanent failures (401/403/404)
	// since the last success, and is what auto-disable triggers on. A
	// transient failure neither increments nor clears it, so a revoked
	// credential is not hidden by an unrelated 5xx in the middle.
	ConsecutivePermanentFailures int64 `json:"consecutive_permanent_failures"`
	// LastError is the most recent failure message. It never holds channel
	// config: the notifiers redact URLs and tokens before building it.
	LastError string `json:"last_error,omitempty"`
	// LastErrorAt is when LastError was recorded, nil if there has been no
	// failure since the last success.
	LastErrorAt *time.Time `json:"last_error_at,omitempty"`
	// LastSuccessAt is the last successful delivery, nil until the first one.
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	// DisabledAt is set when health tracking turned the channel off, and only
	// then. It is what makes the dashboard say "auto-disabled" rather than
	// "disabled", and it is cleared by an explicit re-enable.
	DisabledAt *time.Time `json:"disabled_at,omitempty"`
}

// TimeoutDuration returns the channel's timeout, falling back to
// DefaultChannelTimeout when zero or negative.
func (c Channel) TimeoutDuration() time.Duration {
	if c.Timeout <= 0 {
		return DefaultChannelTimeout
	}
	return c.Timeout
}

// TimeoutSeconds returns the channel's timeout as an integer number of seconds.
func (c Channel) TimeoutSeconds() int {
	return int(c.TimeoutDuration() / time.Second)
}

// MarshalJSON serializes the channel, outputting Timeout as integer seconds
// rather than time.Duration's default nanosecond integer.
func (c Channel) MarshalJSON() ([]byte, error) {
	type Alias Channel
	return json.Marshal(&struct {
		Alias
		Timeout int `json:"timeout"`
	}{
		Alias:   Alias(c),
		Timeout: c.TimeoutSeconds(),
	})
}

// AutoDisabled reports whether health tracking, rather than an operator,
// turned this channel off. Such a channel is kept out of dispatch by
// enabled=false and comes back only through an explicit re-enable.
func (c Channel) AutoDisabled() bool { return c.DisabledAt != nil }

// HealthStatus is the coarse state the dashboard shows: "auto_disabled" when
// health tracking turned the channel off, "disabled" when an operator did,
// "failing" while deliveries are failing but the channel is still on, and
// "ok" otherwise.
func (c Channel) HealthStatus() string {
	switch {
	case c.DisabledAt != nil:
		return "auto_disabled"
	case !c.Enabled:
		return "disabled"
	case c.ConsecutiveFailures > 0:
		return "failing"
	default:
		return "ok"
	}
}

// ChannelHealthUpdate is one delivery outcome folded into a channel's health
// counters. The store applies it as a single UPDATE that both increments and
// (when the threshold is reached) disables, so several poller instances
// dispatching at once can neither lose a count nor disable twice.
type ChannelHealthUpdate struct {
	// Success clears the counters and the last error. It never re-enables a
	// channel: putting one back in rotation is an operator's decision, taken
	// through the channel update path.
	Success bool
	// Permanent marks a failure the channel will not recover from on its own
	// (401/403/404). Only permanent failures move a channel toward
	// auto-disable; a 5xx or a timeout is the provider having a bad day.
	Permanent bool
	// Error is the failure message recorded as last_error. It must already be
	// free of credentials — notifiers build their errors that way.
	Error string
	// DisableAfter is the number of consecutive permanent failures at which
	// the channel is auto-disabled. Zero (the default) never auto-disables.
	DisableAfter int
	// At is when the outcome happened. Zero means now.
	At time.Time
}

// UnmarshalJSON deserializes a channel, parsing Timeout from integer seconds
// or a Go duration string.
func (c *Channel) UnmarshalJSON(data []byte) error {
	type Alias Channel
	var aux struct {
		Alias
		Timeout *json.RawMessage `json:"timeout"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*c = Channel(aux.Alias)
	if aux.Timeout != nil && len(*aux.Timeout) > 0 && string(*aux.Timeout) != "null" {
		d, err := ParseTimeout(*aux.Timeout)
		if err != nil {
			return err
		}
		c.Timeout = d
	}
	return nil
}

// ParseTimeout parses and bounds a channel timeout value. It accepts integer
// seconds (e.g. 15) or a Go duration string (e.g. "15s"). Values outside
// [MinChannelTimeout, MaxChannelTimeout] are rejected.
func ParseTimeout(raw []byte) (time.Duration, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return DefaultChannelTimeout, nil
	}
	// Try parsing as integer number of seconds first (e.g. 15).
	var sec int
	if err := json.Unmarshal(raw, &sec); err == nil {
		d := time.Duration(sec) * time.Second
		if d < MinChannelTimeout || d > MaxChannelTimeout {
			return 0, fmt.Errorf("must be between %s and %s", MinChannelTimeout, MaxChannelTimeout)
		}
		return d, nil
	}
	// Try parsing as a string (e.g. "15s", "30s", "1m", or "15").
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			return DefaultChannelTimeout, nil
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			if sec, errAtoi := strconv.Atoi(s); errAtoi == nil {
				d = time.Duration(sec) * time.Second
			} else {
				return 0, fmt.Errorf("invalid duration %q (want a Go duration such as \"15s\" or integer seconds)", s)
			}
		}
		if d < MinChannelTimeout || d > MaxChannelTimeout {
			return 0, fmt.Errorf("must be between %s and %s", MinChannelTimeout, MaxChannelTimeout)
		}
		return d, nil
	}
	return 0, fmt.Errorf("must be integer seconds or duration string such as \"15s\"")
}

// Alert records one rule match on one event. EventID is the source event's
// TOID-based id; (RuleID, EventID) is unique so the same match can never
// fire twice.
type Alert struct {
	ID         int64           `json:"id"`
	MonitorID  int64           `json:"monitor_id"`
	RuleID     int64           `json:"rule_id"`
	EventID    string          `json:"event_id"`
	Payload    json.RawMessage `json:"payload"`
	Enrichment json.RawMessage `json:"enrichment,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	// InhibitedByRuleID is set when an inhibition rule suppressed this
	// alert's delivery. Nil means delivered (or never subjected to
	// inhibition); the alert row itself is always stored.
	InhibitedByRuleID *int64 `json:"inhibited_by_rule_id,omitempty"`
	// Severity is the alert severity copied from the rule at creation time.
	// It is stored so changing a rule's severity later does not rewrite
	// history.
	Severity Severity `json:"severity"`
	// LedgerClosedAt is the matching event's ledger close time. CreateAlert
	// uses it to stamp monitors.last_matched_at; it is not stored on the
	// alert row. Zero skips the stamp so callers that only persist an
	// alert (tests, retries) do not invent a wall-clock match time.
	LedgerClosedAt time.Time `json:"-"`
	// Suppressed is true when a maintenance window was active when the
	// alert was dispatched. Detection still happened and the row is still
	// persisted; SuppressionReason carries the window's reason.
	Suppressed        bool   `json:"suppressed"`
	SuppressionReason string `json:"suppression_reason,omitempty"`
	// Ledger is the sequence of the ledger the matching event came from. It
	// is stored so retention and reorg handling can address alerts by ledger
	// without parsing the payload. Zero for alerts persisted without one.
	Ledger uint32 `json:"ledger,omitempty"`
	// RetractedAt is set when the ledger this alert came from was orphaned by
	// a chain reorganisation. A retracted alert is kept (the notification
	// cannot be unsent) but reported as no longer reflecting canonical chain
	// history. Nil means the alert is still believed to be on the chain.
	RetractedAt *time.Time `json:"retracted_at,omitempty"`
	// Cooldown, when > 0, makes CreateAlert suppress this alert if the rule
	// already fired within the window. It is rule config, not alert data, so
	// it is never persisted on the alert row.
	Cooldown time.Duration `json:"-"`
	// Network is the Stellar network the matching event came from, copied
	// from the monitor by CreateAlert. It is stored rather than derived by
	// joining monitors (which cascade-delete) so the dashboard's per-network
	// filter and the reorg retraction both stay indexed comparisons.
	Network string `json:"network,omitempty"`
	// SuppressedSinceLast is set by CreateAlert when a row is created: the
	// number of matches this rule dropped under its cooldown since the
	// previous alert. CreateAlert also folds it into Payload so the stored
	// alert and the dispatched notification both report it.
	SuppressedSinceLast int64 `json:"-"`
	// Backfilled marks an alert produced by a historical replay (see
	// internal/backfill) rather than live ingestion. It is persisted so an
	// operator can tell a replayed match from a real-time one.
	Backfilled bool `json:"backfilled"`
	// AcknowledgedAt is when an operator acknowledged the alert. Nil means it
	// has not been acknowledged, so an escalation attached to it keeps
	// running. Acknowledging stops escalation.
	AcknowledgedAt *time.Time `json:"acknowledged_at"`
}

// Maintenance window scopes. A window suppresses matching alerts for its
// scope only: global (everything), monitor (one monitor), contract (one
// contract ID across monitors).
const (
	MaintenanceScopeGlobal   = "global"
	MaintenanceScopeMonitor  = "monitor"
	MaintenanceScopeContract = "contract"
)

// ValidMaintenanceScope reports whether s names a scope the store accepts.
func ValidMaintenanceScope(s string) bool {
	return s == MaintenanceScopeGlobal || s == MaintenanceScopeMonitor || s == MaintenanceScopeContract
}

// MaintenanceWindow is a time-bounded silence. Alerts raised inside
// [StartAt, EndAt) for its scope are persisted but not delivered.
// MonitorID and ContractID are set only when the scope calls for them.
type MaintenanceWindow struct {
	ID         int64     `json:"id"`
	Reason     string    `json:"reason"`
	Scope      string    `json:"scope"`
	MonitorID  *int64    `json:"monitor_id,omitempty"`
	ContractID *string   `json:"contract_id,omitempty"`
	StartAt    time.Time `json:"start_at"`
	EndAt      time.Time `json:"end_at"`
	CreatedAt  time.Time `json:"created_at"`
}

// MaintenanceWindowFilter narrows ListMaintenanceWindows. Zero values mean
// "no constraint".
//
// Active, when true, restricts the listing to windows whose [start, end)
// interval contains At (defaulting to now when At is zero). Upcoming, when
// true, restricts it to windows that start after At. Both false lists every
// window.
type MaintenanceWindowFilter struct {
	Active   bool
	Upcoming bool
	At       time.Time
	Limit    int
}

// AlertOutcome reports what CreateAlert did with a match.
type AlertOutcome string

const (
	// AlertCreated means a new alert row was written.
	AlertCreated AlertOutcome = "created"
	// AlertDuplicate means an alert for the same (rule_id, event_id) already
	// existed — the dedup guard — so nothing was written.
	AlertDuplicate AlertOutcome = "duplicate"
	// AlertSuppressed means the rule was inside its cooldown window, so the
	// match was counted and no alert was written.
	AlertSuppressed AlertOutcome = "suppressed"
)

// Status values persisted on delivery_attempts.status. Anything else is
// rejected by the API; the store filters on these exact strings.
const (
	DeliveryStatusSuccess = "success"
	DeliveryStatusFailed  = "failed"
)

// ValidDeliveryStatus reports whether s is a value the store actually
// writes. The empty string is not valid here — callers that mean "no
// filter" should check for empty themselves.
func ValidDeliveryStatus(s string) bool {
	return s == DeliveryStatusSuccess || s == DeliveryStatusFailed
}

// DeliveryAttempt records one try at sending an alert through a channel.
type DeliveryAttempt struct {
	ID              int64     `json:"id"`
	AlertID         int64     `json:"alert_id"`
	ChannelID       int64     `json:"channel_id"`
	Status          string    `json:"status"` // DeliveryStatusSuccess or DeliveryStatusFailed
	ResponseSnippet string    `json:"response_snippet"`
	AttemptedAt     time.Time `json:"attempted_at"`
}

// AbsenceState is the last time a rule saw the event it is waiting for.
// Absence-of-event rules are driven by a periodic sweep rather than by event
// arrival, so the sweep needs one of these per (rule, awaited pattern); there
// is no event to compare against, only a clock.
//
// LastSeen is the wall-clock instant the process saw the awaited event, or —
// for a rule that has never seen it — the instant the rule was first observed
// by a sweep. Either way it is persisted, so a restart resumes measuring
// silence instead of resetting the clock.
type AbsenceState struct {
	RuleID    int64     `json:"rule_id"`
	EventName string    `json:"event_name"`
	LastSeen  time.Time `json:"last_seen_at"`
}

// IngestState is the poller's checkpoint: the last fully processed ledger
// and, mid-page, the last getEvents cursor.
type IngestState struct {
	LastLedger uint32    `json:"last_ledger"`
	LastCursor string    `json:"last_cursor"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Backfill is the persisted progress of a historical replay for one monitor.
// A monitor has at most one row: an interrupted run resumes from
// NextLedger/Cursor instead of starting over. Complete marks a finished run,
// which a later backfill of the same monitor replaces.
type Backfill struct {
	MonitorID int64 `json:"monitor_id"`
	// FromLedger and ToLedger are the inclusive range the run covers, kept so
	// a resumed run reports the window it actually replayed.
	FromLedger uint32 `json:"from_ledger"`
	ToLedger   uint32 `json:"to_ledger"`
	// NextLedger is the ledger to (re)request when a run restarts; Cursor is
	// the source's opaque resume token from the last completed page.
	NextLedger uint32    `json:"next_ledger"`
	Cursor     string    `json:"cursor"`
	Deliver    bool      `json:"deliver"`
	Complete   bool      `json:"complete"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// LedgerHash is one recently ingested ledger's identity. The poller records
// these as it advances and re-reads them each cycle; a ledger whose hash
// changes is the signature of a chain reorganisation.
type LedgerHash struct {
	Ledger uint32 `json:"ledger"`
	Hash   string `json:"hash"`
}

// Ledgers persists the recent ledger-hash window used for reorg detection,
// and records the alerts a reorg orphaned. It is a separate interface so a
// backend without the feature (or a test fake) can omit it without
// pretending to implement it.
//
// Every method takes the network the window belongs to. Two chains number
// their ledgers independently, so a shared window would report a divergence on
// nearly every cycle — mainnet ledger 100 overwriting testnet ledger 100 is not
// a reorganisation — and a false positive here retracts real alerts.
type Ledgers interface {
	// RecordLedgerHashes upserts the observed hashes. Re-observing the same
	// ledger with the same hash is a no-op; re-observing it with a different
	// hash is the reorg signature and is recorded as the new value.
	RecordLedgerHashes(ctx context.Context, network string, hashes []LedgerHash) error
	// LedgerHashes returns the stored hashes for one network's ledgers in
	// [from, to].
	LedgerHashes(ctx context.Context, network string, from, to uint32) ([]LedgerHash, error)
	// PruneLedgerHashes drops one network's hashes for ledgers strictly before
	// `before`, bounding how far back a reorg can be detected.
	PruneLedgerHashes(ctx context.Context, network string, before uint32) error
	// RetractAlertsFromLedger marks every alert that came from `network` at a
	// ledger at or after `ledger` as retracted (unless already retracted),
	// returning how many rows changed. One statement keeps the correction
	// atomic. The network filter is what stops one chain's reorg orphaning
	// another chain's alerts.
	RetractAlertsFromLedger(ctx context.Context, network string, ledger uint32, at time.Time) (int64, error)
}

// EscalationStep is one step of an escalation policy: after Delay elapses it
// notifies ChannelIDs. Delay is relative to the alert for the first step and
// to the previous step thereafter, so a zero delay fires immediately.
type EscalationStep struct {
	// Position is the step's 0-based place in the policy.
	Position int `json:"position"`
	// DelaySeconds is how long to wait before this step fires.
	DelaySeconds int64 `json:"delay_seconds"`
	// ChannelIDs are the channels this step notifies.
	ChannelIDs []int64 `json:"channel_ids"`
}

// EscalationPolicy is the ordered escalation attached to one monitor. A
// monitor has at most one policy; a monitor without one keeps the flat
// channel fan-out it has always had.
type EscalationPolicy struct {
	ID        int64            `json:"id"`
	MonitorID int64            `json:"monitor_id"`
	Steps     []EscalationStep `json:"steps"`
	CreatedAt time.Time        `json:"created_at"`
	UpdatedAt time.Time        `json:"updated_at"`
}

// EscalationRun is one pending escalation step the scheduler should fire:
// the alert it belongs to, the step index, and when it is due. Snapshot is
// the notification payload captured when the escalation started, so a step
// delivered after a restart does not have to be rebuilt from scratch.
type EscalationRun struct {
	AlertID   int64           `json:"alert_id"`
	PolicyID  int64           `json:"policy_id"`
	NextStep  int             `json:"next_step"`
	NextDueAt time.Time       `json:"next_due_at"`
	Snapshot  json.RawMessage `json:"-"`
}

// AlertFilter narrows ListAlerts. Zero values mean "no constraint".
type AlertFilter struct {
	MonitorID int64
	RuleID    int64
	// ContractID matches payload->>'contract_id'. Empty means no contract filter.
	ContractID string
	// Query is the free-text alert search: a case-insensitive substring of
	// either the source event id or the payload's full text, which carries
	// contract_id, event_name and every other field the API returns. Empty
	// (or whitespace-only, see NormalizeAlertSearch) means no search filter.
	Query string
	From  time.Time
	To    time.Time
	Limit int
	// AfterID is the keyset cursor (the last id of the previous page). The
	// comparison flips with Sort: created_at_desc uses (created_at, id) <
	// the cursor row; created_at_asc uses >. Comparing only on id would
	// repeat or skip rows once sort is not newest-id.
	AfterID int64
	// Severity filters alerts by minimum severity. Empty means no filter.
	Severity Severity
	// Type filters channels by their notifier type ("slack", "discord",
	// ...). Empty means no type filter. Only meaningful for channels.
	Type string
	// Sort is an allowlisted order key: "created_at_desc" (default) or
	// "created_at_asc". Unknown values are treated as the default in the
	// store; the API rejects them with 400. Never interpolate this into SQL.
	Sort string
	// Network filters alerts by the Stellar network their event came from.
	// Empty means every network.
	Network string
}

// MaxAlertSearchLen caps a ?q= search term. The term is always bound as a
// parameter so a longer one could not corrupt the statement, but an
// unbounded term turns the search into a full table scan of a text match
// against every payload — the cap keeps one keystroke-worth of query from
// becoming the most expensive read the instance performs.
const MaxAlertSearchLen = 256

// NormalizeAlertSearch maps a caller-supplied search term onto the term the
// store matches. A blank or whitespace-only term is "no filter", not "match
// nothing": an emptied search box that clears itself is what the operator
// expects, and the alternative would render an empty list for a filter that
// is no longer there.
func NormalizeAlertSearch(q string) string { return strings.TrimSpace(q) }

// AlertSearchPattern builds the LIKE pattern for AlertFilter.Query. Escaping
// the term's own wildcards is what makes the search literal: an operator
// looking for a contract id containing "_" must not get every row, and a
// term taken from a URL is not in on the joke. The ESCAPE character is
// declared by every backend that uses this, so the pattern and the clause
// cannot disagree about whether a backslash is special.
func AlertSearchPattern(q string) string {
	q = NormalizeAlertSearch(q)
	if q == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("%")
	for _, r := range q {
		switch r {
		case '\\', '%', '_':
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	b.WriteString("%")
	return b.String()
}

// MonitorStats answers "is this monitor actually doing anything" for one
// monitor: the counts a caller checks before deleting it or debugging why it
// never fires. Every field is an explicit zero rather than a null when the
// monitor has no history, because "no alerts yet" is the common case and a
// client should not have to special-case it.
type MonitorStats struct {
	MonitorID      int64            `json:"monitor_id"`
	Alerts         int64            `json:"alerts"`
	AlertsLast24h  int64            `json:"alerts_last_24h"`
	AlertsLast7d   int64            `json:"alerts_last_7d"`
	LastAlertAt    *time.Time       `json:"last_alert_at,omitempty"`
	DeliveriesOK   int64            `json:"deliveries_succeeded"`
	DeliveriesFail int64            `json:"deliveries_failed"`
	Rules          []RuleMatchCount `json:"rules"`
}

// RuleMatchCount is one rule's share of a monitor's alerts. Rules are listed
// whether or not they have matched, so a rule that never fires is visible as
// a zero instead of being absent — the absence of a row and a rule that does
// not match are the same thing to a client only if the client can tell them
// apart, and this lets it.
type RuleMatchCount struct {
	RuleID int64  `json:"rule_id"`
	Type   string `json:"type"`
	Alerts int64  `json:"alerts"`
}

// ListFilter pages monitors or channels. Zero values mean "no constraint"
// besides the store's default page size. AfterID uses the same newest-first
// keyset as AlertFilter (id < AfterID) so the API does not grow a second
// cursor dialect.
//
// Query, Sort and Enabled apply to ListMonitorsPage. Channels ignore them
// (EnabledOnly stays the channels listing's on/off switch so enabled=false
// there still means "all", matching the pre-tri-state API).
type ListFilter struct {
	EnabledOnly bool
	// Type filters channels by notifier type ("slack", "discord", ...).
	// Empty means no type filter; only meaningful for channels.
	Type string
	// Enabled is the monitors tri-state filter: nil = all (default), true =
	// enabled only, false = disabled only. When nil, EnabledOnly is used.
	Enabled *bool
	// Query is a case-insensitive name substring. Empty means no name filter.
	Query string
	// Sort is an allowlisted order key: "name" (default), "id", "created_at".
	// Unknown values are treated as "name"; never interpolate this into SQL.
	Sort    string
	Limit   int
	AfterID int64
	// Network filters monitors by the Stellar network they watch. Empty
	// means every network. Only meaningful for monitors.
	Network string
}

// Stats is the aggregate snapshot served by GET /stats.
//
// LastLedger and LastPollAt describe the instance's ingest position: with one
// network they are that network's, and with several they come from whichever
// checkpoint advanced most recently. Networks carries the per-network detail
// so a dashboard can tell "testnet is current, mainnet is two hours behind"
// instead of showing one blended number that hides it.
type Stats struct {
	Monitors     int64          `json:"monitors"`
	Rules        int64          `json:"rules"`
	Channels     int64          `json:"channels"`
	Alerts       int64          `json:"alerts"`
	AlertsLast24 int64          `json:"alerts_last_24h"`
	LastLedger   uint32         `json:"last_ledger"`
	LastPollAt   time.Time      `json:"last_poll_at"`
	Networks     []NetworkStats `json:"networks,omitempty"`
}

// NetworkStats is one network's ingest checkpoint as stored, independent of
// any other network's.
type NetworkStats struct {
	Network    string    `json:"network"`
	LastLedger uint32    `json:"last_ledger"`
	LastPollAt time.Time `json:"last_poll_at"`
}

// summarizeCheckpoints fills Stats' ingest fields from the checkpoints a
// backend read, which must arrive newest-updated first.
//
// Shared by both backends so the dashboard cannot report one instance's
// position differently depending on the DATABASE_URL scheme. Networks lists the
// named networks; the pre-multi-network ” checkpoint is listed only while it is
// the only one there is, so an instance that never configured a second network
// keeps reporting exactly the figure it always did.
func summarizeCheckpoints(st *Stats, cps []NetworkStats) {
	var named, legacy []NetworkStats
	for _, c := range cps {
		if c.Network == "" {
			legacy = append(legacy, c)
		} else {
			named = append(named, c)
		}
	}
	st.Networks = named
	if len(named) == 0 {
		st.Networks = legacy
	}
	if len(cps) > 0 {
		st.LastLedger = cps[0].LastLedger
		st.LastPollAt = cps[0].LastPollAt
	}
}

// AlertSeriesDays is the overview chart window: today (UTC) and the 29
// preceding UTC days. A spike only shows up against that quiet baseline.
const AlertSeriesDays = 30

// AlertDayCount is one UTC calendar-day bucket of alert totals.
type AlertDayCount struct {
	// Day is YYYY-MM-DD in UTC.
	Day   string `json:"day"`
	Count int64  `json:"count"`
}

// ClampAlertSeriesDays maps a caller-supplied window onto a bounded range.
// Non-positive values become AlertSeriesDays so a missing query param cannot
// collapse the series; 90 is a hard cap so a typo cannot scan unbounded history.
func ClampAlertSeriesDays(days int) int {
	if days <= 0 {
		return AlertSeriesDays
	}
	if days > 90 {
		return 90
	}
	return days
}

// Monitors persists monitors and their channel attachments.
type Monitors interface {
	CreateMonitor(ctx context.Context, m *Monitor) error
	GetMonitor(ctx context.Context, id int64) (*Monitor, error)
	ListMonitors(ctx context.Context, enabledOnly bool) ([]Monitor, error)
	// ListMonitorsPage is the keyset-paginated listing used by the API and
	// dashboard. ListMonitors stays unpaginated for the poller, which must
	// see every enabled monitor in one shot.
	ListMonitorsPage(ctx context.Context, f ListFilter) ([]Monitor, error)
	UpdateMonitor(ctx context.Context, m *Monitor) error
	DeleteMonitor(ctx context.Context, id int64) error
	SetMonitorChannels(ctx context.Context, monitorID int64, channelIDs []int64) error
	// SetMonitorsEnabled sets enabled on every existing id in one statement.
	// Unknown IDs are returned rather than treated as an error so a mixed
	// list still applies to the known monitors. Duplicate ids are collapsed.
	SetMonitorsEnabled(ctx context.Context, ids []int64, enabled bool) (updated int, unknown []int64, err error)
	// DuplicateMonitor copies a monitor with its rules and channel
	// attachments in one transaction. The copy is always created
	// disabled so it cannot start alerting before it has been reviewed.
	// Alerts are not copied.
	DuplicateMonitor(ctx context.Context, id int64) (*Monitor, error)
}

// CopyMonitorName returns a unique name for a duplicated monitor.
// The first copy is "name (copy)"; collisions become "name (copy 2)",
// then (copy 3), and so on. existing is the set of names already in use.
func CopyMonitorName(src string, existing []string) string {
	taken := make(map[string]struct{}, len(existing))
	for _, n := range existing {
		taken[n] = struct{}{}
	}
	candidate := src + " (copy)"
	if _, ok := taken[candidate]; !ok {
		return candidate
	}
	for i := 2; ; i++ {
		candidate = fmt.Sprintf("%s (copy %d)", src, i)
		if _, ok := taken[candidate]; !ok {
			return candidate
		}
	}
}

// Rules persists rules.
type Rules interface {
	CreateRule(ctx context.Context, r *Rule) error
	// CreateRules inserts the batch in one transaction and fills each
	// rule's ID in request order. An empty slice is a no-op.
	CreateRules(ctx context.Context, rules []*Rule) error
	GetRule(ctx context.Context, id int64) (*Rule, error)
	ListRules(ctx context.Context, monitorID int64, enabledOnly bool) ([]Rule, error)
	UpdateRule(ctx context.Context, r *Rule) error
	DeleteRule(ctx context.Context, id int64) error
}

// Channels persists notification channels.
type Channels interface {
	CreateChannel(ctx context.Context, c *Channel) error
	GetChannel(ctx context.Context, id int64) (*Channel, error)
	ListChannels(ctx context.Context, enabledOnly bool) ([]Channel, error)
	ListChannelsPage(ctx context.Context, f ListFilter) ([]Channel, error)
	UpdateChannel(ctx context.Context, c *Channel) error
	DeleteChannel(ctx context.Context, id int64) error
	// ListMonitorsForChannel returns monitors attached to a channel, including
	// every attachment so callers can identify monitors left without a channel.
	ListMonitorsForChannel(ctx context.Context, channelID int64) ([]Monitor, error)
	// ListChannelsForMonitor returns the enabled channels a monitor alerts to.
	ListChannelsForMonitor(ctx context.Context, monitorID int64) ([]Channel, error)
	// RecordChannelHealth folds one delivery outcome into a channel's health
	// counters, auto-disabling it once DisableAfter consecutive permanent
	// failures have accumulated. Unknown channel ids are ignored rather than
	// an error: a channel deleted mid-dispatch is a race, not a bug.
	RecordChannelHealth(ctx context.Context, channelID int64, u ChannelHealthUpdate) error
	// ListChannelsByIDs returns the enabled channels among ids, ordered by id,
	// so an escalation step can notify its channel set in one query.
	ListChannelsByIDs(ctx context.Context, ids []int64) ([]Channel, error)
}

// Alerts persists alerts and delivery attempts.
type Alerts interface {
	// CreateAlert inserts a new alert unless the rule is inside its cooldown
	// window (Alert.Cooldown), in which case the match is counted and
	// AlertSuppressed is returned. AlertDuplicate means the (rule_id,
	// event_id) dedup guard rejected it. Enforcing both here, under the same
	// transaction, keeps the decision race-safe across poller instances and
	// restarts. On a new row, a non-zero LedgerClosedAt is written to
	// monitors.last_matched_at when it is newer than the stored value, and
	// Alert.SuppressedSinceLast is filled in.
	CreateAlert(ctx context.Context, a *Alert) (AlertOutcome, error)
	GetAlert(ctx context.Context, id int64) (*Alert, error)
	ListAlerts(ctx context.Context, f AlertFilter) ([]Alert, error)
	// ListAlertsStream walks the same filter as ListAlerts, calling fn per
	// row. f.Limit caps the total rows, not the page size, so an export
	// streams with bounded memory. See streamAlerts for the guarantees.
	ListAlertsStream(ctx context.Context, f AlertFilter, fn func(Alert) error) error
	RecordDeliveryAttempt(ctx context.Context, d *DeliveryAttempt) error
	// ListDeliveryAttempts returns attempts for one alert, oldest first.
	// status empty means no filter; otherwise it is applied in SQL.
	ListDeliveryAttempts(ctx context.Context, alertID int64, status string) ([]DeliveryAttempt, error)
	// DeleteExpiredAlerts removes up to limit alerts with created_at
	// before cutoff. delivery_attempts follow via ON DELETE CASCADE.
	DeleteExpiredAlerts(ctx context.Context, cutoff time.Time, limit int) (deleted int64, err error)
	// ExpiredAlerts returns up to limit alerts with created_at before cutoff,
	// oldest first. Retention uses it to read a batch an archiver can persist
	// before DeleteExpiredAlerts removes it, so a failed archive can block the
	// delete. Ordering matches DeleteExpiredAlerts exactly.
	ExpiredAlerts(ctx context.Context, cutoff time.Time, limit int) ([]Alert, error)
}

// DeadLetter represents a permanently failed delivery attempt.
type DeadLetter struct {
	ID           int64     `json:"id"`
	AlertID      int64     `json:"alert_id"`
	ChannelID    int64     `json:"channel_id"`
	LastError    string    `json:"last_error"`
	AttemptCount int       `json:"attempt_count"`
	LastStatus   int       `json:"last_status"`
	CreatedAt    time.Time `json:"created_at"`
}

// DeadLetterFilter narrows ListDeadLetters.
type DeadLetterFilter struct {
	ChannelID int64
	AlertID   int64
	Limit     int
	AfterID   int64
}

// DeadLetters persists permanently failed deliveries.
type DeadLetters interface {
	CreateDeadLetter(ctx context.Context, d *DeadLetter) error
	GetDeadLetter(ctx context.Context, id int64) (*DeadLetter, error)
	ListDeadLetters(ctx context.Context, f DeadLetterFilter) ([]DeadLetter, error)
	DeleteDeadLetter(ctx context.Context, id int64) error
}

// MaintenanceWindows persists alert-silencing windows and marks suppressed
// alerts. ActiveMaintenanceWindow is the one check on the delivery path.
type MaintenanceWindows interface {
	CreateMaintenanceWindow(ctx context.Context, w *MaintenanceWindow) error
	GetMaintenanceWindow(ctx context.Context, id int64) (*MaintenanceWindow, error)
	ListMaintenanceWindows(ctx context.Context, f MaintenanceWindowFilter) ([]MaintenanceWindow, error)
	UpdateMaintenanceWindow(ctx context.Context, w *MaintenanceWindow) error
	DeleteMaintenanceWindow(ctx context.Context, id int64) error
	// ActiveMaintenanceWindow returns the most specific window covering
	// (monitorID, contractID) at time at, or nil when none is active.
	ActiveMaintenanceWindow(ctx context.Context, monitorID int64, contractID string, at time.Time) (*MaintenanceWindow, error)
	// SetAlertSuppressed marks a persisted alert as silenced, recording
	// the reason so the operator can see why it was not delivered.
	SetAlertSuppressed(ctx context.Context, alertID int64, reason string) error
}

// Ingest persists the poller checkpoints. One per network: two chains advance
// at their own pace and one poller can lag while another is current, so a single
// shared cursor would either rewind or skip one of them every cycle.
//
// network == "" is the pre-multi-network checkpoint row, kept readable so an
// instance that polls no named network behaves exactly as it did before.
type Ingest interface {
	GetIngestState(ctx context.Context, network string) (IngestState, error)
	SetIngestState(ctx context.Context, network string, s IngestState) error
}

// Absence persists the last-seen clocks that absence-of-event rules measure
// silence against.
type Absence interface {
	// ListAbsenceState returns every stored clock. The sweep reads them all
	// in one query: the table holds at most one row per absence rule, and a
	// per-rule lookup would mean a query per rule on every tick.
	ListAbsenceState(ctx context.Context) ([]AbsenceState, error)
	// RecordAbsenceSeen advances the clock for (ruleID, eventName) to at.
	// It never moves the clock backwards: a replayed or out-of-order event
	// must not make a rule look fresher than it is, and the sweep's initial
	// baseline write must not undo a real observation. Writing an older
	// instant is therefore a no-op rather than an error.
	RecordAbsenceSeen(ctx context.Context, ruleID int64, eventName string, at time.Time) error
}

// Backfills persists historical replay progress, so an interrupted backfill
// resumes where it stopped instead of replaying the whole range. GetBackfill
// returns ErrNotFound when the monitor has never been backfilled.
type Backfills interface {
	GetBackfill(ctx context.Context, monitorID int64) (Backfill, error)
	UpsertBackfill(ctx context.Context, b *Backfill) error
}

// SavedSearch is a named, reusable alert filter combination.
type SavedSearch struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Filter    SavedSearchFilter `json:"filter"`
	IsDefault bool              `json:"is_default"`
	CreatedAt time.Time         `json:"created_at"`
}

// SavedSearchFilter stores the structured filter fields so surviving
// renames is straightforward and stale references degrade gracefully.
type SavedSearchFilter struct {
	MonitorID  int64  `json:"monitor_id,omitempty"`
	RuleID     int64  `json:"rule_id,omitempty"`
	ContractID string `json:"contract_id,omitempty"`
	Sort       string `json:"sort,omitempty"`
}

// SavedSearches persists saved alert searches.
type SavedSearches interface {
	CreateSavedSearch(ctx context.Context, s *SavedSearch) error
	ListSavedSearches(ctx context.Context) ([]SavedSearch, error)
	GetSavedSearch(ctx context.Context, id int64) (*SavedSearch, error)
	DeleteSavedSearch(ctx context.Context, id int64) error
	SetDefaultSearch(ctx context.Context, id int64) error
	ClearDefaultSearch(ctx context.Context, id int64) error
}

// Audit actions persisted on audit_log.action. A small closed set so the
// API can filter on it and clients can branch on a stable token.
const (
	AuditActionCreate = "create"
	AuditActionUpdate = "update"
	AuditActionDelete = "delete"
)

// AuditEntry is one append-only record of a mutating operation on a monitor,
// rule or channel. Diff records which fields were sent — never their values:
// channel config holds webhook URLs, bot tokens and SMTP credentials, so only
// the fact that a field changed is stored, never what it changed to.
// Actor is the request ID today and will carry the authenticated principal
// once auth identifies one; the column is a plain string so that needs no
// migration.
type AuditEntry struct {
	ID         int64           `json:"id"`
	Actor      string          `json:"actor"`
	Action     string          `json:"action"`
	TargetType string          `json:"target_type"`
	TargetID   int64           `json:"target_id"`
	Diff       json.RawMessage `json:"diff,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// AuditFilter narrows ListAuditEntries. Zero values mean "no constraint".
type AuditFilter struct {
	TargetType string
	TargetID   int64
	From       time.Time
	To         time.Time
	Limit      int
}

// Digest modes persisted on channels.digest_mode. The empty string means
// immediate delivery.
const (
	DigestModeOff    = ""
	DigestModeWindow = "window"
)

// DigestAlert is one alert waiting to be summarised into a channel digest.
// Payload is the serialised notify.Alert; the store keeps it opaque so the
// store package does not depend on the notification layer.
type DigestAlert struct {
	ID        int64           `json:"id"`
	ChannelID int64           `json:"channel_id"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

// DigestQueue persists alerts awaiting a channel's digest flush so a restart
// does not silently drop a partial window. Rows are owned by the channel and
// cascade when it is deleted.
type DigestQueue interface {
	// PushDigestAlert appends one serialised alert to a channel's window.
	PushDigestAlert(ctx context.Context, channelID int64, payload json.RawMessage) error
	// ListDigestAlerts returns a channel's pending alerts oldest first.
	ListDigestAlerts(ctx context.Context, channelID int64) ([]DigestAlert, error)
	// DeleteDigestAlerts removes the flushed rows by id. Deleting by id
	// rather than draining the channel keeps an alert that arrived during
	// the flush from being dropped.
	DeleteDigestAlerts(ctx context.Context, channelID int64, ids []int64) error
}

// Audits is append-only by design: there is deliberately no Update or Delete
// method, so nothing can rewrite or erase the log through the store.
type Audits interface {
	// CreateAuditEntry appends one entry and fills in its ID and CreatedAt.
	CreateAuditEntry(ctx context.Context, e *AuditEntry) error
	// ListAuditEntries returns entries newest first, at most f.Limit of
	// them (default and cap applied by the store).
	ListAuditEntries(ctx context.Context, f AuditFilter) ([]AuditEntry, error)
}

// MonitorTemplate defines a reusable monitor shape. Monitors created from
// a template are one-time copies: editing the template does not retroactively
// change existing monitors, so operators can tweak instances without fear.
type MonitorTemplate struct {
	ID          int64                 `json:"id"`
	Name        string                `json:"name"`
	Description string                `json:"description"`
	Rules       []MonitorTemplateRule `json:"rules"`
	ChannelIDs  []int64               `json:"channel_ids"`
	Parameters  []TemplateParameter   `json:"parameters"`
	CreatedAt   time.Time             `json:"created_at"`
}

// MonitorTemplateRule is one rule definition inside a template. Params may
// contain {{param_name}} placeholders that are substituted at instantiation.
type MonitorTemplateRule struct {
	Type   string          `json:"type"`
	Params json.RawMessage `json:"params"`
}

// TemplateParameter describes one substitutable value.
type TemplateParameter struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required"`
	Default     string `json:"default,omitempty"`
}

// templateChannelIDs normalises a template's channel list for storage. A nil
// slice would be written as SQL NULL (Postgres) or JSON null (SQLite), and
// channel_ids is NOT NULL: an empty list means "no channels", not "unknown".
func templateChannelIDs(ids []int64) []int64 {
	if ids == nil {
		return []int64{}
	}
	return ids
}

// MonitorTemplates persists monitor templates.
type MonitorTemplates interface {
	CreateMonitorTemplate(ctx context.Context, t *MonitorTemplate) error
	GetMonitorTemplate(ctx context.Context, id int64) (*MonitorTemplate, error)
	ListMonitorTemplates(ctx context.Context) ([]MonitorTemplate, error)
	UpdateMonitorTemplate(ctx context.Context, t *MonitorTemplate) error
	DeleteMonitorTemplate(ctx context.Context, id int64) error
}

// BackupChannel is a Channel with its Config included for configuration
// export. The normal Channel tags Config with json:"-" to prevent leaks
// through the API; the backup path needs the decrypted config and controls
// its own output.
type BackupChannel struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Type      string          `json:"type"`
	Config    json.RawMessage `json:"config"`
	Enabled   bool            `json:"enabled"`
	CreatedAt time.Time       `json:"created_at"`
}

// Escalations persists monitor escalation policies and the per-alert
// scheduling state that keeps a tiered notification going across restarts.
type Escalations interface {
	// GetEscalationPolicyForMonitor returns the policy attached to a monitor,
	// or ErrNotFound when the monitor has none (and therefore fans out flat).
	GetEscalationPolicyForMonitor(ctx context.Context, monitorID int64) (*EscalationPolicy, error)
	// GetEscalationPolicy returns a policy by its own id, used by the
	// scheduler to load the steps for a due escalation.
	GetEscalationPolicy(ctx context.Context, policyID int64) (*EscalationPolicy, error)
	// SetEscalationPolicy replaces the monitor's policy with steps in one
	// transaction, so the old policy can never be observed half-deleted.
	SetEscalationPolicy(ctx context.Context, monitorID int64, steps []EscalationStep) (*EscalationPolicy, error)
	DeleteEscalationPolicy(ctx context.Context, monitorID int64) error
	// ScheduleEscalation records the next due step for an alert. Re-scheduling
	// the same alert replaces the pending schedule rather than duplicating it.
	ScheduleEscalation(ctx context.Context, alertID, policyID int64, snapshot json.RawMessage, nextStep int, nextDue time.Time) error
	// DueEscalations returns unacknowledged, unfinished escalations whose next
	// step is due at or before now, oldest due first.
	DueEscalations(ctx context.Context, now time.Time, limit int) ([]EscalationRun, error)
	// AdvanceEscalation moves a pending escalation to its next step.
	AdvanceEscalation(ctx context.Context, alertID int64, nextStep int, nextDue time.Time) error
	// CompleteEscalation clears a pending escalation once all steps have fired.
	CompleteEscalation(ctx context.Context, alertID int64) error
	// AcknowledgeAlert stamps an alert acknowledged and stops its escalation.
	AcknowledgeAlert(ctx context.Context, alertID int64) error
}

// Workspaces persists the tenant list. EnsureWorkspace is idempotent so
// startup can assert every configured workspace exists without caring whether
// a previous start already created it.
type Workspaces interface {
	EnsureWorkspace(ctx context.Context, id workspace.ID) error
	// AssignLegacyNetwork labels rows the network migration left unlabelled.
	// It runs once at startup in the instance's cross-tenant scope, and its
	// predicate is the empty network rather than a tenant, so it is idempotent.
	AssignLegacyNetwork(ctx context.Context, network string) (int64, error)
}

// APITokens persists the scoped, database-backed API tokens. It is exactly
// auth.TokenStore — declared here too so store.Store carries it, and asserted
// against that interface in tokens.go so the two cannot drift.
//
// Every method scopes itself to ctx's workspace except TokenByHash, which runs
// before tenancy is known: the row it finds is what decides the workspace, so
// a caller cannot name its own.
type APITokens interface {
	CreateAPIToken(ctx context.Context, t *auth.Token) error
	TokenByHash(ctx context.Context, hash string) (token *auth.Token, found bool, err error)
	ListAPITokens(ctx context.Context) ([]auth.Token, error)
	RevokeAPIToken(ctx context.Context, id int64) error
	TouchAPIToken(ctx context.Context, id int64, at time.Time) error
}

// Store is everything the application needs from persistence.
//
// Tenancy: every method below is scoped to the workspace carried by ctx
// (internal/workspace), and rows a method writes land in that workspace. A
// context with no workspace is treated as the default one rather than an
// error, so an instance that never configured tenancy behaves exactly as it
// did before workspaces existed. internal/workspace.WithSystem marks the
// cross-tenant instance work (ingest, delivery, retention, reorg); the
// methods that accept it say so in their own comments, and the rest are
// documented in internal/store/workspace_scope.go.
type Store interface {
	Monitors
	Rules
	Channels
	Alerts
	MaintenanceWindows
	DeadLetters
	Inhibitions
	Ingest
	Absence
	Backfills
	Ledgers
	SavedSearches
	MonitorTemplates
	Audits
	DigestQueue
	Workspaces
	APITokens
	Escalations
	GetStats(ctx context.Context) (Stats, error)
	// GetMonitorStats returns one monitor's alert, delivery and per-rule
	// counts, or ErrNotFound when the monitor does not exist. It exists
	// because GetStats answers "how big is this instance" and says nothing
	// about whether a particular monitor is doing any work.
	GetMonitorStats(ctx context.Context, monitorID int64) (MonitorStats, error)
	// AlertCountsByDay returns UTC calendar-day alert totals for `days`
	// consecutive days ending today (UTC). Days with no alerts are present
	// with count 0 so a chart has no gaps. Bucketing is done in SQL.
	AlertCountsByDay(ctx context.Context, days int) ([]AlertDayCount, error)
	// GroupAlerts creates or increments the alert group for key
	// with windowStart and returns whether the alert should be
	// delivered immediately (first alert in the window) and the
	// current group count. Grouping is off when window duration is
	// zero, which callers enforce before invoking this method.
	GroupAlerts(ctx context.Context, key string, windowStart time.Time) (shouldDeliver bool, currentCount int64, err error)
	// CreateAlertGroup creates or increments the alert group row
	// identified by key and windowStart. Returns the new count.
	CreateAlertGroup(ctx context.Context, key string, windowStart time.Time) (int64, error)
	Ping(ctx context.Context) error
	Close()
}
