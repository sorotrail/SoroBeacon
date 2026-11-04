package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

const (
	// WorkspaceExportFormat identifies the workspace export envelope format.
	WorkspaceExportFormat = "sorobeacon.workspace-export"

	// WorkspaceExportVersion is the schema version of the workspace export format.
	WorkspaceExportVersion = 1

	// DefaultWorkspaceExportPageSize is the default keyset page size used when
	// streaming alert history tables.
	DefaultWorkspaceExportPageSize = 250

	// MaxWorkspaceExportPageSize caps the per-page batch size when streaming
	// alert history tables.
	MaxWorkspaceExportPageSize = 5000

	// RedactedSecretPlaceholder replaces secret values when IncludeSecrets is false.
	RedactedSecretPlaceholder = "REDACTED"
)

// WorkspaceExportRecordKind distinguishes manifest, row, and summary records
// in a streamed workspace export.
type WorkspaceExportRecordKind string

const (
	WorkspaceExportKindManifest WorkspaceExportRecordKind = "manifest"
	WorkspaceExportKindRow      WorkspaceExportRecordKind = "row"
	WorkspaceExportKindSummary  WorkspaceExportRecordKind = "summary"
)

// WorkspaceExportConfigTables lists all workspace-scoped configuration, state,
// and rollup tables in the exact deterministic order they are emitted.
var WorkspaceExportConfigTables = []string{
	"workspaces",
	"workspace_quotas",
	"workspace_query_budget_state",
	"workspace_retention_policies",
	"channels",
	"channel_signing_keys",
	"channel_ack_reconciliations",
	"pending_digests",
	"monitors",
	"monitor_channels",
	"rules",
	"rule_absence_state",
	"alert_inhibitions",
	"escalation_policies",
	"escalation_steps",
	"escalation_step_channels",
	"maintenance_windows",
	"backfills",
	"saved_searches",
	"monitor_templates",
	"api_tokens",
	"workspace_usage_rollups",
	"workspace_notification_usage_rollups",
}

// WorkspaceExportAlertTables lists the high-volume alert history tables that
// are streamed in bounded pages when IncludeAlerts is true.
var WorkspaceExportAlertTables = []string{
	"alerts",
	"alert_dedup",
	"delivery_attempts",
	"dead_letters",
	"alert_escalations",
}

// NonExportedInstanceTables maps every schema table that is deliberately not
// part of a single-workspace export to the architectural reason it is excluded.
// TestWorkspaceExportCoversEverySchemaTable asserts that every table across all
// migrations is either in WorkspaceExportTables() or in this map.
var NonExportedInstanceTables = map[string]string{
	"schema_migrations":       "database migration bookkeeping internal to the instance",
	"ingest_state":            "legacy chain-wide poller checkpoint shared across the instance",
	"network_ingest_state":    "per-network chain-wide poller checkpoint shared across the instance",
	"ledger_hashes":           "legacy chain-wide ledger hash ring buffer used for reorg detection",
	"network_ledger_hashes":   "per-network chain-wide ledger hash ring buffer used for reorg detection",
	"contract_ingest_cursors": "per-network contract ingest cursors shared across all workspaces watching a contract",
	"retention_gaps":          "instance-wide poller ledger retention gap log",
	"event_archive":           "instance-wide raw decoded Stellar event replay corpus keyed by (network, event_id)",
	"audit_log":               "instance-wide append-only operator audit log with no workspace column",
	"persisted_operations":    "instance-wide vetted GraphQL persisted operation registry",
	"idempotency_keys":        "ephemeral HTTP transport idempotency response cache with 24h TTL",
	"workspace_deletions":     "durable workspace deletion progress and completion tombstone log",
}

// WorkspaceExportTables returns the complete ordered list of schema tables
// covered by a full workspace export (configuration tables followed by alert
// history tables).
func WorkspaceExportTables() []string {
	out := make([]string, 0, len(WorkspaceExportConfigTables)+len(WorkspaceExportAlertTables))
	out = append(out, WorkspaceExportConfigTables...)
	out = append(out, WorkspaceExportAlertTables...)
	return out
}

// WorkspaceExportTablesForOptions returns the ordered list of schema tables
// included for the given export options.
func WorkspaceExportTablesForOptions(opts WorkspaceExportOptions) []string {
	if opts.IncludeAlerts {
		return WorkspaceExportTables()
	}
	out := make([]string, len(WorkspaceExportConfigTables))
	copy(out, WorkspaceExportConfigTables)
	return out
}

// WorkspaceExportOptions configures a single-workspace export.
type WorkspaceExportOptions struct {
	// IncludeSecrets controls whether channel configs and signing key secrets
	// are decrypted and exported in cleartext (true) or redacted to "REDACTED"
	// (false, default). API token hashes are also redacted when false.
	IncludeSecrets bool
	// IncludeAlerts controls whether alert history tables (alerts, alert_dedup,
	// delivery_attempts, dead_letters, alert_escalations) are streamed in the
	// export (true) or skipped (false, default).
	IncludeAlerts bool
	// PageSize bounds the number of rows fetched per database query when
	// streaming alert history tables. Zero uses DefaultWorkspaceExportPageSize.
	PageSize int
	// ExportedAt overrides the timestamp recorded in the export manifest when
	// non-zero (used for deterministic tests).
	ExportedAt time.Time
}

func (o WorkspaceExportOptions) effectivePageSize() int {
	if o.PageSize <= 0 {
		return DefaultWorkspaceExportPageSize
	}
	if o.PageSize > MaxWorkspaceExportPageSize {
		return MaxWorkspaceExportPageSize
	}
	return o.PageSize
}

func (o WorkspaceExportOptions) effectiveExportedAt() time.Time {
	if o.ExportedAt.IsZero() {
		return time.Now().UTC().Truncate(time.Millisecond)
	}
	return o.ExportedAt.UTC()
}

// WorkspaceExportManifest is the self-describing header emitted at the start
// of a workspace export.
type WorkspaceExportManifest struct {
	Format          string    `json:"format"`
	Version         int       `json:"version"`
	WorkspaceID     string    `json:"workspace_id"`
	ExportedAt      time.Time `json:"exported_at"`
	IncludeSecrets  bool      `json:"include_secrets"`
	SecretsRedacted bool      `json:"secrets_redacted"`
	IncludeAlerts   bool      `json:"include_alerts"`
	AlertsIncluded  bool      `json:"alerts_included"`
	Tables          []string  `json:"tables"`
}

// WorkspaceExportSummary is the self-describing trailer emitted at the end of
// a workspace export with per-table row counts.
type WorkspaceExportSummary struct {
	TableCounts map[string]int64 `json:"table_counts"`
	TotalRows   int64            `json:"total_rows"`
}

// WorkspaceExportRecord is one streamed item emitted by ExportWorkspace.
type WorkspaceExportRecord struct {
	Kind     WorkspaceExportRecordKind `json:"kind"`
	Table    string                    `json:"table,omitempty"`
	Manifest *WorkspaceExportManifest  `json:"manifest,omitempty"`
	Data     any                       `json:"data,omitempty"`
	Summary  *WorkspaceExportSummary   `json:"summary,omitempty"`
}

// ExportedWorkspace represents one row from workspaces.
type ExportedWorkspace struct {
	ID                      string    `json:"id"`
	Name                    string    `json:"name"`
	DefaultRunbookURL       string    `json:"default_runbook_url,omitempty"`
	DefaultResponseGuidance string    `json:"default_response_guidance,omitempty"`
	CreatedAt               time.Time `json:"created_at"`
}

// ExportedQueryBudgetState represents one row from workspace_query_budget_state.
type ExportedQueryBudgetState struct {
	WorkspaceID string    `json:"workspace_id"`
	Tokens      float64   `json:"tokens"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ExportedWorkspaceRetentionPolicy represents one row from workspace_retention_policies.
type ExportedWorkspaceRetentionPolicy struct {
	WorkspaceID      string    `json:"workspace_id"`
	RetentionSeconds int64     `json:"retention_seconds"`
	UpdatedBy        string    `json:"updated_by,omitempty"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// ExportedChannel represents one row from channels, explicitly serialising
// Config (either redacted when IncludeSecrets is false or decrypted plaintext
// when IncludeSecrets is true).
type ExportedChannel struct {
	ID                           int64           `json:"id"`
	WorkspaceID                  string          `json:"workspace_id"`
	Name                         string          `json:"name"`
	Type                         string          `json:"type"`
	Config                       json.RawMessage `json:"config"`
	Enabled                      bool            `json:"enabled"`
	CreatedAt                    time.Time       `json:"created_at"`
	DigestMode                   string          `json:"digest_mode,omitempty"`
	DigestWindowSeconds          int64           `json:"digest_window_seconds,omitempty"`
	MinSeverity                  Severity        `json:"min_severity,omitempty"`
	TimeoutSeconds               int             `json:"timeout_seconds"`
	ConsecutiveFailures          int             `json:"consecutive_failures"`
	ConsecutivePermanentFailures int             `json:"consecutive_permanent_failures"`
	LastError                    string          `json:"last_error,omitempty"`
	LastErrorAt                  *time.Time      `json:"last_error_at,omitempty"`
	LastSuccessAt                *time.Time      `json:"last_success_at,omitempty"`
	DisabledAt                   *time.Time      `json:"disabled_at,omitempty"`
}

// ExportedChannelSigningKey represents one row from channel_signing_keys,
// explicitly serialising Secret (redacted when IncludeSecrets is false,
// decrypted when IncludeSecrets is true).
type ExportedChannelSigningKey struct {
	ID          int64      `json:"id"`
	ChannelID   int64      `json:"channel_id"`
	WorkspaceID string     `json:"workspace_id"`
	KeyID       string     `json:"key_id"`
	Secret      string     `json:"secret"`
	IsCurrent   bool       `json:"is_current"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

// ExportedMonitorChannel represents one row from monitor_channels.
type ExportedMonitorChannel struct {
	MonitorID int64 `json:"monitor_id"`
	ChannelID int64 `json:"channel_id"`
}

// ExportedRule represents one row from rules including cooldown tracking state.
type ExportedRule struct {
	ID                  int64           `json:"id"`
	MonitorID           int64           `json:"monitor_id"`
	Type                string          `json:"type"`
	Params              json.RawMessage `json:"params"`
	Enabled             bool            `json:"enabled"`
	Severity            Severity        `json:"severity"`
	Version             int             `json:"version"`
	RunbookURL          string          `json:"runbook_url,omitempty"`
	ResponseGuidance    string          `json:"response_guidance,omitempty"`
	LastAlertAt         *time.Time      `json:"last_alert_at,omitempty"`
	SuppressedSinceLast int64           `json:"suppressed_since_last,omitempty"`
}

// ExportedRuleAbsenceState represents one row from rule_absence_state.
type ExportedRuleAbsenceState struct {
	RuleID     int64     `json:"rule_id"`
	EventName  string    `json:"event_name"`
	LastSeenAt time.Time `json:"last_seen_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ExportedEscalationStep represents one row from escalation_steps.
type ExportedEscalationStep struct {
	ID           int64 `json:"id"`
	PolicyID     int64 `json:"policy_id"`
	Position     int   `json:"position"`
	DelaySeconds int64 `json:"delay_seconds"`
}

// ExportedEscalationStepChannel represents one row from escalation_step_channels.
type ExportedEscalationStepChannel struct {
	StepID    int64 `json:"step_id"`
	ChannelID int64 `json:"channel_id"`
}

// ExportedAPIToken represents one row from api_tokens. TokenHash is redacted
// when IncludeSecrets is false. Raw bearer token values are never stored.
type ExportedAPIToken struct {
	ID                   int64      `json:"id"`
	WorkspaceID          string     `json:"workspace_id"`
	Name                 string     `json:"name"`
	TokenHash            string     `json:"token_hash"`
	Prefix               string     `json:"prefix"`
	Scopes               []string   `json:"scopes"`
	ExpiresAt            *time.Time `json:"expires_at,omitempty"`
	LastUsedAt           *time.Time `json:"last_used_at,omitempty"`
	RevokedAt            *time.Time `json:"revoked_at,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	RotatedFromID        *int64     `json:"rotated_from_id,omitempty"`
	SuccessorID          *int64     `json:"successor_id,omitempty"`
	RotatedAt            *time.Time `json:"rotated_at,omitempty"`
	PreRotationExpiresAt *time.Time `json:"pre_rotation_expires_at,omitempty"`
}

// ExportedUsageRollup represents one row from workspace_usage_rollups.
type ExportedUsageRollup struct {
	WorkspaceID     string    `json:"workspace_id"`
	PeriodStart     time.Time `json:"period_start"`
	PeriodEnd       time.Time `json:"period_end"`
	AlertsRaised    int64     `json:"alerts_raised"`
	RuleEvaluations int64     `json:"rule_evaluations"`
	APIRequests     int64     `json:"api_requests"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// ExportedNotificationUsageRollup represents one row from workspace_notification_usage_rollups.
type ExportedNotificationUsageRollup struct {
	WorkspaceID string    `json:"workspace_id"`
	PeriodStart time.Time `json:"period_start"`
	PeriodEnd   time.Time `json:"period_end"`
	ChannelType string    `json:"channel_type"`
	Outcome     string    `json:"outcome"`
	Count       int64     `json:"count"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ExportedAlertDedup represents one row from alert_dedup.
type ExportedAlertDedup struct {
	RuleID    int64     `json:"rule_id"`
	EventID   string    `json:"event_id"`
	AlertID   int64     `json:"alert_id"`
	CreatedAt time.Time `json:"created_at"`
}

// ExportedAlertEscalation represents one row from alert_escalations.
type ExportedAlertEscalation struct {
	AlertID        int64           `json:"alert_id"`
	AlertCreatedAt time.Time       `json:"alert_created_at"`
	PolicyID       int64           `json:"policy_id"`
	AlertSnapshot  json.RawMessage `json:"alert_snapshot,omitempty"`
	NextStep       int             `json:"next_step"`
	NextDueAt      time.Time       `json:"next_due_at"`
	CompletedAt    *time.Time      `json:"completed_at,omitempty"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// WorkspaceExportBundle is the materialized JSON representation of a workspace
// export. WriteWorkspaceExportJSON streams this exact JSON structure directly
// to an io.Writer without buffering all rows in memory.
type WorkspaceExportBundle struct {
	Manifest                          WorkspaceExportManifest           `json:"manifest"`
	Format                            string                            `json:"format"`
	Version                           int                               `json:"version"`
	WorkspaceID                       string                            `json:"workspace_id"`
	ExportedAt                        time.Time                         `json:"exported_at"`
	IncludeSecrets                    bool                              `json:"include_secrets"`
	SecretsRedacted                   bool                              `json:"secrets_redacted"`
	IncludeAlerts                     bool                              `json:"include_alerts"`
	AlertsIncluded                    bool                              `json:"alerts_included"`
	Workspaces                        []ExportedWorkspace               `json:"workspaces"`
	WorkspaceQuotas                   []WorkspaceQuota                  `json:"workspace_quotas"`
	WorkspaceQueryBudgetState         []ExportedQueryBudgetState        `json:"workspace_query_budget_state"`
	WorkspaceRetentionPolicies        []ExportedWorkspaceRetentionPolicy `json:"workspace_retention_policies"`
	Channels                          []ExportedChannel                 `json:"channels"`
	ChannelSigningKeys                []ExportedChannelSigningKey       `json:"channel_signing_keys"`
	ChannelAckReconciliations         []ChannelAckReconciliation        `json:"channel_ack_reconciliations"`
	PendingDigests                    []DigestAlert                     `json:"pending_digests"`
	Monitors                          []Monitor                         `json:"monitors"`
	MonitorChannels                   []ExportedMonitorChannel          `json:"monitor_channels"`
	Rules                             []ExportedRule                    `json:"rules"`
	RuleAbsenceState                  []ExportedRuleAbsenceState        `json:"rule_absence_state"`
	AlertInhibitions                  []Inhibition                      `json:"alert_inhibitions"`
	EscalationPolicies                []EscalationPolicy                `json:"escalation_policies"`
	EscalationSteps                   []ExportedEscalationStep          `json:"escalation_steps"`
	EscalationStepChannels            []ExportedEscalationStepChannel   `json:"escalation_step_channels"`
	MaintenanceWindows                []MaintenanceWindow               `json:"maintenance_windows"`
	Backfills                         []Backfill                        `json:"backfills"`
	SavedSearches                     []SavedSearch                     `json:"saved_searches"`
	MonitorTemplates                  []MonitorTemplate                 `json:"monitor_templates"`
	APITokens                         []ExportedAPIToken                `json:"api_tokens"`
	WorkspaceUsageRollups             []ExportedUsageRollup             `json:"workspace_usage_rollups"`
	WorkspaceNotificationUsageRollups []ExportedNotificationUsageRollup `json:"workspace_notification_usage_rollups"`
	Alerts                            []Alert                           `json:"alerts"`
	AlertDedup                        []ExportedAlertDedup              `json:"alert_dedup"`
	DeliveryAttempts                  []DeliveryAttempt                 `json:"delivery_attempts"`
	DeadLetters                       []DeadLetter                      `json:"dead_letters"`
	AlertEscalations                  []ExportedAlertEscalation         `json:"alert_escalations"`
	Summary                           WorkspaceExportSummary            `json:"summary"`
}

// sensitiveConfigKeyNames lists exact lowercase JSON property names inside a
// channel configuration object that hold credentials, webhook URLs, or keys.
var sensitiveConfigKeyNames = map[string]bool{
	"url":               true,
	"webhook_url":       true,
	"events_url":        true,
	"secret":            true,
	"previous_secret":   true,
	"signing_secret":    true,
	"token":             true,
	"bot_token":         true,
	"auth_token":        true,
	"access_token":      true,
	"app_token":         true,
	"password":          true,
	"pass":              true,
	"api_key":           true,
	"apikey":            true,
	"routing_key":       true,
	"user_key":          true,
	"user":              true,
	"private_key":       true,
	"secret_access_key": true,
	"access_key_id":     true,
	"authorization":     true,
}

func isSensitiveConfigKey(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	if sensitiveConfigKeyNames[lower] {
		return true
	}
	for _, sub := range []string{
		"secret",
		"token",
		"password",
		"api_key",
		"apikey",
		"routing_key",
		"webhook_url",
		"private_key",
		"access_key",
	} {
		if strings.Contains(lower, sub) {
			return true
		}
	}
	return false
}

func redactJSONValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, child := range val {
			if strings.EqualFold(k, "headers") {
				if hdrs, ok := child.(map[string]any); ok {
					redactedHdrs := make(map[string]any, len(hdrs))
					for hk := range hdrs {
						redactedHdrs[hk] = RedactedSecretPlaceholder
					}
					out[k] = redactedHdrs
					continue
				}
			}
			if isSensitiveConfigKey(k) {
				out[k] = RedactedSecretPlaceholder
				continue
			}
			out[k] = redactJSONValue(child)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, elem := range val {
			out[i] = redactJSONValue(elem)
		}
		return out
	default:
		return val
	}
}

// RedactChannelConfig returns a copy of raw with every sensitive field (such as
// webhook URLs, tokens, secrets, passwords, API keys, and HTTP headers)
// replaced by "REDACTED" while preserving non-sensitive structural settings.
func RedactChannelConfig(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return json.RawMessage(`{"redacted":"` + RedactedSecretPlaceholder + `"}`)
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return json.RawMessage(`{"redacted":"` + RedactedSecretPlaceholder + `"}`)
	}
	redacted := redactJSONValue(obj)
	b, err := json.Marshal(redacted)
	if err != nil {
		return json.RawMessage(`{"redacted":"` + RedactedSecretPlaceholder + `"}`)
	}
	return json.RawMessage(b)
}

func prepareExportChannelConfig(cipher ConfigCipher, channelID int64, channelName string, raw json.RawMessage, includeSecrets bool) (json.RawMessage, error) {
	ch := Channel{
		ID:     channelID,
		Name:   channelName,
		Config: raw,
	}
	if err := decryptChannel(cipher, &ch); err != nil {
		if !includeSecrets {
			return json.RawMessage(`{"redacted":"` + RedactedSecretPlaceholder + `"}`), nil
		}
		return nil, err
	}
	if !includeSecrets {
		return RedactChannelConfig(ch.Config), nil
	}
	if len(ch.Config) == 0 {
		return json.RawMessage(`{}`), nil
	}
	return ch.Config, nil
}

// WriteWorkspaceExportNDJSON streams a workspace export as newline-delimited
// JSON records (manifest, rows in deterministic table and primary-key order,
// and summary) directly to w.
func WriteWorkspaceExportNDJSON(ctx context.Context, w io.Writer, st Store, ws workspace.ID, opts WorkspaceExportOptions) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return st.ExportWorkspace(ctx, ws, opts, func(rec WorkspaceExportRecord) error {
		if err := enc.Encode(rec); err != nil {
			return err
		}
		if f, ok := w.(interface{ Flush() }); ok {
			f.Flush()
		}
		return nil
	})
}

// WriteWorkspaceExportJSON streams a workspace export as a single versioned,
// self-describing JSON document directly to w without buffering alert history
// in memory.
func WriteWorkspaceExportJSON(ctx context.Context, w io.Writer, st Store, ws workspace.ID, opts WorkspaceExportOptions) error {
	allTables := WorkspaceExportTables()
	tableIdx := -1
	var rowInCurrentTable int64

	marshalBytes := func(v any) ([]byte, error) {
		var sb strings.Builder
		enc := json.NewEncoder(&sb)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			return nil, err
		}
		return []byte(strings.TrimSuffix(sb.String(), "\n")), nil
	}

	writeStr := func(s string) error {
		_, err := io.WriteString(w, s)
		return err
	}

	advanceToTable := func(targetIdx int) error {
		for tableIdx < targetIdx {
			if tableIdx >= 0 {
				if err := writeStr("],\n"); err != nil {
					return err
				}
			}
			tableIdx++
			rowInCurrentTable = 0
			if err := writeStr(fmt.Sprintf("%q:[", allTables[tableIdx])); err != nil {
				return err
			}
		}
		return nil
	}

	return st.ExportWorkspace(ctx, ws, opts, func(rec WorkspaceExportRecord) error {
		switch rec.Kind {
		case WorkspaceExportKindManifest:
			m := rec.Manifest
			if m == nil {
				return errors.New("missing export manifest")
			}
			mb, err := marshalBytes(m)
			if err != nil {
				return err
			}
			tsBytes, err := marshalBytes(m.ExportedAt)
			if err != nil {
				return err
			}
			header := fmt.Sprintf(
				"{\"manifest\":%s,\n\"format\":%q,\n\"version\":%d,\n\"workspace_id\":%q,\n\"exported_at\":%s,\n\"include_secrets\":%t,\n\"secrets_redacted\":%t,\n\"include_alerts\":%t,\n\"alerts_included\":%t,\n",
				string(mb),
				m.Format,
				m.Version,
				m.WorkspaceID,
				string(tsBytes),
				m.IncludeSecrets,
				m.SecretsRedacted,
				m.IncludeAlerts,
				m.AlertsIncluded,
			)
			if err := writeStr(header); err != nil {
				return err
			}
		case WorkspaceExportKindRow:
			idx := slices.Index(allTables, rec.Table)
			if idx < 0 {
				return fmt.Errorf("unknown export table %q", rec.Table)
			}
			if idx < tableIdx {
				return fmt.Errorf("out-of-order export table %q after %q", rec.Table, allTables[tableIdx])
			}
			if err := advanceToTable(idx); err != nil {
				return err
			}
			if rowInCurrentTable > 0 {
				if err := writeStr(","); err != nil {
					return err
				}
			}
			rb, err := marshalBytes(rec.Data)
			if err != nil {
				return err
			}
			if err := writeStr(string(rb)); err != nil {
				return err
			}
			rowInCurrentTable++
		case WorkspaceExportKindSummary:
			if err := advanceToTable(len(allTables) - 1); err != nil {
				return err
			}
			if tableIdx >= 0 {
				if err := writeStr("],\n"); err != nil {
					return err
				}
			}
			sb, err := marshalBytes(rec.Summary)
			if err != nil {
				return err
			}
			if err := writeStr(fmt.Sprintf("\"summary\":%s}\n", string(sb))); err != nil {
				return err
			}
		}
		if f, ok := w.(interface{ Flush() }); ok {
			f.Flush()
		}
		return nil
	})
}

// ExportWorkspaceBundle collects a streaming workspace export into a typed
// WorkspaceExportBundle struct for callers and tests that inspect the export in
// memory.
func ExportWorkspaceBundle(ctx context.Context, st Store, ws workspace.ID, opts WorkspaceExportOptions) (*WorkspaceExportBundle, error) {
	var buf strings.Builder
	if err := WriteWorkspaceExportJSON(ctx, &buf, st, ws, opts); err != nil {
		return nil, err
	}
	var bundle WorkspaceExportBundle
	if err := json.Unmarshal([]byte(buf.String()), &bundle); err != nil {
		return nil, fmt.Errorf("unmarshal workspace export bundle: %w", err)
	}
	return &bundle, nil
}

// --- Postgres implementation ---

// ExportWorkspace streams every record belonging to workspace ws in
// deterministic table and primary-key order.
func (p *Postgres) ExportWorkspace(ctx context.Context, ws workspace.ID, opts WorkspaceExportOptions, emit func(WorkspaceExportRecord) error) error {
	if !workspace.Valid(string(ws)) {
		return fmt.Errorf("invalid workspace id %q", ws)
	}
	exportedAt := opts.effectiveExportedAt()
	pageSize := opts.effectivePageSize()
	tables := WorkspaceExportTablesForOptions(opts)

	manifest := &WorkspaceExportManifest{
		Format:          WorkspaceExportFormat,
		Version:         WorkspaceExportVersion,
		WorkspaceID:     string(ws),
		ExportedAt:      exportedAt,
		IncludeSecrets:  opts.IncludeSecrets,
		SecretsRedacted: !opts.IncludeSecrets,
		IncludeAlerts:   opts.IncludeAlerts,
		AlertsIncluded:  opts.IncludeAlerts,
		Tables:          tables,
	}
	if err := emit(WorkspaceExportRecord{
		Kind:     WorkspaceExportKindManifest,
		Manifest: manifest,
	}); err != nil {
		return err
	}

	counts := make(map[string]int64, len(tables))
	for _, tbl := range tables {
		counts[tbl] = 0
	}
	var totalRows int64
	emitRow := func(table string, data any) error {
		counts[table]++
		totalRows++
		return emit(WorkspaceExportRecord{
			Kind:  WorkspaceExportKindRow,
			Table: table,
			Data:  data,
		})
	}

	wsStr := string(ws)
	if err := p.exportPostgresWorkspaces(ctx, wsStr, exportedAt, emitRow); err != nil {
		return err
	}
	if err := p.exportPostgresQuotas(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := p.exportPostgresQueryBudgetState(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := p.exportPostgresRetentionPolicies(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := p.exportPostgresChannels(ctx, wsStr, opts.IncludeSecrets, emitRow); err != nil {
		return err
	}
	if err := p.exportPostgresChannelSigningKeys(ctx, wsStr, opts.IncludeSecrets, emitRow); err != nil {
		return err
	}
	if err := p.exportPostgresChannelAckReconciliations(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := p.exportPostgresPendingDigests(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := p.exportPostgresMonitors(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := p.exportPostgresMonitorChannels(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := p.exportPostgresRules(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := p.exportPostgresRuleAbsenceState(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := p.exportPostgresAlertInhibitions(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := p.exportPostgresEscalationPolicies(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := p.exportPostgresEscalationSteps(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := p.exportPostgresEscalationStepChannels(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := p.exportPostgresMaintenanceWindows(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := p.exportPostgresBackfills(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := p.exportPostgresSavedSearches(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := p.exportPostgresMonitorTemplates(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := p.exportPostgresAPITokens(ctx, wsStr, opts.IncludeSecrets, emitRow); err != nil {
		return err
	}
	if err := p.exportPostgresUsageRollups(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := p.exportPostgresNotificationUsageRollups(ctx, wsStr, emitRow); err != nil {
		return err
	}

	if opts.IncludeAlerts {
		if err := p.exportPostgresAlertsPaged(ctx, wsStr, pageSize, emitRow); err != nil {
			return err
		}
		if err := p.exportPostgresAlertDedupPaged(ctx, wsStr, pageSize, emitRow); err != nil {
			return err
		}
		if err := p.exportPostgresDeliveryAttemptsPaged(ctx, wsStr, pageSize, emitRow); err != nil {
			return err
		}
		if err := p.exportPostgresDeadLettersPaged(ctx, wsStr, pageSize, emitRow); err != nil {
			return err
		}
		if err := p.exportPostgresAlertEscalationsPaged(ctx, wsStr, pageSize, emitRow); err != nil {
			return err
		}
	}

	return emit(WorkspaceExportRecord{
		Kind: WorkspaceExportKindSummary,
		Summary: &WorkspaceExportSummary{
			TableCounts: counts,
			TotalRows:   totalRows,
		},
	})
}

func (p *Postgres) exportPostgresWorkspaces(ctx context.Context, ws string, fallbackCreatedAt time.Time, emitRow func(string, any) error) error {
	var row ExportedWorkspace
	err := p.pool.QueryRow(ctx,
		`SELECT id, name, default_runbook_url, default_response_guidance, created_at
		   FROM workspaces WHERE id = $1`, ws,
	).Scan(&row.ID, &row.Name, &row.DefaultRunbookURL, &row.DefaultResponseGuidance, &row.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		row = ExportedWorkspace{
			ID:        ws,
			Name:      ws,
			CreatedAt: fallbackCreatedAt,
		}
		return emitRow("workspaces", row)
	}
	if err != nil {
		return mapErr(err)
	}
	row.CreatedAt = row.CreatedAt.UTC()
	return emitRow("workspaces", row)
}

func (p *Postgres) exportPostgresQuotas(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := p.pool.Query(ctx,
		`SELECT workspace_id, max_monitors, max_rules, max_channels, max_subscriptions,
		        query_cost_budget, query_cost_refill_per_sec, updated_at
		   FROM workspace_quotas
		  WHERE workspace_id = $1
		  ORDER BY workspace_id ASC`, ws)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var q WorkspaceQuota
		if err := rows.Scan(
			&q.WorkspaceID,
			&q.MaxMonitors,
			&q.MaxRules,
			&q.MaxChannels,
			&q.MaxSubscriptions,
			&q.QueryCostBudget,
			&q.QueryCostRefillPerSec,
			&q.UpdatedAt,
		); err != nil {
			return mapErr(err)
		}
		q.UpdatedAt = q.UpdatedAt.UTC()
		if err := emitRow("workspace_quotas", q); err != nil {
			return err
		}
	}
	return mapErr(rows.Err())
}

func (p *Postgres) exportPostgresQueryBudgetState(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := p.pool.Query(ctx,
		`SELECT workspace_id, tokens, updated_at
		   FROM workspace_query_budget_state
		  WHERE workspace_id = $1
		  ORDER BY workspace_id ASC`, ws)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var st ExportedQueryBudgetState
		if err := rows.Scan(&st.WorkspaceID, &st.Tokens, &st.UpdatedAt); err != nil {
			return mapErr(err)
		}
		st.UpdatedAt = st.UpdatedAt.UTC()
		if err := emitRow("workspace_query_budget_state", st); err != nil {
			return err
		}
	}
	return mapErr(rows.Err())
}

func (p *Postgres) exportPostgresRetentionPolicies(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := p.pool.Query(ctx,
		`SELECT workspace_id, retention_seconds, updated_by, updated_at
		   FROM workspace_retention_policies
		  WHERE workspace_id = $1
		  ORDER BY workspace_id ASC`, ws)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var pol ExportedWorkspaceRetentionPolicy
		if err := rows.Scan(&pol.WorkspaceID, &pol.RetentionSeconds, &pol.UpdatedBy, &pol.UpdatedAt); err != nil {
			return mapErr(err)
		}
		pol.UpdatedAt = pol.UpdatedAt.UTC()
		if err := emitRow("workspace_retention_policies", pol); err != nil {
			return err
		}
	}
	return mapErr(rows.Err())
}

func (p *Postgres) exportPostgresChannels(ctx context.Context, ws string, includeSecrets bool, emitRow func(string, any) error) error {
	rows, err := p.pool.Query(ctx,
		`SELECT id, workspace_id, name, type, config, enabled, created_at,
		        digest_mode, digest_window_seconds, COALESCE(min_severity, ''),
		        timeout,
		        consecutive_failures, consecutive_permanent_failures, last_error,
		        last_error_at, last_success_at, disabled_at
		   FROM channels
		  WHERE workspace_id = $1
		  ORDER BY id ASC`, ws)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var ch ExportedChannel
		var rawCfg []byte
		var minSev string
		if err := rows.Scan(
			&ch.ID,
			&ch.WorkspaceID,
			&ch.Name,
			&ch.Type,
			&rawCfg,
			&ch.Enabled,
			&ch.CreatedAt,
			&ch.DigestMode,
			&ch.DigestWindowSeconds,
			&minSev,
			&ch.TimeoutSeconds,
			&ch.ConsecutiveFailures,
			&ch.ConsecutivePermanentFailures,
			&ch.LastError,
			&ch.LastErrorAt,
			&ch.LastSuccessAt,
			&ch.DisabledAt,
		); err != nil {
			return mapErr(err)
		}
		ch.MinSeverity = Severity(minSev)
		ch.CreatedAt = ch.CreatedAt.UTC()
		cfg, err := prepareExportChannelConfig(p.cipher, ch.ID, ch.Name, json.RawMessage(rawCfg), includeSecrets)
		if err != nil {
			return err
		}
		ch.Config = cfg
		if err := emitRow("channels", ch); err != nil {
			return err
		}
	}
	return mapErr(rows.Err())
}

func (p *Postgres) exportPostgresChannelSigningKeys(ctx context.Context, ws string, includeSecrets bool, emitRow func(string, any) error) error {
	rows, err := p.pool.Query(ctx,
		`SELECT csk.id, csk.channel_id, csk.workspace_id, csk.key_id, csk.secret,
		        csk.is_current, csk.status, csk.created_at, csk.expires_at, csk.last_used_at, csk.revoked_at
		   FROM channel_signing_keys csk
		   JOIN channels c ON c.id = csk.channel_id
		  WHERE csk.workspace_id = $1 AND c.workspace_id = $1
		  ORDER BY csk.id ASC`, ws)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k ExportedChannelSigningKey
		var storedSecret string
		if err := rows.Scan(
			&k.ID,
			&k.ChannelID,
			&k.WorkspaceID,
			&k.KeyID,
			&storedSecret,
			&k.IsCurrent,
			&k.Status,
			&k.CreatedAt,
			&k.ExpiresAt,
			&k.LastUsedAt,
			&k.RevokedAt,
		); err != nil {
			return mapErr(err)
		}
		k.CreatedAt = k.CreatedAt.UTC()
		if includeSecrets {
			dec, err := p.decryptSigningSecret(storedSecret)
			if err != nil {
				return err
			}
			k.Secret = dec
		} else {
			k.Secret = RedactedSecretPlaceholder
		}
		if err := emitRow("channel_signing_keys", k); err != nil {
			return err
		}
	}
	return mapErr(rows.Err())
}

func (p *Postgres) exportPostgresChannelAckReconciliations(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := p.pool.Query(ctx,
		`SELECT car.channel_id, car.workspace_id, car.delivered_total, car.acknowledged_total,
		        car.unacknowledged_total, car.unack_rate, car.flagged, car.computed_at
		   FROM channel_ack_reconciliations car
		   JOIN channels c ON c.id = car.channel_id
		  WHERE car.workspace_id = $1 AND c.workspace_id = $1
		  ORDER BY car.channel_id ASC`, ws)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var r ChannelAckReconciliation
		if err := rows.Scan(
			&r.ChannelID,
			&r.WorkspaceID,
			&r.DeliveredTotal,
			&r.AcknowledgedTotal,
			&r.UnacknowledgedTotal,
			&r.UnackRate,
			&r.Flagged,
			&r.ComputedAt,
		); err != nil {
			return mapErr(err)
		}
		r.ComputedAt = r.ComputedAt.UTC()
		if err := emitRow("channel_ack_reconciliations", r); err != nil {
			return err
		}
	}
	return mapErr(rows.Err())
}

func (p *Postgres) exportPostgresPendingDigests(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := p.pool.Query(ctx,
		`SELECT pd.id, pd.channel_id, pd.payload, pd.created_at
		   FROM pending_digests pd
		   JOIN channels c ON c.id = pd.channel_id
		  WHERE c.workspace_id = $1
		  ORDER BY pd.id ASC`, ws)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var d DigestAlert
		if err := rows.Scan(&d.ID, &d.ChannelID, &d.Payload, &d.CreatedAt); err != nil {
			return mapErr(err)
		}
		d.CreatedAt = d.CreatedAt.UTC()
		if err := emitRow("pending_digests", d); err != nil {
			return err
		}
	}
	return mapErr(rows.Err())
}

func (p *Postgres) exportPostgresMonitors(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := p.pool.Query(ctx,
		`SELECT id, name, contract_ids, tags, enabled, created_at, last_matched_at,
		        priority, network, deleted_at, runbook_url, response_guidance, workspace_id
		   FROM monitors
		  WHERE workspace_id = $1
		  ORDER BY id ASC`, ws)
	if err != nil {
		return mapErr(err)
	}
	var monitors []Monitor
	for rows.Next() {
		m, err := scanMonitor(rows)
		if err != nil {
			rows.Close()
			return err
		}
		m.CreatedAt = m.CreatedAt.UTC()
		monitors = append(monitors, *m)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return mapErr(err)
	}
	rows.Close()

	for i := range monitors {
		cids, err := p.exportPostgresMonitorChannelIDs(ctx, monitors[i].ID, ws)
		if err != nil {
			return err
		}
		monitors[i].ChannelIDs = cids
		if err := emitRow("monitors", monitors[i]); err != nil {
			return err
		}
	}
	return nil
}

func (p *Postgres) exportPostgresMonitorChannelIDs(ctx context.Context, monitorID int64, ws string) ([]int64, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT mc.channel_id
		   FROM monitor_channels mc
		   JOIN monitors m ON m.id = mc.monitor_id
		   JOIN channels c ON c.id = mc.channel_id
		  WHERE mc.monitor_id = $1 AND m.workspace_id = $2 AND c.workspace_id = $2
		  ORDER BY mc.channel_id ASC`, monitorID, ws)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, mapErr(err)
		}
		out = append(out, id)
	}
	return out, mapErr(rows.Err())
}

func (p *Postgres) exportPostgresMonitorChannels(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := p.pool.Query(ctx,
		`SELECT mc.monitor_id, mc.channel_id
		   FROM monitor_channels mc
		   JOIN monitors m ON m.id = mc.monitor_id
		   JOIN channels c ON c.id = mc.channel_id
		  WHERE m.workspace_id = $1 AND c.workspace_id = $1
		  ORDER BY mc.monitor_id ASC, mc.channel_id ASC`, ws)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var mc ExportedMonitorChannel
		if err := rows.Scan(&mc.MonitorID, &mc.ChannelID); err != nil {
			return mapErr(err)
		}
		if err := emitRow("monitor_channels", mc); err != nil {
			return err
		}
	}
	return mapErr(rows.Err())
}

func (p *Postgres) exportPostgresRules(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := p.pool.Query(ctx,
		`SELECT r.id, r.monitor_id, r.type, r.params, r.enabled, COALESCE(r.severity, 'warning'),
		        r.version, r.runbook_url, r.response_guidance, r.last_alert_at, r.suppressed_since_last
		   FROM rules r
		   JOIN monitors m ON m.id = r.monitor_id
		  WHERE m.workspace_id = $1
		  ORDER BY r.id ASC`, ws)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var r ExportedRule
		var sev string
		if err := rows.Scan(
			&r.ID,
			&r.MonitorID,
			&r.Type,
			&r.Params,
			&r.Enabled,
			&sev,
			&r.Version,
			&r.RunbookURL,
			&r.ResponseGuidance,
			&r.LastAlertAt,
			&r.SuppressedSinceLast,
		); err != nil {
			return mapErr(err)
		}
		r.Severity = Severity(sev)
		if err := emitRow("rules", r); err != nil {
			return err
		}
	}
	return mapErr(rows.Err())
}

func (p *Postgres) exportPostgresRuleAbsenceState(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := p.pool.Query(ctx,
		`SELECT ras.rule_id, ras.event_name, ras.last_seen_at, ras.updated_at
		   FROM rule_absence_state ras
		   JOIN rules r ON r.id = ras.rule_id
		   JOIN monitors m ON m.id = r.monitor_id
		  WHERE m.workspace_id = $1
		  ORDER BY ras.rule_id ASC, ras.event_name ASC`, ws)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var st ExportedRuleAbsenceState
		if err := rows.Scan(&st.RuleID, &st.EventName, &st.LastSeenAt, &st.UpdatedAt); err != nil {
			return mapErr(err)
		}
		st.LastSeenAt = st.LastSeenAt.UTC()
		st.UpdatedAt = st.UpdatedAt.UTC()
		if err := emitRow("rule_absence_state", st); err != nil {
			return err
		}
	}
	return mapErr(rows.Err())
}

func (p *Postgres) exportPostgresAlertInhibitions(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := p.pool.Query(ctx,
		`SELECT ai.source_rule_id, ai.target_rule_id, ai.firing_window_seconds, ai.created_at
		   FROM alert_inhibitions ai
		   JOIN rules sr ON sr.id = ai.source_rule_id
		   JOIN monitors sm ON sm.id = sr.monitor_id
		   JOIN rules tr ON tr.id = ai.target_rule_id
		   JOIN monitors tm ON tm.id = tr.monitor_id
		  WHERE sm.workspace_id = $1 AND tm.workspace_id = $1
		  ORDER BY ai.source_rule_id ASC, ai.target_rule_id ASC`, ws)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var in Inhibition
		if err := rows.Scan(&in.SourceRuleID, &in.TargetRuleID, &in.FiringWindowSeconds, &in.CreatedAt); err != nil {
			return mapErr(err)
		}
		in.CreatedAt = in.CreatedAt.UTC()
		if err := emitRow("alert_inhibitions", in); err != nil {
			return err
		}
	}
	return mapErr(rows.Err())
}

func (p *Postgres) exportPostgresEscalationPolicies(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := p.pool.Query(ctx,
		`SELECT ep.id, ep.monitor_id, ep.created_at, ep.updated_at
		   FROM escalation_policies ep
		   JOIN monitors m ON m.id = ep.monitor_id
		  WHERE m.workspace_id = $1
		  ORDER BY ep.id ASC`, ws)
	if err != nil {
		return mapErr(err)
	}
	var policies []EscalationPolicy
	for rows.Next() {
		var ep EscalationPolicy
		if err := rows.Scan(&ep.ID, &ep.MonitorID, &ep.CreatedAt, &ep.UpdatedAt); err != nil {
			rows.Close()
			return mapErr(err)
		}
		ep.CreatedAt = ep.CreatedAt.UTC()
		ep.UpdatedAt = ep.UpdatedAt.UTC()
		policies = append(policies, ep)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return mapErr(err)
	}
	rows.Close()

	for i := range policies {
		steps, err := p.listEscalationSteps(ctx, policies[i].ID)
		if err != nil {
			return err
		}
		policies[i].Steps = steps
		if err := emitRow("escalation_policies", policies[i]); err != nil {
			return err
		}
	}
	return nil
}

func (p *Postgres) exportPostgresEscalationSteps(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := p.pool.Query(ctx,
		`SELECT es.id, es.policy_id, es.position, es.delay_seconds
		   FROM escalation_steps es
		   JOIN escalation_policies ep ON ep.id = es.policy_id
		   JOIN monitors m ON m.id = ep.monitor_id
		  WHERE m.workspace_id = $1
		  ORDER BY es.id ASC`, ws)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s ExportedEscalationStep
		if err := rows.Scan(&s.ID, &s.PolicyID, &s.Position, &s.DelaySeconds); err != nil {
			return mapErr(err)
		}
		if err := emitRow("escalation_steps", s); err != nil {
			return err
		}
	}
	return mapErr(rows.Err())
}

func (p *Postgres) exportPostgresEscalationStepChannels(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := p.pool.Query(ctx,
		`SELECT esc.step_id, esc.channel_id
		   FROM escalation_step_channels esc
		   JOIN escalation_steps es ON es.id = esc.step_id
		   JOIN escalation_policies ep ON ep.id = es.policy_id
		   JOIN monitors m ON m.id = ep.monitor_id
		   JOIN channels c ON c.id = esc.channel_id
		  WHERE m.workspace_id = $1 AND c.workspace_id = $1
		  ORDER BY esc.step_id ASC, esc.channel_id ASC`, ws)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var sc ExportedEscalationStepChannel
		if err := rows.Scan(&sc.StepID, &sc.ChannelID); err != nil {
			return mapErr(err)
		}
		if err := emitRow("escalation_step_channels", sc); err != nil {
			return err
		}
	}
	return mapErr(rows.Err())
}

func (p *Postgres) exportPostgresMaintenanceWindows(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := p.pool.Query(ctx,
		`SELECT mw.id, mw.reason, mw.scope, mw.monitor_id, mw.contract_id, mw.start_at, mw.end_at, mw.created_at
		   FROM maintenance_windows mw
		   LEFT JOIN monitors m ON m.id = mw.monitor_id
		  WHERE m.workspace_id = $1
		     OR (mw.scope = 'contract' AND EXISTS (
		           SELECT 1
		             FROM monitors cm, jsonb_array_elements_text(cm.contract_ids) AS cid(val)
		            WHERE cm.workspace_id = $1 AND cid.val = mw.contract_id
		        ))
		  ORDER BY mw.id ASC`, ws)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		mw, err := scanMaintenanceWindow(rows)
		if err != nil {
			return err
		}
		mw.StartAt = mw.StartAt.UTC()
		mw.EndAt = mw.EndAt.UTC()
		mw.CreatedAt = mw.CreatedAt.UTC()
		if err := emitRow("maintenance_windows", *mw); err != nil {
			return err
		}
	}
	return mapErr(rows.Err())
}

func (p *Postgres) exportPostgresBackfills(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := p.pool.Query(ctx,
		`SELECT b.monitor_id, b.from_ledger, b.to_ledger, b.next_ledger, b.cursor, b.deliver, b.complete, b.updated_at
		   FROM backfills b
		   JOIN monitors m ON m.id = b.monitor_id
		  WHERE m.workspace_id = $1
		  ORDER BY b.monitor_id ASC`, ws)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var b Backfill
		var fromL, toL, nextL int64
		if err := rows.Scan(&b.MonitorID, &fromL, &toL, &nextL, &b.Cursor, &b.Deliver, &b.Complete, &b.UpdatedAt); err != nil {
			return mapErr(err)
		}
		b.FromLedger = uint32(fromL)
		b.ToLedger = uint32(toL)
		b.NextLedger = uint32(nextL)
		b.UpdatedAt = b.UpdatedAt.UTC()
		if err := emitRow("backfills", b); err != nil {
			return err
		}
	}
	return mapErr(rows.Err())
}

func (p *Postgres) exportPostgresSavedSearches(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := p.pool.Query(ctx,
		`SELECT id, name, filter, is_default, created_at
		   FROM saved_searches
		  WHERE workspace_id = $1
		  ORDER BY id ASC`, ws)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var ss SavedSearch
		var rawFilter []byte
		if err := rows.Scan(&ss.ID, &ss.Name, &rawFilter, &ss.IsDefault, &ss.CreatedAt); err != nil {
			return mapErr(err)
		}
		_ = json.Unmarshal(rawFilter, &ss.Filter)
		ss.CreatedAt = ss.CreatedAt.UTC()
		if err := emitRow("saved_searches", ss); err != nil {
			return err
		}
	}
	return mapErr(rows.Err())
}

func (p *Postgres) exportPostgresMonitorTemplates(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := p.pool.Query(ctx,
		`SELECT id, name, description, rules, channel_ids, parameters, created_at
		   FROM monitor_templates
		  WHERE workspace_id = $1
		  ORDER BY id ASC`, ws)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var t MonitorTemplate
		var rulesJSON, paramsJSON []byte
		if err := rows.Scan(&t.ID, &t.Name, &t.Description, &rulesJSON, &t.ChannelIDs, &paramsJSON, &t.CreatedAt); err != nil {
			return mapErr(err)
		}
		_ = json.Unmarshal(rulesJSON, &t.Rules)
		_ = json.Unmarshal(paramsJSON, &t.Parameters)
		if t.ChannelIDs == nil {
			t.ChannelIDs = []int64{}
		}
		t.CreatedAt = t.CreatedAt.UTC()
		if err := emitRow("monitor_templates", t); err != nil {
			return err
		}
	}
	return mapErr(rows.Err())
}

func splitStoredScopes(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{}
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (p *Postgres) exportPostgresAPITokens(ctx context.Context, ws string, includeSecrets bool, emitRow func(string, any) error) error {
	rows, err := p.pool.Query(ctx,
		`SELECT id, workspace_id, name, token_hash, prefix, scopes,
		        expires_at, last_used_at, revoked_at, created_at,
		        rotated_from_id, successor_id, rotated_at, pre_rotation_expires_at
		   FROM api_tokens
		  WHERE workspace_id = $1
		  ORDER BY id ASC`, ws)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var tok ExportedAPIToken
		var scopesRaw string
		if err := rows.Scan(
			&tok.ID,
			&tok.WorkspaceID,
			&tok.Name,
			&tok.TokenHash,
			&tok.Prefix,
			&scopesRaw,
			&tok.ExpiresAt,
			&tok.LastUsedAt,
			&tok.RevokedAt,
			&tok.CreatedAt,
			&tok.RotatedFromID,
			&tok.SuccessorID,
			&tok.RotatedAt,
			&tok.PreRotationExpiresAt,
		); err != nil {
			return mapErr(err)
		}
		tok.Scopes = splitStoredScopes(scopesRaw)
		tok.CreatedAt = tok.CreatedAt.UTC()
		if !includeSecrets {
			tok.TokenHash = RedactedSecretPlaceholder
		}
		if err := emitRow("api_tokens", tok); err != nil {
			return err
		}
	}
	return mapErr(rows.Err())
}

func (p *Postgres) exportPostgresUsageRollups(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := p.pool.Query(ctx,
		`SELECT workspace_id, period_start, period_end, alerts_raised, rule_evaluations, api_requests, updated_at
		   FROM workspace_usage_rollups
		  WHERE workspace_id = $1
		  ORDER BY period_start ASC`, ws)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var r ExportedUsageRollup
		if err := rows.Scan(
			&r.WorkspaceID,
			&r.PeriodStart,
			&r.PeriodEnd,
			&r.AlertsRaised,
			&r.RuleEvaluations,
			&r.APIRequests,
			&r.UpdatedAt,
		); err != nil {
			return mapErr(err)
		}
		r.PeriodStart = r.PeriodStart.UTC()
		r.PeriodEnd = r.PeriodEnd.UTC()
		r.UpdatedAt = r.UpdatedAt.UTC()
		if err := emitRow("workspace_usage_rollups", r); err != nil {
			return err
		}
	}
	return mapErr(rows.Err())
}

func (p *Postgres) exportPostgresNotificationUsageRollups(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := p.pool.Query(ctx,
		`SELECT workspace_id, period_start, period_end, channel_type, outcome, count, updated_at
		   FROM workspace_notification_usage_rollups
		  WHERE workspace_id = $1
		  ORDER BY period_start ASC, channel_type ASC, outcome ASC`, ws)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var r ExportedNotificationUsageRollup
		if err := rows.Scan(
			&r.WorkspaceID,
			&r.PeriodStart,
			&r.PeriodEnd,
			&r.ChannelType,
			&r.Outcome,
			&r.Count,
			&r.UpdatedAt,
		); err != nil {
			return mapErr(err)
		}
		r.PeriodStart = r.PeriodStart.UTC()
		r.PeriodEnd = r.PeriodEnd.UTC()
		r.UpdatedAt = r.UpdatedAt.UTC()
		if err := emitRow("workspace_notification_usage_rollups", r); err != nil {
			return err
		}
	}
	return mapErr(rows.Err())
}

func (p *Postgres) exportPostgresAlertsPaged(ctx context.Context, ws string, pageSize int, emitRow func(string, any) error) error {
	var afterID int64
	for {
		rows, err := p.pool.Query(ctx,
			`SELECT id, monitor_id, rule_id, event_id, payload, enrichment, severity, created_at,
			        ledger, retracted_at, inhibited_by_rule_id, suppressed, suppression_reason,
			        acknowledged_at, acknowledged_by, network, federated, federation_origin,
			        source_monitor_id, source_rule_id, rule_version, runbook_url, response_guidance
			   FROM alerts
			  WHERE workspace_id = $1 AND id > $2
			  ORDER BY id ASC
			  LIMIT $3`, ws, afterID, pageSize)
		if err != nil {
			return mapErr(err)
		}
		var page []Alert
		for rows.Next() {
			var a Alert
			var sev string
			var ledger int64
			var monitorID, ruleID, sourceMonitorID, sourceRuleID *int64
			var origin *string
			if err := rows.Scan(
				&a.ID,
				&monitorID,
				&ruleID,
				&a.EventID,
				&a.Payload,
				&a.Enrichment,
				&sev,
				&a.CreatedAt,
				&ledger,
				&a.RetractedAt,
				&a.InhibitedByRuleID,
				&a.Suppressed,
				&a.SuppressionReason,
				&a.AcknowledgedAt,
				&a.AcknowledgedBy,
				&a.Network,
				&a.Federated,
				&origin,
				&sourceMonitorID,
				&sourceRuleID,
				&a.RuleVersion,
				&a.RunbookURL,
				&a.ResponseGuidance,
			); err != nil {
				rows.Close()
				return mapErr(err)
			}
			if monitorID != nil {
				a.MonitorID = *monitorID
			}
			if ruleID != nil {
				a.RuleID = *ruleID
			}
			if origin != nil {
				a.FederationOrigin = *origin
			}
			if sourceMonitorID != nil {
				a.SourceMonitorID = *sourceMonitorID
			}
			if sourceRuleID != nil {
				a.SourceRuleID = *sourceRuleID
			}
			a.Severity = Severity(sev)
			a.Ledger = uint32(ledger)
			a.CreatedAt = a.CreatedAt.UTC()
			page = append(page, a)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return mapErr(err)
		}
		rows.Close()

		for _, a := range page {
			afterID = a.ID
			if err := emitRow("alerts", a); err != nil {
				return err
			}
		}
		if len(page) < pageSize {
			return nil
		}
	}
}

func (p *Postgres) exportPostgresAlertDedupPaged(ctx context.Context, ws string, pageSize int, emitRow func(string, any) error) error {
	var (
		afterAlertID int64
		afterRuleID  int64
		afterEventID string
	)
	for {
		rows, err := p.pool.Query(ctx,
			`SELECT ad.rule_id, ad.event_id, ad.alert_id, ad.created_at
			   FROM alert_dedup ad
			   JOIN alerts a ON a.id = ad.alert_id
			  WHERE a.workspace_id = $1
			    AND (ad.alert_id > $2
			         OR (ad.alert_id = $2 AND ad.rule_id > $3)
			         OR (ad.alert_id = $2 AND ad.rule_id = $3 AND ad.event_id > $4))
			  ORDER BY ad.alert_id ASC, ad.rule_id ASC, ad.event_id ASC
			  LIMIT $5`, ws, afterAlertID, afterRuleID, afterEventID, pageSize)
		if err != nil {
			return mapErr(err)
		}
		var page []ExportedAlertDedup
		for rows.Next() {
			var d ExportedAlertDedup
			if err := rows.Scan(&d.RuleID, &d.EventID, &d.AlertID, &d.CreatedAt); err != nil {
				rows.Close()
				return mapErr(err)
			}
			d.CreatedAt = d.CreatedAt.UTC()
			page = append(page, d)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return mapErr(err)
		}
		rows.Close()

		for _, d := range page {
			afterAlertID = d.AlertID
			afterRuleID = d.RuleID
			afterEventID = d.EventID
			if err := emitRow("alert_dedup", d); err != nil {
				return err
			}
		}
		if len(page) < pageSize {
			return nil
		}
	}
}

func (p *Postgres) exportPostgresDeliveryAttemptsPaged(ctx context.Context, ws string, pageSize int, emitRow func(string, any) error) error {
	var afterID int64
	for {
		rows, err := p.pool.Query(ctx,
			`SELECT da.id, da.alert_id, da.channel_id, da.status, da.response_snippet, da.attempted_at
			   FROM delivery_attempts da
			   JOIN alerts a ON a.id = da.alert_id
			  WHERE a.workspace_id = $1 AND da.id > $2
			  ORDER BY da.id ASC
			  LIMIT $3`, ws, afterID, pageSize)
		if err != nil {
			return mapErr(err)
		}
		var page []DeliveryAttempt
		for rows.Next() {
			var da DeliveryAttempt
			if err := rows.Scan(&da.ID, &da.AlertID, &da.ChannelID, &da.Status, &da.ResponseSnippet, &da.AttemptedAt); err != nil {
				rows.Close()
				return mapErr(err)
			}
			da.AttemptedAt = da.AttemptedAt.UTC()
			page = append(page, da)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return mapErr(err)
		}
		rows.Close()

		for _, da := range page {
			afterID = da.ID
			if err := emitRow("delivery_attempts", da); err != nil {
				return err
			}
		}
		if len(page) < pageSize {
			return nil
		}
	}
}

func (p *Postgres) exportPostgresDeadLettersPaged(ctx context.Context, ws string, pageSize int, emitRow func(string, any) error) error {
	var afterID int64
	for {
		rows, err := p.pool.Query(ctx,
			`SELECT dl.id, dl.alert_id, dl.channel_id, dl.last_error, dl.attempt_count, dl.last_status, dl.created_at
			   FROM dead_letters dl
			   JOIN alerts a ON a.id = dl.alert_id
			  WHERE a.workspace_id = $1 AND dl.id > $2
			  ORDER BY dl.id ASC
			  LIMIT $3`, ws, afterID, pageSize)
		if err != nil {
			return mapErr(err)
		}
		var page []DeadLetter
		for rows.Next() {
			var dl DeadLetter
			if err := rows.Scan(&dl.ID, &dl.AlertID, &dl.ChannelID, &dl.LastError, &dl.AttemptCount, &dl.LastStatus, &dl.CreatedAt); err != nil {
				rows.Close()
				return mapErr(err)
			}
			dl.CreatedAt = dl.CreatedAt.UTC()
			dl.Cause = ClassifyCause(dl.LastStatus, dl.LastError)
			page = append(page, dl)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return mapErr(err)
		}
		rows.Close()

		for _, dl := range page {
			afterID = dl.ID
			if err := emitRow("dead_letters", dl); err != nil {
				return err
			}
		}
		if len(page) < pageSize {
			return nil
		}
	}
}

func (p *Postgres) exportPostgresAlertEscalationsPaged(ctx context.Context, ws string, pageSize int, emitRow func(string, any) error) error {
	var afterAlertID int64
	for {
		rows, err := p.pool.Query(ctx,
			`SELECT ae.alert_id, ae.alert_created_at, ae.policy_id, ae.alert_snapshot, ae.next_step, ae.next_due_at, ae.completed_at, ae.updated_at
			   FROM alert_escalations ae
			   JOIN alerts a ON a.id = ae.alert_id
			  WHERE a.workspace_id = $1 AND ae.alert_id > $2
			  ORDER BY ae.alert_id ASC
			  LIMIT $3`, ws, afterAlertID, pageSize)
		if err != nil {
			return mapErr(err)
		}
		var page []ExportedAlertEscalation
		for rows.Next() {
			var ae ExportedAlertEscalation
			if err := rows.Scan(
				&ae.AlertID,
				&ae.AlertCreatedAt,
				&ae.PolicyID,
				&ae.AlertSnapshot,
				&ae.NextStep,
				&ae.NextDueAt,
				&ae.CompletedAt,
				&ae.UpdatedAt,
			); err != nil {
				rows.Close()
				return mapErr(err)
			}
			ae.AlertCreatedAt = ae.AlertCreatedAt.UTC()
			ae.NextDueAt = ae.NextDueAt.UTC()
			ae.UpdatedAt = ae.UpdatedAt.UTC()
			page = append(page, ae)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return mapErr(err)
		}
		rows.Close()

		for _, ae := range page {
			afterAlertID = ae.AlertID
			if err := emitRow("alert_escalations", ae); err != nil {
				return err
			}
		}
		if len(page) < pageSize {
			return nil
		}
	}
}

// --- SQLite implementation ---

// ExportWorkspace streams every record belonging to workspace ws in
// deterministic table and primary-key order.
func (s *SQLite) ExportWorkspace(ctx context.Context, ws workspace.ID, opts WorkspaceExportOptions, emit func(WorkspaceExportRecord) error) error {
	if !workspace.Valid(string(ws)) {
		return fmt.Errorf("invalid workspace id %q", ws)
	}
	exportedAt := opts.effectiveExportedAt()
	pageSize := opts.effectivePageSize()
	tables := WorkspaceExportTablesForOptions(opts)

	manifest := &WorkspaceExportManifest{
		Format:          WorkspaceExportFormat,
		Version:         WorkspaceExportVersion,
		WorkspaceID:     string(ws),
		ExportedAt:      exportedAt,
		IncludeSecrets:  opts.IncludeSecrets,
		SecretsRedacted: !opts.IncludeSecrets,
		IncludeAlerts:   opts.IncludeAlerts,
		AlertsIncluded:  opts.IncludeAlerts,
		Tables:          tables,
	}
	if err := emit(WorkspaceExportRecord{
		Kind:     WorkspaceExportKindManifest,
		Manifest: manifest,
	}); err != nil {
		return err
	}

	counts := make(map[string]int64, len(tables))
	for _, tbl := range tables {
		counts[tbl] = 0
	}
	var totalRows int64
	emitRow := func(table string, data any) error {
		counts[table]++
		totalRows++
		return emit(WorkspaceExportRecord{
			Kind:  WorkspaceExportKindRow,
			Table: table,
			Data:  data,
		})
	}

	wsStr := string(ws)
	if err := s.exportSQLiteWorkspaces(ctx, wsStr, exportedAt, emitRow); err != nil {
		return err
	}
	if err := s.exportSQLiteQuotas(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := s.exportSQLiteQueryBudgetState(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := s.exportSQLiteRetentionPolicies(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := s.exportSQLiteChannels(ctx, wsStr, opts.IncludeSecrets, emitRow); err != nil {
		return err
	}
	if err := s.exportSQLiteChannelSigningKeys(ctx, wsStr, opts.IncludeSecrets, emitRow); err != nil {
		return err
	}
	if err := s.exportSQLiteChannelAckReconciliations(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := s.exportSQLitePendingDigests(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := s.exportSQLiteMonitors(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := s.exportSQLiteMonitorChannels(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := s.exportSQLiteRules(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := s.exportSQLiteRuleAbsenceState(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := s.exportSQLiteAlertInhibitions(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := s.exportSQLiteEscalationPolicies(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := s.exportSQLiteEscalationSteps(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := s.exportSQLiteEscalationStepChannels(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := s.exportSQLiteMaintenanceWindows(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := s.exportSQLiteBackfills(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := s.exportSQLiteSavedSearches(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := s.exportSQLiteMonitorTemplates(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := s.exportSQLiteAPITokens(ctx, wsStr, opts.IncludeSecrets, emitRow); err != nil {
		return err
	}
	if err := s.exportSQLiteUsageRollups(ctx, wsStr, emitRow); err != nil {
		return err
	}
	if err := s.exportSQLiteNotificationUsageRollups(ctx, wsStr, emitRow); err != nil {
		return err
	}

	if opts.IncludeAlerts {
		if err := s.exportSQLiteAlertsPaged(ctx, wsStr, pageSize, emitRow); err != nil {
			return err
		}
		if err := s.exportSQLiteAlertDedupPaged(ctx, wsStr, pageSize, emitRow); err != nil {
			return err
		}
		if err := s.exportSQLiteDeliveryAttemptsPaged(ctx, wsStr, pageSize, emitRow); err != nil {
			return err
		}
		if err := s.exportSQLiteDeadLettersPaged(ctx, wsStr, pageSize, emitRow); err != nil {
			return err
		}
		if err := s.exportSQLiteAlertEscalationsPaged(ctx, wsStr, pageSize, emitRow); err != nil {
			return err
		}
	}

	return emit(WorkspaceExportRecord{
		Kind: WorkspaceExportKindSummary,
		Summary: &WorkspaceExportSummary{
			TableCounts: counts,
			TotalRows:   totalRows,
		},
	})
}

func (s *SQLite) exportSQLiteWorkspaces(ctx context.Context, ws string, fallbackCreatedAt time.Time, emitRow func(string, any) error) error {
	var row ExportedWorkspace
	var createdStr string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, default_runbook_url, default_response_guidance, created_at
		   FROM workspaces WHERE id = ?`, ws,
	).Scan(&row.ID, &row.Name, &row.DefaultRunbookURL, &row.DefaultResponseGuidance, &createdStr)
	if errors.Is(err, sql.ErrNoRows) {
		row = ExportedWorkspace{
			ID:        ws,
			Name:      ws,
			CreatedAt: fallbackCreatedAt,
		}
		return emitRow("workspaces", row)
	}
	if err != nil {
		return mapSQLiteErr(err)
	}
	row.CreatedAt, err = parseExportSQLiteTime(createdStr)
	if err != nil {
		return err
	}
	return emitRow("workspaces", row)
}

func (s *SQLite) exportSQLiteQuotas(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT workspace_id, max_monitors, max_rules, max_channels, max_subscriptions,
		        query_cost_budget, query_cost_refill_per_sec, updated_at
		   FROM workspace_quotas
		  WHERE workspace_id = ?
		  ORDER BY workspace_id ASC`, ws)
	if err != nil {
		return mapSQLiteErr(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var q WorkspaceQuota
		var updatedStr string
		if err := rows.Scan(
			&q.WorkspaceID,
			&q.MaxMonitors,
			&q.MaxRules,
			&q.MaxChannels,
			&q.MaxSubscriptions,
			&q.QueryCostBudget,
			&q.QueryCostRefillPerSec,
			&updatedStr,
		); err != nil {
			return mapSQLiteErr(err)
		}
		q.UpdatedAt, err = parseExportSQLiteTime(updatedStr)
		if err != nil {
			return err
		}
		if err := emitRow("workspace_quotas", q); err != nil {
			return err
		}
	}
	return mapSQLiteErr(rows.Err())
}

func (s *SQLite) exportSQLiteQueryBudgetState(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT workspace_id, tokens, updated_at
		   FROM workspace_query_budget_state
		  WHERE workspace_id = ?
		  ORDER BY workspace_id ASC`, ws)
	if err != nil {
		return mapSQLiteErr(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var st ExportedQueryBudgetState
		var updatedStr string
		if err := rows.Scan(&st.WorkspaceID, &st.Tokens, &updatedStr); err != nil {
			return mapSQLiteErr(err)
		}
		st.UpdatedAt, err = parseExportSQLiteTime(updatedStr)
		if err != nil {
			return err
		}
		if err := emitRow("workspace_query_budget_state", st); err != nil {
			return err
		}
	}
	return mapSQLiteErr(rows.Err())
}

func (s *SQLite) exportSQLiteRetentionPolicies(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT workspace_id, retention_seconds, updated_by, updated_at
		   FROM workspace_retention_policies
		  WHERE workspace_id = ?
		  ORDER BY workspace_id ASC`, ws)
	if err != nil {
		return mapSQLiteErr(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var pol ExportedWorkspaceRetentionPolicy
		var updatedStr string
		if err := rows.Scan(&pol.WorkspaceID, &pol.RetentionSeconds, &pol.UpdatedBy, &updatedStr); err != nil {
			return mapSQLiteErr(err)
		}
		pol.UpdatedAt, err = parseExportSQLiteTime(updatedStr)
		if err != nil {
			return err
		}
		if err := emitRow("workspace_retention_policies", pol); err != nil {
			return err
		}
	}
	return mapSQLiteErr(rows.Err())
}

func (s *SQLite) exportSQLiteChannels(ctx context.Context, ws string, includeSecrets bool, emitRow func(string, any) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, workspace_id, name, type, config, enabled, created_at,
		        digest_mode, digest_window_seconds, COALESCE(min_severity, ''), timeout,
		        consecutive_failures, consecutive_permanent_failures, last_error,
		        last_error_at, last_success_at, disabled_at
		   FROM channels
		  WHERE workspace_id = ?
		  ORDER BY id ASC`, ws)
	if err != nil {
		return mapSQLiteErr(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var ch ExportedChannel
		var rawCfg, createdStr, minSev string
		var enabled int64
		var lastErrAt, lastSuccAt, disabledAt sql.NullString
		if err := rows.Scan(
			&ch.ID,
			&ch.WorkspaceID,
			&ch.Name,
			&ch.Type,
			&rawCfg,
			&enabled,
			&createdStr,
			&ch.DigestMode,
			&ch.DigestWindowSeconds,
			&minSev,
			&ch.TimeoutSeconds,
			&ch.ConsecutiveFailures,
			&ch.ConsecutivePermanentFailures,
			&ch.LastError,
			&lastErrAt,
			&lastSuccAt,
			&disabledAt,
		); err != nil {
			return mapSQLiteErr(err)
		}
		ch.Enabled = enabled != 0
		ch.MinSeverity = Severity(minSev)
		if ch.CreatedAt, err = parseExportSQLiteTime(createdStr); err != nil {
			return err
		}
		if ch.LastErrorAt, err = parseExportSQLiteTimePtr(lastErrAt); err != nil {
			return err
		}
		if ch.LastSuccessAt, err = parseExportSQLiteTimePtr(lastSuccAt); err != nil {
			return err
		}
		if ch.DisabledAt, err = parseExportSQLiteTimePtr(disabledAt); err != nil {
			return err
		}
		cfg, err := prepareExportChannelConfig(s.cipher, ch.ID, ch.Name, json.RawMessage(rawCfg), includeSecrets)
		if err != nil {
			return err
		}
		ch.Config = cfg
		if err := emitRow("channels", ch); err != nil {
			return err
		}
	}
	return mapSQLiteErr(rows.Err())
}

func (s *SQLite) exportSQLiteChannelSigningKeys(ctx context.Context, ws string, includeSecrets bool, emitRow func(string, any) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT csk.id, csk.channel_id, csk.workspace_id, csk.key_id, csk.secret,
		        csk.is_current, csk.status, csk.created_at, csk.expires_at, csk.last_used_at, csk.revoked_at
		   FROM channel_signing_keys csk
		   JOIN channels c ON c.id = csk.channel_id
		  WHERE csk.workspace_id = ? AND c.workspace_id = ?
		  ORDER BY csk.id ASC`, ws, ws)
	if err != nil {
		return mapSQLiteErr(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k ExportedChannelSigningKey
		var storedSecret, createdStr string
		var isCurrent int64
		var expiresAt, lastUsedAt, revokedAt sql.NullString
		if err := rows.Scan(
			&k.ID,
			&k.ChannelID,
			&k.WorkspaceID,
			&k.KeyID,
			&storedSecret,
			&isCurrent,
			&k.Status,
			&createdStr,
			&expiresAt,
			&lastUsedAt,
			&revokedAt,
		); err != nil {
			return mapSQLiteErr(err)
		}
		k.IsCurrent = isCurrent != 0
		if k.CreatedAt, err = parseExportSQLiteTime(createdStr); err != nil {
			return err
		}
		if k.ExpiresAt, err = parseExportSQLiteTimePtr(expiresAt); err != nil {
			return err
		}
		if k.LastUsedAt, err = parseExportSQLiteTimePtr(lastUsedAt); err != nil {
			return err
		}
		if k.RevokedAt, err = parseExportSQLiteTimePtr(revokedAt); err != nil {
			return err
		}
		if includeSecrets {
			dec, err := s.decryptSigningSecret(storedSecret)
			if err != nil {
				return err
			}
			k.Secret = dec
		} else {
			k.Secret = RedactedSecretPlaceholder
		}
		if err := emitRow("channel_signing_keys", k); err != nil {
			return err
		}
	}
	return mapSQLiteErr(rows.Err())
}

func (s *SQLite) exportSQLiteChannelAckReconciliations(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT car.channel_id, car.workspace_id, car.delivered_total, car.acknowledged_total,
		        car.unacknowledged_total, car.unack_rate, car.flagged, car.computed_at
		   FROM channel_ack_reconciliations car
		   JOIN channels c ON c.id = car.channel_id
		  WHERE car.workspace_id = ? AND c.workspace_id = ?
		  ORDER BY car.channel_id ASC`, ws, ws)
	if err != nil {
		return mapSQLiteErr(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var r ChannelAckReconciliation
		var rateVal sql.NullFloat64
		var flaggedInt int64
		var computedStr string
		if err := rows.Scan(
			&r.ChannelID,
			&r.WorkspaceID,
			&r.DeliveredTotal,
			&r.AcknowledgedTotal,
			&r.UnacknowledgedTotal,
			&rateVal,
			&flaggedInt,
			&computedStr,
		); err != nil {
			return mapSQLiteErr(err)
		}
		if rateVal.Valid {
			v := rateVal.Float64
			r.UnackRate = &v
		}
		r.Flagged = flaggedInt != 0
		if r.ComputedAt, err = parseExportSQLiteTime(computedStr); err != nil {
			return err
		}
		if err := emitRow("channel_ack_reconciliations", r); err != nil {
			return err
		}
	}
	return mapSQLiteErr(rows.Err())
}

func (s *SQLite) exportSQLitePendingDigests(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT pd.id, pd.channel_id, pd.payload, pd.created_at
		   FROM pending_digests pd
		   JOIN channels c ON c.id = pd.channel_id
		  WHERE c.workspace_id = ?
		  ORDER BY pd.id ASC`, ws)
	if err != nil {
		return mapSQLiteErr(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var d DigestAlert
		var payload, createdStr string
		if err := rows.Scan(&d.ID, &d.ChannelID, &payload, &createdStr); err != nil {
			return mapSQLiteErr(err)
		}
		d.Payload = json.RawMessage(payload)
		if d.CreatedAt, err = parseExportSQLiteTime(createdStr); err != nil {
			return err
		}
		if err := emitRow("pending_digests", d); err != nil {
			return err
		}
	}
	return mapSQLiteErr(rows.Err())
}

func (s *SQLite) exportSQLiteMonitors(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, contract_ids, tags, enabled, created_at, last_matched_at,
		        priority, network, deleted_at, runbook_url, response_guidance, workspace_id
		   FROM monitors
		  WHERE workspace_id = ?
		  ORDER BY id ASC`, ws)
	if err != nil {
		return mapSQLiteErr(err)
	}
	var monitors []Monitor
	for rows.Next() {
		m, err := scanSQLiteMonitor(rows)
		if err != nil {
			_ = rows.Close()
			return err
		}
		monitors = append(monitors, *m)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return mapSQLiteErr(err)
	}
	_ = rows.Close()

	for i := range monitors {
		cids, err := queryInt64Column(ctx, s.db,
			`SELECT mc.channel_id
			   FROM monitor_channels mc
			   JOIN monitors m ON m.id = mc.monitor_id
			   JOIN channels c ON c.id = mc.channel_id
			  WHERE mc.monitor_id = ? AND m.workspace_id = ? AND c.workspace_id = ?
			  ORDER BY mc.channel_id ASC`, monitors[i].ID, ws, ws)
		if err != nil {
			return mapSQLiteErr(err)
		}
		if cids == nil {
			cids = []int64{}
		}
		monitors[i].ChannelIDs = cids
		if err := emitRow("monitors", monitors[i]); err != nil {
			return err
		}
	}
	return nil
}

func (s *SQLite) exportSQLiteMonitorChannels(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT mc.monitor_id, mc.channel_id
		   FROM monitor_channels mc
		   JOIN monitors m ON m.id = mc.monitor_id
		   JOIN channels c ON c.id = mc.channel_id
		  WHERE m.workspace_id = ? AND c.workspace_id = ?
		  ORDER BY mc.monitor_id ASC, mc.channel_id ASC`, ws, ws)
	if err != nil {
		return mapSQLiteErr(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var mc ExportedMonitorChannel
		if err := rows.Scan(&mc.MonitorID, &mc.ChannelID); err != nil {
			return mapSQLiteErr(err)
		}
		if err := emitRow("monitor_channels", mc); err != nil {
			return err
		}
	}
	return mapSQLiteErr(rows.Err())
}

func (s *SQLite) exportSQLiteRules(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT r.id, r.monitor_id, r.type, r.params, r.enabled, COALESCE(r.severity, 'warning'),
		        r.version, r.runbook_url, r.response_guidance, r.last_alert_at, r.suppressed_since_last
		   FROM rules r
		   JOIN monitors m ON m.id = r.monitor_id
		  WHERE m.workspace_id = ?
		  ORDER BY r.id ASC`, ws)
	if err != nil {
		return mapSQLiteErr(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var r ExportedRule
		var params, sev string
		var enabled int64
		var lastAlert sql.NullString
		if err := rows.Scan(
			&r.ID,
			&r.MonitorID,
			&r.Type,
			&params,
			&enabled,
			&sev,
			&r.Version,
			&r.RunbookURL,
			&r.ResponseGuidance,
			&lastAlert,
			&r.SuppressedSinceLast,
		); err != nil {
			return mapSQLiteErr(err)
		}
		r.Enabled = enabled != 0
		r.Params = json.RawMessage(params)
		r.Severity = Severity(sev)
		if r.LastAlertAt, err = parseExportSQLiteTimePtr(lastAlert); err != nil {
			return err
		}
		if err := emitRow("rules", r); err != nil {
			return err
		}
	}
	return mapSQLiteErr(rows.Err())
}

func (s *SQLite) exportSQLiteRuleAbsenceState(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ras.rule_id, ras.event_name, ras.last_seen_at, ras.updated_at
		   FROM rule_absence_state ras
		   JOIN rules r ON r.id = ras.rule_id
		   JOIN monitors m ON m.id = r.monitor_id
		  WHERE m.workspace_id = ?
		  ORDER BY ras.rule_id ASC, ras.event_name ASC`, ws)
	if err != nil {
		return mapSQLiteErr(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var st ExportedRuleAbsenceState
		var lastSeenStr, updatedStr string
		if err := rows.Scan(&st.RuleID, &st.EventName, &lastSeenStr, &updatedStr); err != nil {
			return mapSQLiteErr(err)
		}
		if st.LastSeenAt, err = parseExportSQLiteTime(lastSeenStr); err != nil {
			return err
		}
		if st.UpdatedAt, err = parseExportSQLiteTime(updatedStr); err != nil {
			return err
		}
		if err := emitRow("rule_absence_state", st); err != nil {
			return err
		}
	}
	return mapSQLiteErr(rows.Err())
}

func (s *SQLite) exportSQLiteAlertInhibitions(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ai.source_rule_id, ai.target_rule_id, ai.firing_window_seconds, ai.created_at
		   FROM alert_inhibitions ai
		   JOIN rules sr ON sr.id = ai.source_rule_id
		   JOIN monitors sm ON sm.id = sr.monitor_id
		   JOIN rules tr ON tr.id = ai.target_rule_id
		   JOIN monitors tm ON tm.id = tr.monitor_id
		  WHERE sm.workspace_id = ? AND tm.workspace_id = ?
		  ORDER BY ai.source_rule_id ASC, ai.target_rule_id ASC`, ws, ws)
	if err != nil {
		return mapSQLiteErr(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		in, err := scanSQLiteInhibition(rows)
		if err != nil {
			return err
		}
		if err := emitRow("alert_inhibitions", in); err != nil {
			return err
		}
	}
	return mapSQLiteErr(rows.Err())
}

func (s *SQLite) exportSQLiteEscalationPolicies(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ep.id, ep.monitor_id, ep.created_at, ep.updated_at
		   FROM escalation_policies ep
		   JOIN monitors m ON m.id = ep.monitor_id
		  WHERE m.workspace_id = ?
		  ORDER BY ep.id ASC`, ws)
	if err != nil {
		return mapSQLiteErr(err)
	}
	var policies []EscalationPolicy
	for rows.Next() {
		var ep EscalationPolicy
		var createdStr, updatedStr string
		if err := rows.Scan(&ep.ID, &ep.MonitorID, &createdStr, &updatedStr); err != nil {
			_ = rows.Close()
			return mapSQLiteErr(err)
		}
		if ep.CreatedAt, err = parseExportSQLiteTime(createdStr); err != nil {
			_ = rows.Close()
			return err
		}
		if ep.UpdatedAt, err = parseExportSQLiteTime(updatedStr); err != nil {
			_ = rows.Close()
			return err
		}
		policies = append(policies, ep)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return mapSQLiteErr(err)
	}
	_ = rows.Close()

	for i := range policies {
		steps, err := s.listEscalationSteps(ctx, policies[i].ID)
		if err != nil {
			return err
		}
		policies[i].Steps = steps
		if err := emitRow("escalation_policies", policies[i]); err != nil {
			return err
		}
	}
	return nil
}

func (s *SQLite) exportSQLiteEscalationSteps(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT es.id, es.policy_id, es.position, es.delay_seconds
		   FROM escalation_steps es
		   JOIN escalation_policies ep ON ep.id = es.policy_id
		   JOIN monitors m ON m.id = ep.monitor_id
		  WHERE m.workspace_id = ?
		  ORDER BY es.id ASC`, ws)
	if err != nil {
		return mapSQLiteErr(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var st ExportedEscalationStep
		if err := rows.Scan(&st.ID, &st.PolicyID, &st.Position, &st.DelaySeconds); err != nil {
			return mapSQLiteErr(err)
		}
		if err := emitRow("escalation_steps", st); err != nil {
			return err
		}
	}
	return mapSQLiteErr(rows.Err())
}

func (s *SQLite) exportSQLiteEscalationStepChannels(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT esc.step_id, esc.channel_id
		   FROM escalation_step_channels esc
		   JOIN escalation_steps es ON es.id = esc.step_id
		   JOIN escalation_policies ep ON ep.id = es.policy_id
		   JOIN monitors m ON m.id = ep.monitor_id
		   JOIN channels c ON c.id = esc.channel_id
		  WHERE m.workspace_id = ? AND c.workspace_id = ?
		  ORDER BY esc.step_id ASC, esc.channel_id ASC`, ws, ws)
	if err != nil {
		return mapSQLiteErr(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var sc ExportedEscalationStepChannel
		if err := rows.Scan(&sc.StepID, &sc.ChannelID); err != nil {
			return mapSQLiteErr(err)
		}
		if err := emitRow("escalation_step_channels", sc); err != nil {
			return err
		}
	}
	return mapSQLiteErr(rows.Err())
}

func (s *SQLite) exportSQLiteMaintenanceWindows(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT mw.id, mw.reason, mw.scope, mw.monitor_id, mw.contract_id, mw.start_at, mw.end_at, mw.created_at
		   FROM maintenance_windows mw
		   LEFT JOIN monitors m ON m.id = mw.monitor_id
		  WHERE m.workspace_id = ?
		     OR (mw.scope = 'contract' AND EXISTS (
		           SELECT 1
		             FROM monitors cm
		             JOIN json_each(cm.contract_ids) AS cid
		            WHERE cm.workspace_id = ? AND cid.value = mw.contract_id
		        ))
		  ORDER BY mw.id ASC`, ws, ws)
	if err != nil {
		return mapSQLiteErr(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		mw, err := scanSQLiteMaintenanceWindow(rows)
		if err != nil {
			return err
		}
		if err := emitRow("maintenance_windows", *mw); err != nil {
			return err
		}
	}
	return mapSQLiteErr(rows.Err())
}

func (s *SQLite) exportSQLiteBackfills(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT b.monitor_id, b.from_ledger, b.to_ledger, b.next_ledger, b.cursor, b.deliver, b.complete, b.updated_at
		   FROM backfills b
		   JOIN monitors m ON m.id = b.monitor_id
		  WHERE m.workspace_id = ?
		  ORDER BY b.monitor_id ASC`, ws)
	if err != nil {
		return mapSQLiteErr(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var b Backfill
		var fromL, toL, nextL int64
		var deliver, complete int64
		var updatedStr string
		if err := rows.Scan(&b.MonitorID, &fromL, &toL, &nextL, &b.Cursor, &deliver, &complete, &updatedStr); err != nil {
			return mapSQLiteErr(err)
		}
		b.FromLedger = uint32(fromL)
		b.ToLedger = uint32(toL)
		b.NextLedger = uint32(nextL)
		b.Deliver = deliver != 0
		b.Complete = complete != 0
		if b.UpdatedAt, err = parseExportSQLiteTime(updatedStr); err != nil {
			return err
		}
		if err := emitRow("backfills", b); err != nil {
			return err
		}
	}
	return mapSQLiteErr(rows.Err())
}

func (s *SQLite) exportSQLiteSavedSearches(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, filter, is_default, created_at
		   FROM saved_searches
		  WHERE workspace_id = ?
		  ORDER BY id ASC`, ws)
	if err != nil {
		return mapSQLiteErr(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		ss, err := scanSQLiteSavedSearch(rows)
		if err != nil {
			return err
		}
		if err := emitRow("saved_searches", ss); err != nil {
			return err
		}
	}
	return mapSQLiteErr(rows.Err())
}

func (s *SQLite) exportSQLiteMonitorTemplates(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, description, rules, channel_ids, parameters, created_at
		   FROM monitor_templates
		  WHERE workspace_id = ?
		  ORDER BY id ASC`, ws)
	if err != nil {
		return mapSQLiteErr(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		t, err := scanSQLiteTemplate(rows)
		if err != nil {
			return err
		}
		if err := emitRow("monitor_templates", t); err != nil {
			return err
		}
	}
	return mapSQLiteErr(rows.Err())
}

func (s *SQLite) exportSQLiteAPITokens(ctx context.Context, ws string, includeSecrets bool, emitRow func(string, any) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, workspace_id, name, token_hash, prefix, scopes,
		        expires_at, last_used_at, revoked_at, created_at,
		        rotated_from_id, successor_id, rotated_at, pre_rotation_expires_at
		   FROM api_tokens
		  WHERE workspace_id = ?
		  ORDER BY id ASC`, ws)
	if err != nil {
		return mapSQLiteErr(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var tok ExportedAPIToken
		var scopesJSON, createdStr string
		var expiresAt, lastUsedAt, revokedAt, rotatedAt, preRotationExpiresAt sql.NullString
		var rotatedFromID, successorID sql.NullInt64
		if err := rows.Scan(
			&tok.ID,
			&tok.WorkspaceID,
			&tok.Name,
			&tok.TokenHash,
			&tok.Prefix,
			&scopesJSON,
			&expiresAt,
			&lastUsedAt,
			&revokedAt,
			&createdStr,
			&rotatedFromID,
			&successorID,
			&rotatedAt,
			&preRotationExpiresAt,
		); err != nil {
			return mapSQLiteErr(err)
		}
		tok.Scopes = splitStoredScopes(scopesJSON)
		if tok.CreatedAt, err = parseExportSQLiteTime(createdStr); err != nil {
			return err
		}
		if tok.ExpiresAt, err = parseExportSQLiteTimePtr(expiresAt); err != nil {
			return err
		}
		if tok.LastUsedAt, err = parseExportSQLiteTimePtr(lastUsedAt); err != nil {
			return err
		}
		if tok.RevokedAt, err = parseExportSQLiteTimePtr(revokedAt); err != nil {
			return err
		}
		if rotatedFromID.Valid {
			v := rotatedFromID.Int64
			tok.RotatedFromID = &v
		}
		if successorID.Valid {
			v := successorID.Int64
			tok.SuccessorID = &v
		}
		if tok.RotatedAt, err = parseExportSQLiteTimePtr(rotatedAt); err != nil {
			return err
		}
		if tok.PreRotationExpiresAt, err = parseExportSQLiteTimePtr(preRotationExpiresAt); err != nil {
			return err
		}
		if !includeSecrets {
			tok.TokenHash = RedactedSecretPlaceholder
		}
		if err := emitRow("api_tokens", tok); err != nil {
			return err
		}
	}
	return mapSQLiteErr(rows.Err())
}

func (s *SQLite) exportSQLiteUsageRollups(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT workspace_id, period_start, period_end, alerts_raised, rule_evaluations, api_requests, updated_at
		   FROM workspace_usage_rollups
		  WHERE workspace_id = ?
		  ORDER BY period_start ASC`, ws)
	if err != nil {
		return mapSQLiteErr(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var r ExportedUsageRollup
		var startStr, endStr, updatedStr string
		if err := rows.Scan(
			&r.WorkspaceID,
			&startStr,
			&endStr,
			&r.AlertsRaised,
			&r.RuleEvaluations,
			&r.APIRequests,
			&updatedStr,
		); err != nil {
			return mapSQLiteErr(err)
		}
		if r.PeriodStart, err = parseExportSQLiteTime(startStr); err != nil {
			return err
		}
		if r.PeriodEnd, err = parseExportSQLiteTime(endStr); err != nil {
			return err
		}
		if r.UpdatedAt, err = parseExportSQLiteTime(updatedStr); err != nil {
			return err
		}
		if err := emitRow("workspace_usage_rollups", r); err != nil {
			return err
		}
	}
	return mapSQLiteErr(rows.Err())
}

func (s *SQLite) exportSQLiteNotificationUsageRollups(ctx context.Context, ws string, emitRow func(string, any) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT workspace_id, period_start, period_end, channel_type, outcome, count, updated_at
		   FROM workspace_notification_usage_rollups
		  WHERE workspace_id = ?
		  ORDER BY period_start ASC, channel_type ASC, outcome ASC`, ws)
	if err != nil {
		return mapSQLiteErr(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var r ExportedNotificationUsageRollup
		var startStr, endStr, updatedStr string
		if err := rows.Scan(
			&r.WorkspaceID,
			&startStr,
			&endStr,
			&r.ChannelType,
			&r.Outcome,
			&r.Count,
			&updatedStr,
		); err != nil {
			return mapSQLiteErr(err)
		}
		if r.PeriodStart, err = parseExportSQLiteTime(startStr); err != nil {
			return err
		}
		if r.PeriodEnd, err = parseExportSQLiteTime(endStr); err != nil {
			return err
		}
		if r.UpdatedAt, err = parseExportSQLiteTime(updatedStr); err != nil {
			return err
		}
		if err := emitRow("workspace_notification_usage_rollups", r); err != nil {
			return err
		}
	}
	return mapSQLiteErr(rows.Err())
}

func (s *SQLite) exportSQLiteAlertsPaged(ctx context.Context, ws string, pageSize int, emitRow func(string, any) error) error {
	var afterID int64
	for {
		rows, err := s.db.QueryContext(ctx,
			`SELECT id, monitor_id, rule_id, event_id, payload, enrichment, COALESCE(severity, 'warning'), created_at,
			        ledger, retracted_at, inhibited_by_rule_id, suppressed, suppression_reason,
			        acknowledged_at, acknowledged_by, network, federated, federation_origin,
			        source_monitor_id, source_rule_id, rule_version, runbook_url, response_guidance
			   FROM alerts
			  WHERE workspace_id = ? AND id > ?
			  ORDER BY id ASC
			  LIMIT ?`, ws, afterID, pageSize)
		if err != nil {
			return mapSQLiteErr(err)
		}
		var page []Alert
		for rows.Next() {
			var a Alert
			var payload, sev, createdStr, suppressionReason string
			var enrichment, retracted, acknowledged, acknowledgedBy, origin sql.NullString
			var ledger, suppressed int64
			var inhibited, monitorID, ruleID, sourceMonitorID, sourceRuleID sql.NullInt64
			if err := rows.Scan(
				&a.ID,
				&monitorID,
				&ruleID,
				&a.EventID,
				&payload,
				&enrichment,
				&sev,
				&createdStr,
				&ledger,
				&retracted,
				&inhibited,
				&suppressed,
				&suppressionReason,
				&acknowledged,
				&acknowledgedBy,
				&a.Network,
				&a.Federated,
				&origin,
				&sourceMonitorID,
				&sourceRuleID,
				&a.RuleVersion,
				&a.RunbookURL,
				&a.ResponseGuidance,
			); err != nil {
				_ = rows.Close()
				return mapSQLiteErr(err)
			}
			if monitorID.Valid {
				a.MonitorID = monitorID.Int64
			}
			if ruleID.Valid {
				a.RuleID = ruleID.Int64
			}
			if origin.Valid {
				a.FederationOrigin = origin.String
			}
			if sourceMonitorID.Valid {
				a.SourceMonitorID = sourceMonitorID.Int64
			}
			if sourceRuleID.Valid {
				a.SourceRuleID = sourceRuleID.Int64
			}
			if inhibited.Valid {
				v := inhibited.Int64
				a.InhibitedByRuleID = &v
			}
			a.Severity = Severity(sev)
			a.Suppressed = suppressed != 0
			a.SuppressionReason = suppressionReason
			a.Payload = json.RawMessage(payload)
			if enrichment.Valid && enrichment.String != "" {
				a.Enrichment = json.RawMessage(enrichment.String)
			}
			a.Ledger = uint32(ledger)
			if a.CreatedAt, err = parseExportSQLiteTime(createdStr); err != nil {
				_ = rows.Close()
				return err
			}
			if a.RetractedAt, err = parseExportSQLiteTimePtr(retracted); err != nil {
				_ = rows.Close()
				return err
			}
			if a.AcknowledgedAt, err = parseExportSQLiteTimePtr(acknowledged); err != nil {
				_ = rows.Close()
				return err
			}
			if acknowledgedBy.Valid {
				a.AcknowledgedBy = acknowledgedBy.String
			}
			page = append(page, a)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return mapSQLiteErr(err)
		}
		_ = rows.Close()

		for _, a := range page {
			afterID = a.ID
			if err := emitRow("alerts", a); err != nil {
				return err
			}
		}
		if len(page) < pageSize {
			return nil
		}
	}
}

func (s *SQLite) exportSQLiteAlertDedupPaged(ctx context.Context, ws string, pageSize int, emitRow func(string, any) error) error {
	// SQLite enforces (rule_id, event_id) uniqueness directly on the alerts
	// table instead of a separate physical alert_dedup table, so we project the
	// equivalent dedup rows from alerts to keep the export identical across
	// backends.
	var (
		afterAlertID int64
		afterRuleID  int64
		afterEventID string
	)
	for {
		rows, err := s.db.QueryContext(ctx,
			`SELECT rule_id, event_id, id, created_at
			   FROM alerts
			  WHERE workspace_id = ?
			    AND rule_id IS NOT NULL
			    AND (id > ?
			         OR (id = ? AND rule_id > ?)
			         OR (id = ? AND rule_id = ? AND event_id > ?))
			  ORDER BY id ASC, rule_id ASC, event_id ASC
			  LIMIT ?`, ws, afterAlertID, afterAlertID, afterRuleID, afterAlertID, afterRuleID, afterEventID, pageSize)
		if err != nil {
			return mapSQLiteErr(err)
		}
		var page []ExportedAlertDedup
		for rows.Next() {
			var d ExportedAlertDedup
			var createdStr string
			if err := rows.Scan(&d.RuleID, &d.EventID, &d.AlertID, &createdStr); err != nil {
				_ = rows.Close()
				return mapSQLiteErr(err)
			}
			if d.CreatedAt, err = parseExportSQLiteTime(createdStr); err != nil {
				_ = rows.Close()
				return err
			}
			page = append(page, d)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return mapSQLiteErr(err)
		}
		_ = rows.Close()

		for _, d := range page {
			afterAlertID = d.AlertID
			afterRuleID = d.RuleID
			afterEventID = d.EventID
			if err := emitRow("alert_dedup", d); err != nil {
				return err
			}
		}
		if len(page) < pageSize {
			return nil
		}
	}
}

func (s *SQLite) exportSQLiteDeliveryAttemptsPaged(ctx context.Context, ws string, pageSize int, emitRow func(string, any) error) error {
	var afterID int64
	for {
		rows, err := s.db.QueryContext(ctx,
			`SELECT da.id, da.alert_id, da.channel_id, da.status, da.response_snippet, da.attempted_at
			   FROM delivery_attempts da
			   JOIN alerts a ON a.id = da.alert_id
			  WHERE a.workspace_id = ? AND da.id > ?
			  ORDER BY da.id ASC
			  LIMIT ?`, ws, afterID, pageSize)
		if err != nil {
			return mapSQLiteErr(err)
		}
		var page []DeliveryAttempt
		for rows.Next() {
			var da DeliveryAttempt
			var attemptedStr string
			if err := rows.Scan(&da.ID, &da.AlertID, &da.ChannelID, &da.Status, &da.ResponseSnippet, &attemptedStr); err != nil {
				_ = rows.Close()
				return mapSQLiteErr(err)
			}
			if da.AttemptedAt, err = parseExportSQLiteTime(attemptedStr); err != nil {
				_ = rows.Close()
				return err
			}
			page = append(page, da)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return mapSQLiteErr(err)
		}
		_ = rows.Close()

		for _, da := range page {
			afterID = da.ID
			if err := emitRow("delivery_attempts", da); err != nil {
				return err
			}
		}
		if len(page) < pageSize {
			return nil
		}
	}
}

func (s *SQLite) exportSQLiteDeadLettersPaged(ctx context.Context, ws string, pageSize int, emitRow func(string, any) error) error {
	var afterID int64
	for {
		rows, err := s.db.QueryContext(ctx,
			`SELECT dl.id, dl.alert_id, dl.channel_id, dl.last_error, dl.attempt_count, dl.last_status, dl.created_at
			   FROM dead_letters dl
			   JOIN alerts a ON a.id = dl.alert_id
			  WHERE a.workspace_id = ? AND dl.id > ?
			  ORDER BY dl.id ASC
			  LIMIT ?`, ws, afterID, pageSize)
		if err != nil {
			return mapSQLiteErr(err)
		}
		var page []DeadLetter
		for rows.Next() {
			var dl DeadLetter
			var createdStr string
			if err := rows.Scan(&dl.ID, &dl.AlertID, &dl.ChannelID, &dl.LastError, &dl.AttemptCount, &dl.LastStatus, &createdStr); err != nil {
				_ = rows.Close()
				return mapSQLiteErr(err)
			}
			if dl.CreatedAt, err = parseExportSQLiteTime(createdStr); err != nil {
				_ = rows.Close()
				return err
			}
			dl.Cause = ClassifyCause(dl.LastStatus, dl.LastError)
			page = append(page, dl)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return mapSQLiteErr(err)
		}
		_ = rows.Close()

		for _, dl := range page {
			afterID = dl.ID
			if err := emitRow("dead_letters", dl); err != nil {
				return err
			}
		}
		if len(page) < pageSize {
			return nil
		}
	}
}

func (s *SQLite) exportSQLiteAlertEscalationsPaged(ctx context.Context, ws string, pageSize int, emitRow func(string, any) error) error {
	var afterAlertID int64
	for {
		rows, err := s.db.QueryContext(ctx,
			`SELECT ae.alert_id, ae.alert_created_at, ae.policy_id, ae.alert_snapshot, ae.next_step, ae.next_due_at, ae.completed_at, ae.updated_at
			   FROM alert_escalations ae
			   JOIN alerts a ON a.id = ae.alert_id
			  WHERE a.workspace_id = ? AND ae.alert_id > ?
			  ORDER BY ae.alert_id ASC
			  LIMIT ?`, ws, afterAlertID, pageSize)
		if err != nil {
			return mapSQLiteErr(err)
		}
		var page []ExportedAlertEscalation
		for rows.Next() {
			var ae ExportedAlertEscalation
			var alertCreatedStr, snapshotStr, nextDueStr, updatedStr string
			var completedStr sql.NullString
			if err := rows.Scan(
				&ae.AlertID,
				&alertCreatedStr,
				&ae.PolicyID,
				&snapshotStr,
				&ae.NextStep,
				&nextDueStr,
				&completedStr,
				&updatedStr,
			); err != nil {
				_ = rows.Close()
				return mapSQLiteErr(err)
			}
			ae.AlertSnapshot = json.RawMessage(snapshotStr)
			if ae.AlertCreatedAt, err = parseExportSQLiteTime(alertCreatedStr); err != nil {
				_ = rows.Close()
				return err
			}
			if ae.NextDueAt, err = parseExportSQLiteTime(nextDueStr); err != nil {
				_ = rows.Close()
				return err
			}
			if ae.CompletedAt, err = parseExportSQLiteTimePtr(completedStr); err != nil {
				_ = rows.Close()
				return err
			}
			if ae.UpdatedAt, err = parseExportSQLiteTime(updatedStr); err != nil {
				_ = rows.Close()
				return err
			}
			page = append(page, ae)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return mapSQLiteErr(err)
		}
		_ = rows.Close()

		for _, ae := range page {
			afterAlertID = ae.AlertID
			if err := emitRow("alert_escalations", ae); err != nil {
				return err
			}
		}
		if len(page) < pageSize {
			return nil
		}
	}
}

func parseExportSQLiteTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(sqliteTimeLayout, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	return parseSQLiteTime(s)
}

func parseExportSQLiteTimePtr(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return nil, nil
	}
	t, err := parseExportSQLiteTime(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}
