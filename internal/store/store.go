// Package store defines SoroBeacon's persistence interfaces and models.
// The Postgres implementation lives in postgres.go; tests and alternative
// backends can implement the narrow per-domain interfaces below.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// Monitor watches one or more Soroban contracts.
type Monitor struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	ContractIDs []string  `json:"contract_ids"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
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
// TODO(contributors): encrypt Config at rest; see CONTRIBUTING.md.
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
}

// DeliveryAttempt records one try at sending an alert through a channel.
type DeliveryAttempt struct {
	ID              int64     `json:"id"`
	AlertID         int64     `json:"alert_id"`
	ChannelID       int64     `json:"channel_id"`
	Status          string    `json:"status"` // "success" or "failed"
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

// AlertFilter narrows ListAlerts. Zero values mean "no constraint".
type AlertFilter struct {
	MonitorID int64
	From      time.Time
	To        time.Time
	Limit     int
	// AfterID returns alerts with id < AfterID (newest-first keyset cursor).
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

// Monitors persists monitors and their channel attachments.
type Monitors interface {
	CreateMonitor(ctx context.Context, m *Monitor) error
	GetMonitor(ctx context.Context, id int64) (*Monitor, error)
	ListMonitors(ctx context.Context, enabledOnly bool) ([]Monitor, error)
	UpdateMonitor(ctx context.Context, m *Monitor) error
	DeleteMonitor(ctx context.Context, id int64) error
	SetMonitorChannels(ctx context.Context, monitorID int64, channelIDs []int64) error
}

// Rules persists rules.
type Rules interface {
	CreateRule(ctx context.Context, r *Rule) error
	GetRule(ctx context.Context, id int64) (*Rule, error)
	ListRules(ctx context.Context, monitorID int64, enabledOnly bool) ([]Rule, error)
	UpdateRule(ctx context.Context, r *Rule) error
	DeleteRule(ctx context.Context, id int64) error
}

// Channels persists notification channels.
type Channels interface {
	CreateChannel(ctx context.Context, c *Channel) error
	GetChannel(ctx context.Context, id int64) (*Channel, error)
	ListChannels(ctx context.Context) ([]Channel, error)
	UpdateChannel(ctx context.Context, c *Channel) error
	DeleteChannel(ctx context.Context, id int64) error
	// ListChannelsForMonitor returns the enabled channels a monitor alerts to.
	ListChannelsForMonitor(ctx context.Context, monitorID int64) ([]Channel, error)
}

// Alerts persists alerts and delivery attempts.
type Alerts interface {
	// CreateAlert inserts a new alert. It returns created=false (and no
	// error) when an alert for the same (rule_id, event_id) already exists —
	// the dedup guard.
	CreateAlert(ctx context.Context, a *Alert) (created bool, err error)
	ListAlerts(ctx context.Context, f AlertFilter) ([]Alert, error)
	RecordDeliveryAttempt(ctx context.Context, d *DeliveryAttempt) error
	ListDeliveryAttempts(ctx context.Context, alertID int64) ([]DeliveryAttempt, error)
}

// Ingest persists the poller checkpoint.
type Ingest interface {
	GetIngestState(ctx context.Context) (IngestState, error)
	SetIngestState(ctx context.Context, s IngestState) error
}

// Store is everything the application needs from persistence.
type Store interface {
	Monitors
	Rules
	Channels
	Alerts
	Ingest
	GetStats(ctx context.Context) (Stats, error)
	Ping(ctx context.Context) error
	Close()
}
