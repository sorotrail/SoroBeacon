// Package store defines SoroBeacon's persistence interfaces and models.
// The Postgres implementation lives in postgres.go; tests and alternative
// backends can implement the narrow per-domain interfaces below.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
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
}

// Rule is one condition evaluated against every event of its monitor's
// contracts. Type selects a rules.RuleEvaluator; Params are its arguments.
type Rule struct {
	ID        int64           `json:"id"`
	MonitorID int64           `json:"monitor_id"`
	Type      string          `json:"type"`
	Params    json.RawMessage `json:"params"`
	Enabled   bool            `json:"enabled"`
}

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
	DigestWindowSeconds int64     `json:"digest_window_seconds"`
	CreatedAt           time.Time `json:"created_at"`
}

// Alert records one rule match on one event. EventID is the source event's
// TOID-based id; (RuleID, EventID) is unique so the same match can never
// fire twice.
type Alert struct {
	ID        int64           `json:"id"`
	MonitorID int64           `json:"monitor_id"`
	RuleID    int64           `json:"rule_id"`
	EventID   string          `json:"event_id"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
	// LedgerClosedAt is the matching event's ledger close time. CreateAlert
	// uses it to stamp monitors.last_matched_at; it is not stored on the
	// alert row. Zero skips the stamp so callers that only persist an
	// alert (tests, retries) do not invent a wall-clock match time.
	LedgerClosedAt time.Time `json:"-"`
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
	// SuppressedSinceLast is set by CreateAlert when a row is created: the
	// number of matches this rule dropped under its cooldown since the
	// previous alert. CreateAlert also folds it into Payload so the stored
	// alert and the dispatched notification both report it.
	SuppressedSinceLast int64 `json:"-"`
	// Backfilled marks an alert produced by a historical replay (see
	// internal/backfill) rather than live ingestion. It is persisted so an
	// operator can tell a replayed match from a real-time one.
	Backfilled bool `json:"backfilled"`
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
type Ledgers interface {
	// RecordLedgerHashes upserts the observed hashes. Re-observing the same
	// ledger with the same hash is a no-op; re-observing it with a different
	// hash is the reorg signature and is recorded as the new value.
	RecordLedgerHashes(ctx context.Context, hashes []LedgerHash) error
	// LedgerHashes returns the stored hashes for ledgers in [from, to],
	// ascending. Ledgers outside the window are omitted.
	LedgerHashes(ctx context.Context, from, to uint32) ([]LedgerHash, error)
	// PruneLedgerHashes drops hashes for ledgers strictly before `before`,
	// bounding how far back a reorg can be detected.
	PruneLedgerHashes(ctx context.Context, before uint32) error
	// RetractAlertsFromLedger marks every alert that came from a ledger at or
	// after `ledger` as retracted (unless already retracted), returning how
	// many rows changed. One statement keeps the correction atomic.
	RetractAlertsFromLedger(ctx context.Context, ledger uint32, at time.Time) (int64, error)
}

// AlertFilter narrows ListAlerts. Zero values mean "no constraint".
type AlertFilter struct {
	MonitorID int64
	RuleID    int64
	// ContractID matches payload->>'contract_id'. Empty means no contract filter.
	ContractID string
	From       time.Time
	To         time.Time
	Limit      int
	// AfterID is the keyset cursor (the last id of the previous page). The
	// comparison flips with Sort: created_at_desc uses (created_at, id) <
	// the cursor row; created_at_asc uses >. Comparing only on id would
	// repeat or skip rows once sort is not newest-id.
	AfterID int64
	// Type filters channels by their notifier type ("slack", "discord",
	// ...). Empty means no type filter. Only meaningful for channels.
	Type string
	// Sort is an allowlisted order key: "created_at_desc" (default) or
	// "created_at_asc". Unknown values are treated as the default in the
	// store; the API rejects them with 400. Never interpolate this into SQL.
	Sort string
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
}

// Stats is the aggregate snapshot served by GET /stats.
type Stats struct {
	Monitors     int64     `json:"monitors"`
	Rules        int64     `json:"rules"`
	Channels     int64     `json:"channels"`
	Alerts       int64     `json:"alerts"`
	AlertsLast24 int64     `json:"alerts_last_24h"`
	LastLedger   uint32    `json:"last_ledger"`
	LastPollAt   time.Time `json:"last_poll_at"`
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

// Ingest persists the poller checkpoint.
type Ingest interface {
	GetIngestState(ctx context.Context) (IngestState, error)
	SetIngestState(ctx context.Context, s IngestState) error
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

// Store is everything the application needs from persistence.
type Store interface {
	Monitors
	Rules
	Channels
	Alerts
	Ingest
	Backfills
	Ledgers
	SavedSearches
	MonitorTemplates
	Audits
	DigestQueue
	GetStats(ctx context.Context) (Stats, error)
	// AlertCountsByDay returns UTC calendar-day alert totals for `days`
	// consecutive days ending today (UTC). Days with no alerts are present
	// with count 0 so a chart has no gaps. Bucketing is done in SQL.
	AlertCountsByDay(ctx context.Context, days int) ([]AlertDayCount, error)
	Ping(ctx context.Context) error
	Close()
}
