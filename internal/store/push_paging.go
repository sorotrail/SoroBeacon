package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

// PushDevice is one registered mobile device for a push channel. It holds
// only what delivery needs. Token is a credential: it is excluded from JSON
// so no API response can echo it, and it is never logged.
type PushDevice struct {
	ChannelID int64  `json:"channel_id"`
	DeviceID  string `json:"device_id"`
	Platform  string `json:"platform"`
	Token     string `json:"-"`
}

// Paging sync states recorded on an AlertExternalRef. State only moves
// forward: open, acknowledged, resolved.
const (
	ExternalStateOpen         = ""
	ExternalStateAcknowledged = "acknowledged"
	ExternalStateResolved     = "resolved"
)

// AlertExternalRef correlates an alert with the incident it opened at an
// external paging provider. DedupKey is what SoroBeacon sent; IncidentID is
// the provider's own identifier, learned from the provider's webhook.
// SyncError is a short, secret-free reason the last outbound update failed,
// which is how a divergence stays visible instead of silent.
type AlertExternalRef struct {
	AlertID    int64
	ChannelID  int64
	Provider   string
	DedupKey   string
	IncidentID string
	State      string
	SyncError  string
}

// PushDeviceStore is the narrow view the push notifier depends on.
type PushDeviceStore interface {
	ListPushDevices(ctx context.Context, channelID int64) ([]PushDevice, error)
	RetirePushDevice(ctx context.Context, channelID int64, deviceID string) error
}

// PagingRefStore is the narrow view the paging sync depends on.
type PagingRefStore interface {
	RecordAlertExternalRef(ctx context.Context, ref *AlertExternalRef) error
	GetAlertExternalRefByDedupKey(ctx context.Context, channelID int64, provider, dedupKey string) (*AlertExternalRef, error)
	ListAlertExternalRefs(ctx context.Context, alertID int64) ([]AlertExternalRef, error)
	UpdateAlertExternalRef(ctx context.Context, alertID, channelID int64, incidentID, state, syncError string) error
}

// ErrInvalidDevice reports a device registration the store will not hold.
var ErrInvalidDevice = errors.New("invalid device registration")

func cleanDeviceInput(deviceID, platform, token string) (string, string, string, error) {
	deviceID, platform, token = strings.TrimSpace(deviceID), strings.TrimSpace(platform), strings.TrimSpace(token)
	if platform == "" {
		platform = "fcm"
	}
	if deviceID == "" || len(deviceID) > 128 || token == "" || len(token) > 4096 || platform != "fcm" {
		return "", "", "", ErrInvalidDevice
	}
	return deviceID, platform, token, nil
}

// ---- Postgres ----

func (p *Postgres) channelInWorkspace(ctx context.Context, channelID int64) (bool, error) {
	var ok bool
	err := p.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM channels WHERE id = $1 AND workspace_id = $2)`, channelID, workspaceID(ctx)).Scan(&ok)
	return ok, err
}

// UpsertPushDevice registers a device or refreshes its token. Re-registering
// a retired device id revives it, since the client is evidently live again.
func (p *Postgres) UpsertPushDevice(ctx context.Context, channelID int64, deviceID, platform, token string) error {
	deviceID, platform, token, err := cleanDeviceInput(deviceID, platform, token)
	if err != nil {
		return err
	}
	if ok, err := p.channelInWorkspace(ctx, channelID); err != nil {
		return err
	} else if !ok {
		return ErrNotFound
	}
	_, err = p.pool.Exec(ctx, `INSERT INTO push_devices (channel_id, workspace_id, device_id, platform, token)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (channel_id, device_id) DO UPDATE
		SET token = EXCLUDED.token, platform = EXCLUDED.platform, updated_at = now(), retired_at = NULL`,
		channelID, workspaceID(ctx), deviceID, platform, token)
	return err
}

func (p *Postgres) DeletePushDevice(ctx context.Context, channelID int64, deviceID string) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM push_devices WHERE channel_id = $1 AND device_id = $2 AND workspace_id = $3`, channelID, deviceID, workspaceID(ctx))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) ListPushDevices(ctx context.Context, channelID int64) ([]PushDevice, error) {
	rows, err := p.pool.Query(ctx, `SELECT channel_id, device_id, platform, token FROM push_devices
		WHERE channel_id = $1 AND workspace_id = $2 AND retired_at IS NULL ORDER BY id`, channelID, workspaceID(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PushDevice
	for rows.Next() {
		var d PushDevice
		if err := rows.Scan(&d.ChannelID, &d.DeviceID, &d.Platform, &d.Token); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (p *Postgres) RetirePushDevice(ctx context.Context, channelID int64, deviceID string) error {
	_, err := p.pool.Exec(ctx, `UPDATE push_devices SET retired_at = now(), updated_at = now()
		WHERE channel_id = $1 AND device_id = $2 AND workspace_id = $3 AND retired_at IS NULL`, channelID, deviceID, workspaceID(ctx))
	return err
}

func (p *Postgres) RecordAlertExternalRef(ctx context.Context, r *AlertExternalRef) error {
	// The insert selects through the alert, scoped to the caller's workspace,
	// so another tenant's alert id inserts nothing.
	tag, err := p.pool.Exec(ctx, `INSERT INTO alert_external_refs (alert_id, channel_id, workspace_id, provider, dedup_key)
		SELECT a.id, $2, $3, $4, $5 FROM alerts a WHERE a.id = $1 AND a.workspace_id = $3
		ON CONFLICT (alert_id, channel_id) DO UPDATE SET dedup_key = EXCLUDED.dedup_key, updated_at = now()`,
		r.AlertID, r.ChannelID, workspaceID(ctx), r.Provider, r.DedupKey)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) GetAlertExternalRefByDedupKey(ctx context.Context, channelID int64, provider, dedupKey string) (*AlertExternalRef, error) {
	var r AlertExternalRef
	err := p.pool.QueryRow(ctx, `SELECT alert_id, channel_id, provider, dedup_key, incident_id, state, sync_error FROM alert_external_refs
		WHERE channel_id = $1 AND provider = $2 AND dedup_key = $3 AND workspace_id = $4 ORDER BY alert_id DESC LIMIT 1`,
		channelID, provider, dedupKey, workspaceID(ctx)).Scan(&r.AlertID, &r.ChannelID, &r.Provider, &r.DedupKey, &r.IncidentID, &r.State, &r.SyncError)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (p *Postgres) ListAlertExternalRefs(ctx context.Context, alertID int64) ([]AlertExternalRef, error) {
	rows, err := p.pool.Query(ctx, `SELECT alert_id, channel_id, provider, dedup_key, incident_id, state, sync_error FROM alert_external_refs
		WHERE alert_id = $1 AND workspace_id = $2 ORDER BY channel_id`, alertID, workspaceID(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AlertExternalRef
	for rows.Next() {
		var r AlertExternalRef
		if err := rows.Scan(&r.AlertID, &r.ChannelID, &r.Provider, &r.DedupKey, &r.IncidentID, &r.State, &r.SyncError); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateAlertExternalRef records the provider's state. An empty incidentID
// keeps the stored one; state only moves forward, so a late or replayed
// event cannot reopen a resolved incident.
func (p *Postgres) UpdateAlertExternalRef(ctx context.Context, alertID, channelID int64, incidentID, state, syncError string) error {
	_, err := p.pool.Exec(ctx, `UPDATE alert_external_refs SET
		incident_id = CASE WHEN $3::text = '' THEN incident_id ELSE $3::text END,
		state = CASE WHEN $4::text = 'resolved' OR (state = '' AND $4::text = 'acknowledged') THEN $4::text ELSE state END,
		sync_error = $5, updated_at = now()
		WHERE alert_id = $1 AND channel_id = $2 AND workspace_id = $6`, alertID, channelID, incidentID, state, syncError, workspaceID(ctx))
	return err
}

// ---- SQLite ----

func (s *SQLite) channelInWorkspace(ctx context.Context, channelID int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM channels WHERE id = ? AND workspace_id = ?`, channelID, string(workspaceID(ctx))).Scan(&n)
	return n > 0, err
}

func (s *SQLite) UpsertPushDevice(ctx context.Context, channelID int64, deviceID, platform, token string) error {
	deviceID, platform, token, err := cleanDeviceInput(deviceID, platform, token)
	if err != nil {
		return err
	}
	if ok, err := s.channelInWorkspace(ctx, channelID); err != nil {
		return err
	} else if !ok {
		return ErrNotFound
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO push_devices (channel_id, workspace_id, device_id, platform, token)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (channel_id, device_id) DO UPDATE
		SET token = excluded.token, platform = excluded.platform, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), retired_at = NULL`,
		channelID, string(workspaceID(ctx)), deviceID, platform, token)
	return err
}

func (s *SQLite) DeletePushDevice(ctx context.Context, channelID int64, deviceID string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM push_devices WHERE channel_id = ? AND device_id = ? AND workspace_id = ?`, channelID, deviceID, string(workspaceID(ctx)))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLite) ListPushDevices(ctx context.Context, channelID int64) ([]PushDevice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT channel_id, device_id, platform, token FROM push_devices
		WHERE channel_id = ? AND workspace_id = ? AND retired_at IS NULL ORDER BY id`, channelID, string(workspaceID(ctx)))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []PushDevice
	for rows.Next() {
		var d PushDevice
		if err := rows.Scan(&d.ChannelID, &d.DeviceID, &d.Platform, &d.Token); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *SQLite) RetirePushDevice(ctx context.Context, channelID int64, deviceID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE push_devices SET retired_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE channel_id = ? AND device_id = ? AND workspace_id = ? AND retired_at IS NULL`, channelID, deviceID, string(workspaceID(ctx)))
	return err
}

func (s *SQLite) RecordAlertExternalRef(ctx context.Context, r *AlertExternalRef) error {
	res, err := s.db.ExecContext(ctx, `INSERT INTO alert_external_refs (alert_id, channel_id, workspace_id, provider, dedup_key)
		SELECT a.id, ?, ?, ?, ? FROM alerts a WHERE a.id = ? AND a.workspace_id = ?
		ON CONFLICT (alert_id, channel_id) DO UPDATE SET dedup_key = excluded.dedup_key, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`,
		r.ChannelID, string(workspaceID(ctx)), r.Provider, r.DedupKey, r.AlertID, string(workspaceID(ctx)))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLite) GetAlertExternalRefByDedupKey(ctx context.Context, channelID int64, provider, dedupKey string) (*AlertExternalRef, error) {
	var r AlertExternalRef
	err := s.db.QueryRowContext(ctx, `SELECT alert_id, channel_id, provider, dedup_key, incident_id, state, sync_error FROM alert_external_refs
		WHERE channel_id = ? AND provider = ? AND dedup_key = ? AND workspace_id = ? ORDER BY alert_id DESC LIMIT 1`,
		channelID, provider, dedupKey, string(workspaceID(ctx))).Scan(&r.AlertID, &r.ChannelID, &r.Provider, &r.DedupKey, &r.IncidentID, &r.State, &r.SyncError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *SQLite) ListAlertExternalRefs(ctx context.Context, alertID int64) ([]AlertExternalRef, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT alert_id, channel_id, provider, dedup_key, incident_id, state, sync_error FROM alert_external_refs
		WHERE alert_id = ? AND workspace_id = ? ORDER BY channel_id`, alertID, string(workspaceID(ctx)))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AlertExternalRef
	for rows.Next() {
		var r AlertExternalRef
		if err := rows.Scan(&r.AlertID, &r.ChannelID, &r.Provider, &r.DedupKey, &r.IncidentID, &r.State, &r.SyncError); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *SQLite) UpdateAlertExternalRef(ctx context.Context, alertID, channelID int64, incidentID, state, syncError string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE alert_external_refs SET
		incident_id = CASE WHEN ?1 = '' THEN incident_id ELSE ?1 END,
		state = CASE WHEN ?2 = 'resolved' OR (state = '' AND ?2 = 'acknowledged') THEN ?2 ELSE state END,
		sync_error = ?3, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE alert_id = ?4 AND channel_id = ?5 AND workspace_id = ?6`, incidentID, state, syncError, alertID, channelID, string(workspaceID(ctx)))
	return err
}
