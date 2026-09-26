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
}

// Channel is a configured notification destination. Config holds
// channel-specific settings including secrets (webhook URLs, bot tokens,
// SMTP credentials) — never log it and never return it from the API.
// When a ConfigCipher is configured, Config is encrypted at rest and the
// store returns it decrypted (see crypto.go).
type Channel struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Type      string          `json:"type"`
	Config    json.RawMessage `json:"-"`
	Enabled   bool            `json:"enabled"`
	CreatedAt time.Time       `json:"created_at"`
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
	// Network filters alerts by the Stellar network their event came from.
	// Empty means every network.
	Network string
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

// Workspaces is the tenancy boundary's own table: the named scopes every
// monitor, channel, alert, saved search and template belongs to.
//
// Seeding is the only write here on purpose. A workspace is created by
// configuration, not by an API call — WORKSPACE_TOKENS names the tenants an
// instance serves, and startup records them so the table describes reality
// rather than only the implicit default. Deleting one is a data-lifecycle
// decision this issue deliberately leaves out of scope.
type Workspaces interface {
	// EnsureWorkspace inserts id when it is absent, as the id's own name.
	// It is idempotent because it runs on every startup, and it never
	// overwrites a name that was set some other way.
	//
	// It takes the id directly rather than reading it from ctx: it is called
	// once per configured workspace by the process that owns the list.
	EnsureWorkspace(ctx context.Context, id workspace.ID) error
}

// APITokens persists scoped, expiring, revocable bearer credentials. The rows
// are credentials, so the shape that crosses this boundary is auth.Token and it
// carries a digest rather than a secret: the plaintext exists only in the
// response that mints it, which is what makes "shown once" a property of the
// schema rather than a promise.
//
// TokenByHash is the one method here that ignores the context's workspace, and
// it has to: it runs before the caller's tenant is known, and the row it returns
// is where the tenant comes from. Every other method is tenant-scoped like the
// rest of the package (see workspace_scope.go), so a workspace cannot list,
// revoke or refresh another's tokens — nor learn that one exists, since each of
// those answers ErrNotFound.
type APITokens interface {
	// CreateAPIToken inserts one token row and fills its ID, creation time and
	// workspace from the context.
	CreateAPIToken(ctx context.Context, t *auth.Token) error
	// TokenByHash returns the row whose digest is hash, or found false. The
	// digest is compared, never decoded, so this is an indexed read.
	TokenByHash(ctx context.Context, hash string) (token *auth.Token, found bool, err error)
	// ListAPITokens returns the caller workspace's tokens newest first,
	// including revoked and expired ones: the dashboard has to show what is
	// retired, not hide it.
	ListAPITokens(ctx context.Context) ([]auth.Token, error)
	// RevokeAPIToken retires one of the caller's tokens, keeping the first
	// revocation timestamp so a repeated call is a no-op rather than an error.
	RevokeAPIToken(ctx context.Context, id int64) error
	// TouchAPIToken stamps when a token last authenticated. It is called at
	// most once a minute per token, which is how often the stale-token report
	// needs to be right.
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
	Ingest
	Ledgers
	SavedSearches
	MonitorTemplates
	Workspaces
	APITokens
	// AssignLegacyNetwork labels every monitor and alert whose network column
	// is still empty — i.e. everything written before multi-network ingestion
	// existed — with network, returning how many rows changed. Startup calls it
	// once with the instance's primary network, before any poller runs, so an
	// upgraded single-network instance neither loses its monitors from the
	// listing nor strands its alert history outside every network filter.
	// Idempotent: the second call finds nothing to label and reports 0.
	AssignLegacyNetwork(ctx context.Context, network string) (int64, error)
	GetStats(ctx context.Context) (Stats, error)
	// AlertCountsByDay returns UTC calendar-day alert totals for `days`
	// consecutive days ending today (UTC). Days with no alerts are present
	// with count 0 so a chart has no gaps. Bucketing is done in SQL.
	AlertCountsByDay(ctx context.Context, days int) ([]AlertDayCount, error)
	Ping(ctx context.Context) error
	Close()
}
