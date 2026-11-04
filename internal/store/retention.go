package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

var (
	// ErrRetentionShortenUnconfirmed is returned when an update or reset would
	// shorten a workspace's effective alert retention window without explicit
	// confirmation (ConfirmShorten: true).
	ErrRetentionShortenUnconfirmed = errors.New("shortening workspace retention requires confirmation (confirm_shorten=true)")

	// ErrInvalidRetention is returned when a retention duration is non-positive
	// or shorter than one second.
	ErrInvalidRetention = errors.New("retention must be a positive duration of at least 1s")
)

const (
	RetentionPolicySourceWorkspace = "workspace"
	RetentionPolicySourceInstance  = "instance"
	RetentionPolicySourceUnset     = "unset"

	AuditTargetWorkspaceRetention = "workspace_retention"
)

// WorkspaceRetentionPolicy describes the configured and effective alert
// retention policy for a single workspace.
//
// Partition interaction trade-off (design (a) — hybrid partition-safe):
//
//   - Postgres partitions `alerts` by UTC calendar month (`created_at`). A
//     monthly partition holds rows from every workspace on the instance, so
//     dropping a partition via DDL (`DROP TABLE`) is only safe once *all*
//     workspaces have passed their retention horizon (`EffectiveRetentionCutoff`
//     uses the maximum retention across the instance default and every
//     workspace override).
//   - For workspaces with a shorter retention policy than the longest horizon
//     on the instance (or on SQLite / inside the active month partition),
//     expired alerts are batch-deleted per workspace by the pruner
//     (`DeleteExpiredAlertsForWorkspace`) AND filtered out at read time
//     (`created_at >= pruned_before`) across `GetAlert`, `ListAlerts`,
//     `ListAlertsStream`, `AlertCountsByDay`, `GetStats`, and `GetMonitorStats`.
//     This guarantees a workspace never sees alerts older than its configured
//     retention even before the next background prune tick or partition drop.
type WorkspaceRetentionPolicy struct {
	WorkspaceID               string     `json:"workspace_id"`
	Retention                 string     `json:"retention,omitempty"`
	RetentionSeconds          int64      `json:"retention_seconds"`
	EffectiveRetention        string     `json:"effective_retention,omitempty"`
	EffectiveRetentionSeconds int64      `json:"effective_retention_seconds"`
	Configured                bool       `json:"configured"`
	PolicySource              string     `json:"policy_source"`
	PrunedBefore              *time.Time `json:"pruned_before,omitempty"`
	UpdatedBy                 string     `json:"updated_by,omitempty"`
	UpdatedAt                 *time.Time `json:"updated_at,omitempty"`
}

// EffectiveDuration returns the effective retention window as a time.Duration.
func (p WorkspaceRetentionPolicy) EffectiveDuration() time.Duration {
	if p.EffectiveRetentionSeconds <= 0 {
		return 0
	}
	return time.Duration(p.EffectiveRetentionSeconds) * time.Second
}

// SetWorkspaceRetentionInput carries the parameters for configuring a
// workspace's alert retention policy.
type SetWorkspaceRetentionInput struct {
	WorkspaceID       string
	Retention         time.Duration
	ConfirmShorten    bool
	Actor             string
	InstanceRetention time.Duration
}

// ParseRetentionDuration parses a positive duration string supporting both Go's
// time.ParseDuration syntax ("24h", "168h") and day units ("7d", "30d", "1.5d").
func ParseRetentionDuration(raw string) (time.Duration, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, ErrInvalidRetention
	}
	if strings.HasSuffix(s, "d") {
		num := strings.TrimSpace(strings.TrimSuffix(s, "d"))
		days, err := strconv.ParseFloat(num, 64)
		if err != nil || math.IsNaN(days) || math.IsInf(days, 0) || days <= 0 {
			return 0, ErrInvalidRetention
		}
		d := time.Duration(days * float64(24*time.Hour))
		if d < time.Second {
			return 0, ErrInvalidRetention
		}
		return d.Truncate(time.Second), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < time.Second {
		return 0, ErrInvalidRetention
	}
	return d.Truncate(time.Second), nil
}

// InstanceAlertRetentionConfigurable is implemented by store backends that can
// hold the instance-wide default ALERT_RETENTION duration.
type InstanceAlertRetentionConfigurable interface {
	SetAlertRetention(d time.Duration)
	InstanceAlertRetention() time.Duration
}

// SetInstanceAlertRetention configures the instance-wide fallback alert
// retention on st when supported by the backend.
func SetInstanceAlertRetention(st Store, d time.Duration) {
	if st == nil {
		return
	}
	if c, ok := st.(InstanceAlertRetentionConfigurable); ok {
		c.SetAlertRetention(d)
	}
}

// GetInstanceAlertRetention reads the instance-wide fallback alert retention
// configured on st, or 0 if unset.
func GetInstanceAlertRetention(st Store) time.Duration {
	if st == nil {
		return 0
	}
	if c, ok := st.(InstanceAlertRetentionConfigurable); ok {
		return c.InstanceAlertRetention()
	}
	return 0
}

// WithAlertRetention sets the instance-wide fallback alert retention on Postgres.
func (p *Postgres) WithAlertRetention(d time.Duration) *Postgres {
	p.SetAlertRetention(d)
	return p
}

// SetAlertRetention updates the instance-wide fallback alert retention on Postgres.
func (p *Postgres) SetAlertRetention(d time.Duration) {
	if d < 0 {
		d = 0
	}
	p.alertRetention.Store(int64(d))
}

// InstanceAlertRetention returns the instance-wide fallback alert retention on Postgres.
func (p *Postgres) InstanceAlertRetention() time.Duration {
	return time.Duration(p.alertRetention.Load())
}

func resolvePolicyFields(ws workspace.ID, configuredSeconds int64, configured bool, updatedBy string, updatedAt *time.Time, instanceRetention time.Duration, now time.Time) WorkspaceRetentionPolicy {
	out := WorkspaceRetentionPolicy{
		WorkspaceID: string(ws),
		Configured:  configured,
		UpdatedBy:   updatedBy,
		UpdatedAt:   updatedAt,
	}
	if configured && configuredSeconds > 0 {
		d := time.Duration(configuredSeconds) * time.Second
		out.RetentionSeconds = configuredSeconds
		out.Retention = d.String()
		out.EffectiveRetentionSeconds = configuredSeconds
		out.EffectiveRetention = d.String()
		out.PolicySource = RetentionPolicySourceWorkspace
		if !now.IsZero() {
			cutoff := now.UTC().Add(-d)
			out.PrunedBefore = &cutoff
		}
		return out
	}
	instSec := int64(instanceRetention.Truncate(time.Second) / time.Second)
	if instSec > 0 {
		d := time.Duration(instSec) * time.Second
		out.EffectiveRetentionSeconds = instSec
		out.EffectiveRetention = d.String()
		out.PolicySource = RetentionPolicySourceInstance
		if !now.IsZero() {
			cutoff := now.UTC().Add(-d)
			out.PrunedBefore = &cutoff
		}
		return out
	}
	out.PolicySource = RetentionPolicySourceUnset
	return out
}

func normalizeRetentionWorkspace(ctx context.Context, explicit string) workspace.ID {
	ws := workspace.ID(strings.TrimSpace(explicit))
	if ws == "" {
		ws = workspaceID(ctx)
	}
	if ws == "" {
		ws = workspace.Default
	}
	return ws
}

// GetWorkspaceRetentionPolicy returns the retention policy for ws, falling back
// to the store's instance-wide alert retention when no workspace-specific row
// exists.
func (p *Postgres) GetWorkspaceRetentionPolicy(ctx context.Context, ws workspace.ID) (WorkspaceRetentionPolicy, error) {
	if ws == "" {
		ws = workspaceID(ctx)
	}
	now := time.Now().UTC()
	var (
		seconds   int64
		updatedBy string
		updatedAt time.Time
	)
	err := p.pool.QueryRow(ctx, `
		SELECT retention_seconds, updated_by, updated_at
		FROM workspace_retention_policies
		WHERE workspace_id = $1`, string(ws)).Scan(&seconds, &updatedBy, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return resolvePolicyFields(ws, 0, false, "", nil, p.InstanceAlertRetention(), now), nil
	}
	if err != nil {
		return WorkspaceRetentionPolicy{}, mapErr(err)
	}
	ua := updatedAt.UTC()
	return resolvePolicyFields(ws, seconds, true, updatedBy, &ua, p.InstanceAlertRetention(), now), nil
}

// SetWorkspaceRetentionPolicy creates or updates a workspace's alert retention
// policy. If the new retention is shorter than the workspace's previous
// effective retention (either an existing workspace policy or the instance
// fallback), ConfirmShorten must be true or ErrRetentionShortenUnconfirmed is
// returned. Every change is recorded in audit_log, including whether a
// retention shortening was confirmed.
func (p *Postgres) SetWorkspaceRetentionPolicy(ctx context.Context, in SetWorkspaceRetentionInput) (WorkspaceRetentionPolicy, error) {
	ws := normalizeRetentionWorkspace(ctx, in.WorkspaceID)
	d := in.Retention.Truncate(time.Second)
	if d < time.Second {
		return WorkspaceRetentionPolicy{}, ErrInvalidRetention
	}
	newSec := int64(d / time.Second)

	instRetention := in.InstanceRetention
	if instRetention <= 0 {
		instRetention = p.InstanceAlertRetention()
	}
	instSec := int64(instRetention.Truncate(time.Second) / time.Second)

	if err := p.EnsureWorkspace(ctx, ws); err != nil {
		return WorkspaceRetentionPolicy{}, err
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return WorkspaceRetentionPolicy{}, mapErr(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		prevSec        int64
		prevConfigured bool
	)
	err = tx.QueryRow(ctx, `
		SELECT retention_seconds
		FROM workspace_retention_policies
		WHERE workspace_id = $1
		FOR UPDATE`, string(ws)).Scan(&prevSec)
	switch {
	case err == nil:
		prevConfigured = true
	case errors.Is(err, pgx.ErrNoRows):
		prevConfigured = false
	default:
		return WorkspaceRetentionPolicy{}, mapErr(err)
	}

	prevEffSec := prevSec
	if !prevConfigured {
		prevEffSec = instSec
	}
	shortened := (prevConfigured && newSec < prevSec) || (prevEffSec > 0 && newSec < prevEffSec)
	if shortened && !in.ConfirmShorten {
		return WorkspaceRetentionPolicy{}, ErrRetentionShortenUnconfirmed
	}

	actor := strings.TrimSpace(in.Actor)
	if actor == "" {
		actor = string(ws)
	}

	var updatedAt time.Time
	if err := tx.QueryRow(ctx, `
		INSERT INTO workspace_retention_policies (workspace_id, retention_seconds, updated_by, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (workspace_id) DO UPDATE SET
			retention_seconds = EXCLUDED.retention_seconds,
			updated_by        = EXCLUDED.updated_by,
			updated_at        = now()
		RETURNING updated_at`,
		string(ws), newSec, actor).Scan(&updatedAt); err != nil {
		return WorkspaceRetentionPolicy{}, mapErr(err)
	}

	diffPayload, _ := json.Marshal(map[string]any{
		"workspace_id":               string(ws),
		"previous_retention_seconds": prevSec,
		"previous_effective_seconds": prevEffSec,
		"new_retention_seconds":      newSec,
		"new_effective_seconds":      newSec,
		"shortened":                  shortened,
		"confirm_shorten":            in.ConfirmShorten,
		"policy_source":              RetentionPolicySourceWorkspace,
	})

	if err := tx.Commit(ctx); err != nil {
		return WorkspaceRetentionPolicy{}, mapErr(err)
	}

	if err := p.CreateAuditEntry(workspace.With(ctx, ws), &AuditEntry{
		WorkspaceID: string(ws),
		Actor:       actor,
		Action:      AuditActionUpdate,
		TargetType:  AuditTargetWorkspaceRetention,
		TargetID:    0,
		ResourceID:  string(ws),
		Diff:        diffPayload,
	}); err != nil {
		return WorkspaceRetentionPolicy{}, mapErr(err)
	}

	ua := updatedAt.UTC()
	return resolvePolicyFields(ws, newSec, true, actor, &ua, instRetention, time.Now().UTC()), nil
}

// DeleteWorkspaceRetentionPolicy removes a workspace's explicit retention
// policy so it reverts to the instance-wide ALERT_RETENTION fallback. If the
// instance fallback is a positive duration shorter than the workspace's
// previously configured retention, confirmShorten must be true.
func (p *Postgres) DeleteWorkspaceRetentionPolicy(ctx context.Context, ws workspace.ID, confirmShorten bool, actor string) (WorkspaceRetentionPolicy, error) {
	if ws == "" {
		ws = workspaceID(ctx)
	}
	instRetention := p.InstanceAlertRetention()
	instSec := int64(instRetention.Truncate(time.Second) / time.Second)

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return WorkspaceRetentionPolicy{}, mapErr(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		prevSec        int64
		prevConfigured bool
	)
	err = tx.QueryRow(ctx, `
		SELECT retention_seconds
		FROM workspace_retention_policies
		WHERE workspace_id = $1
		FOR UPDATE`, string(ws)).Scan(&prevSec)
	switch {
	case err == nil:
		prevConfigured = true
	case errors.Is(err, pgx.ErrNoRows):
		prevConfigured = false
	default:
		return WorkspaceRetentionPolicy{}, mapErr(err)
	}

	shortened := prevConfigured && instSec > 0 && instSec < prevSec
	if shortened && !confirmShorten {
		return WorkspaceRetentionPolicy{}, ErrRetentionShortenUnconfirmed
	}

	var (
		actorName   string
		diffPayload []byte
	)
	if prevConfigured {
		if _, err := tx.Exec(ctx, `DELETE FROM workspace_retention_policies WHERE workspace_id = $1`, string(ws)); err != nil {
			return WorkspaceRetentionPolicy{}, mapErr(err)
		}
		actorName = strings.TrimSpace(actor)
		if actorName == "" {
			actorName = string(ws)
		}
		fallbackSource := RetentionPolicySourceUnset
		if instSec > 0 {
			fallbackSource = RetentionPolicySourceInstance
		}
		diffPayload, _ = json.Marshal(map[string]any{
			"workspace_id":               string(ws),
			"previous_retention_seconds": prevSec,
			"previous_effective_seconds": prevSec,
			"new_retention_seconds":      int64(0),
			"new_effective_seconds":      instSec,
			"shortened":                  shortened,
			"confirm_shorten":            confirmShorten,
			"policy_source":              fallbackSource,
		})
	}

	if err := tx.Commit(ctx); err != nil {
		return WorkspaceRetentionPolicy{}, mapErr(err)
	}

	if prevConfigured {
		if err := p.CreateAuditEntry(workspace.With(ctx, ws), &AuditEntry{
			WorkspaceID: string(ws),
			Actor:       actorName,
			Action:      AuditActionDelete,
			TargetType:  AuditTargetWorkspaceRetention,
			TargetID:    0,
			ResourceID:  string(ws),
			Diff:        diffPayload,
		}); err != nil {
			return WorkspaceRetentionPolicy{}, mapErr(err)
		}
	}

	return resolvePolicyFields(ws, 0, false, "", nil, instRetention, time.Now().UTC()), nil
}

// ListWorkspaceRetentionPolicies returns the resolved retention policy for
// every known workspace on the instance, ordered by workspace_id.
func (p *Postgres) ListWorkspaceRetentionPolicies(ctx context.Context) ([]WorkspaceRetentionPolicy, error) {
	now := time.Now().UTC()
	instRetention := p.InstanceAlertRetention()
	rows, err := p.pool.Query(ctx, `
		WITH all_workspaces AS (
			SELECT id AS workspace_id FROM workspaces
			UNION
			SELECT workspace_id FROM workspace_retention_policies
		)
		SELECT w.workspace_id,
		       COALESCE(rp.retention_seconds, 0),
		       rp.workspace_id IS NOT NULL,
		       COALESCE(rp.updated_by, ''),
		       rp.updated_at
		FROM all_workspaces w
		LEFT JOIN workspace_retention_policies rp ON rp.workspace_id = w.workspace_id
		ORDER BY w.workspace_id ASC`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var out []WorkspaceRetentionPolicy
	for rows.Next() {
		var (
			wsID       string
			sec        int64
			configured bool
			updatedBy  string
			updatedAt  *time.Time
		)
		if err := rows.Scan(&wsID, &sec, &configured, &updatedBy, &updatedAt); err != nil {
			return nil, mapErr(err)
		}
		var ua *time.Time
		if updatedAt != nil {
			t := updatedAt.UTC()
			ua = &t
		}
		out = append(out, resolvePolicyFields(workspace.ID(wsID), sec, configured, updatedBy, ua, instRetention, now))
	}
	return out, rows.Err()
}

// ExpiredAlertsForWorkspace returns up to limit alerts in ws older than cutoff,
// oldest first, matching DeleteExpiredAlertsForWorkspace's ordering.
func (p *Postgres) ExpiredAlertsForWorkspace(ctx context.Context, ws workspace.ID, cutoff time.Time, limit int) ([]Alert, error) {
	if limit <= 0 {
		limit = DefaultPruneBatch
	}
	rows, err := p.pool.Query(ctx,
		`SELECT id, monitor_id, rule_id, event_id, payload, enrichment, created_at, ledger, retracted_at, backfilled, inhibited_by_rule_id, suppressed, suppression_reason, acknowledged_at, acknowledged_by, network, federated, federation_origin, source_monitor_id, source_rule_id, rule_version, runbook_url, response_guidance
		   FROM alerts
		  WHERE workspace_id = $1 AND created_at < $2
		  ORDER BY created_at ASC, id ASC
		  LIMIT $3`,
		string(ws), cutoff.UTC(), limit)
	if err != nil {
		return nil, mapErr(err)
	}
	return pgx.CollectRows(rows, scanAlert)
}

// DeleteExpiredAlertsForWorkspace deletes up to limit alerts in ws whose
// created_at is strictly before cutoff and clears their alert_dedup entries in
// the same statement.
func (p *Postgres) DeleteExpiredAlertsForWorkspace(ctx context.Context, ws workspace.ID, cutoff time.Time, limit int) (int64, error) {
	if limit <= 0 {
		limit = DefaultPruneBatch
	}
	var deleted int64
	err := p.pool.QueryRow(ctx, `
		WITH expired AS (
			SELECT id, created_at FROM alerts
			WHERE workspace_id = $1 AND created_at < $2
			ORDER BY created_at ASC, id ASC
			LIMIT $3
		),
		cleared_dedup AS (
			DELETE FROM alert_dedup d
			USING expired e
			WHERE d.alert_id = e.id
		),
		deleted_rows AS (
			DELETE FROM alerts a
			USING expired e
			WHERE a.id = e.id AND a.created_at = e.created_at AND a.workspace_id = $1
			RETURNING a.id
		)
		SELECT count(*) FROM deleted_rows`, string(ws), cutoff.UTC(), limit).Scan(&deleted)
	return deleted, mapErr(err)
}

// workspaceAlertCutoff resolves the active read-time retention cutoff for the
// tenant workspace in ctx. For cross-tenant system contexts or workspaces with
// no active retention policy, it returns (time.Time{}, policy, false).
func (p *Postgres) workspaceAlertCutoff(ctx context.Context) (time.Time, WorkspaceRetentionPolicy, bool) {
	ws, scoped := tenantWorkspace(ctx)
	if !scoped {
		return time.Time{}, WorkspaceRetentionPolicy{}, false
	}
	policy, err := p.GetWorkspaceRetentionPolicy(ctx, ws)
	if err != nil || policy.EffectiveRetentionSeconds <= 0 || policy.PrunedBefore == nil {
		return time.Time{}, policy, false
	}
	return *policy.PrunedBefore, policy, true
}

// SafeGetWorkspaceRetentionPolicy invokes st.GetWorkspaceRetentionPolicy and
// falls back to an unset policy when a test fake embeds a nil Store.
func SafeGetWorkspaceRetentionPolicy(ctx context.Context, st Store, ws workspace.ID) (policy WorkspaceRetentionPolicy, err error) {
	if st == nil {
		return resolvePolicyFields(ws, 0, false, "", nil, 0, time.Now().UTC()), nil
	}
	defer func() {
		if r := recover(); r != nil {
			policy = resolvePolicyFields(ws, 0, false, "", nil, GetInstanceAlertRetention(st), time.Now().UTC())
			err = nil
		}
	}()
	return st.GetWorkspaceRetentionPolicy(ctx, ws)
}

// SafeSetWorkspaceRetentionPolicy invokes st.SetWorkspaceRetentionPolicy.
func SafeSetWorkspaceRetentionPolicy(ctx context.Context, st Store, in SetWorkspaceRetentionInput) (WorkspaceRetentionPolicy, error) {
	if st == nil {
		return WorkspaceRetentionPolicy{}, errors.New("store is not configured")
	}
	return st.SetWorkspaceRetentionPolicy(ctx, in)
}

// SafeDeleteWorkspaceRetentionPolicy invokes st.DeleteWorkspaceRetentionPolicy.
func SafeDeleteWorkspaceRetentionPolicy(ctx context.Context, st Store, ws workspace.ID, confirmShorten bool, actor string) (WorkspaceRetentionPolicy, error) {
	if st == nil {
		return WorkspaceRetentionPolicy{}, errors.New("store is not configured")
	}
	return st.DeleteWorkspaceRetentionPolicy(ctx, ws, confirmShorten, actor)
}

// SafeListWorkspaceRetentionPolicies invokes st.ListWorkspaceRetentionPolicies safely.
func SafeListWorkspaceRetentionPolicies(ctx context.Context, st Store) (policies []WorkspaceRetentionPolicy, err error) {
	if st == nil {
		return nil, nil
	}
	defer func() {
		if r := recover(); r != nil {
			policies = nil
			err = nil
		}
	}()
	return st.ListWorkspaceRetentionPolicies(ctx)
}

// SafeEffectiveAlertCutoff returns the active alert retention cutoff for the
// workspace in ctx without panicking on partial Store test doubles.
func SafeEffectiveAlertCutoff(ctx context.Context, st Store, now time.Time) (cutoff time.Time, policy WorkspaceRetentionPolicy, err error) {
	ws := workspaceID(ctx)
	policy, err = SafeGetWorkspaceRetentionPolicy(ctx, st, ws)
	if err != nil {
		return time.Time{}, WorkspaceRetentionPolicy{}, err
	}
	if policy.EffectiveRetentionSeconds <= 0 {
		return time.Time{}, policy, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	c := now.UTC().Add(-time.Duration(policy.EffectiveRetentionSeconds) * time.Second)
	policy.PrunedBefore = &c
	return c, policy, nil
}

// WorkspaceRetentionMap converts a slice of WorkspaceRetentionPolicy into a map
// of configured workspace retention durations suitable for EffectiveRetentionCutoff.
func WorkspaceRetentionMap(policies []WorkspaceRetentionPolicy) map[string]time.Duration {
	if len(policies) == 0 {
		return nil
	}
	out := make(map[string]time.Duration, len(policies))
	for _, p := range policies {
		if d := p.EffectiveDuration(); d > 0 {
			out[p.WorkspaceID] = d
		}
	}
	return out
}

// FormatRetentionSummary formats a human-readable summary of a workspace retention policy.
func FormatRetentionSummary(p WorkspaceRetentionPolicy) string {
	if p.EffectiveRetentionSeconds <= 0 {
		return fmt.Sprintf("workspace=%s policy_source=%s retention=unset", p.WorkspaceID, p.PolicySource)
	}
	return fmt.Sprintf("workspace=%s policy_source=%s retention=%s", p.WorkspaceID, p.PolicySource, p.EffectiveRetention)
}
