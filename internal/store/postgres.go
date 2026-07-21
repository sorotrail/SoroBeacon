package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres implements Store on top of a pgx connection pool.
type Postgres struct {
	pool *pgxpool.Pool
}

var _ Store = (*Postgres)(nil)

// NewPostgres connects to databaseURL and verifies the connection.
// Call Migrate before using the store on a fresh database.
func NewPostgres(ctx context.Context, databaseURL string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }
func (p *Postgres) Close()                         { p.pool.Close() }

// --- monitors ---

func (p *Postgres) CreateMonitor(ctx context.Context, m *Monitor) error {
	ids, err := json.Marshal(m.ContractIDs)
	if err != nil {
		return err
	}
	return p.pool.QueryRow(ctx,
		`INSERT INTO monitors (name, contract_ids, enabled) VALUES ($1, $2, $3)
		 RETURNING id, created_at`,
		m.Name, ids, m.Enabled,
	).Scan(&m.ID, &m.CreatedAt)
}

func (p *Postgres) GetMonitor(ctx context.Context, id int64) (*Monitor, error) {
	m, err := scanMonitor(p.pool.QueryRow(ctx,
		`SELECT id, name, contract_ids, enabled, created_at FROM monitors WHERE id = $1`, id))
	if err != nil {
		return nil, err
	}
	rows, err := p.pool.Query(ctx,
		`SELECT channel_id FROM monitor_channels WHERE monitor_id = $1 ORDER BY channel_id`, id)
	if err != nil {
		return nil, err
	}
	m.ChannelIDs, err = pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (p *Postgres) ListMonitors(ctx context.Context, enabledOnly bool) ([]Monitor, error) {
	q := `SELECT id, name, contract_ids, enabled, created_at FROM monitors`
	if enabledOnly {
		q += ` WHERE enabled`
	}
	q += ` ORDER BY id`
	rows, err := p.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Monitor
	for rows.Next() {
		m, err := scanMonitor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (p *Postgres) UpdateMonitor(ctx context.Context, m *Monitor) error {
	ids, err := json.Marshal(m.ContractIDs)
	if err != nil {
		return err
	}
	tag, err := p.pool.Exec(ctx,
		`UPDATE monitors SET name = $2, contract_ids = $3, enabled = $4 WHERE id = $1`,
		m.ID, m.Name, ids, m.Enabled)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) DeleteMonitor(ctx context.Context, id int64) error {
	return p.deleteByID(ctx, "monitors", id)
}

func (p *Postgres) SetMonitorChannels(ctx context.Context, monitorID int64, channelIDs []int64) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	if _, err := tx.Exec(ctx, `DELETE FROM monitor_channels WHERE monitor_id = $1`, monitorID); err != nil {
		return err
	}
	for _, cid := range channelIDs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO monitor_channels (monitor_id, channel_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			monitorID, cid); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

type rowScanner interface{ Scan(dest ...any) error }

func scanMonitor(r rowScanner) (*Monitor, error) {
	var m Monitor
	var ids []byte
	if err := r.Scan(&m.ID, &m.Name, &ids, &m.Enabled, &m.CreatedAt); err != nil {
		return nil, mapErr(err)
	}
	if err := json.Unmarshal(ids, &m.ContractIDs); err != nil {
		return nil, fmt.Errorf("monitor %d: bad contract_ids: %w", m.ID, err)
	}
	return &m, nil
}

// --- rules ---

func (p *Postgres) CreateRule(ctx context.Context, r *Rule) error {
	return mapErr(p.pool.QueryRow(ctx,
		`INSERT INTO rules (monitor_id, type, params, enabled) VALUES ($1, $2, $3, $4) RETURNING id`,
		r.MonitorID, r.Type, jsonOrEmpty(r.Params), r.Enabled,
	).Scan(&r.ID))
}

func (p *Postgres) GetRule(ctx context.Context, id int64) (*Rule, error) {
	var r Rule
	err := p.pool.QueryRow(ctx,
		`SELECT id, monitor_id, type, params, enabled FROM rules WHERE id = $1`, id,
	).Scan(&r.ID, &r.MonitorID, &r.Type, &r.Params, &r.Enabled)
	if err != nil {
		return nil, mapErr(err)
	}
	return &r, nil
}

func (p *Postgres) ListRules(ctx context.Context, monitorID int64, enabledOnly bool) ([]Rule, error) {
	q := `SELECT id, monitor_id, type, params, enabled FROM rules WHERE monitor_id = $1`
	if enabledOnly {
		q += ` AND enabled`
	}
	q += ` ORDER BY id`
	rows, err := p.pool.Query(ctx, q, monitorID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Rule, error) {
		var r Rule
		err := row.Scan(&r.ID, &r.MonitorID, &r.Type, &r.Params, &r.Enabled)
		return r, err
	})
}

func (p *Postgres) UpdateRule(ctx context.Context, r *Rule) error {
	tag, err := p.pool.Exec(ctx,
		`UPDATE rules SET type = $2, params = $3, enabled = $4 WHERE id = $1`,
		r.ID, r.Type, jsonOrEmpty(r.Params), r.Enabled)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) DeleteRule(ctx context.Context, id int64) error {
	return p.deleteByID(ctx, "rules", id)
}

// --- channels ---

func (p *Postgres) CreateChannel(ctx context.Context, c *Channel) error {
	return p.pool.QueryRow(ctx,
		`INSERT INTO channels (name, type, config, enabled) VALUES ($1, $2, $3, $4)
		 RETURNING id, created_at`,
		c.Name, c.Type, jsonOrEmpty(c.Config), c.Enabled,
	).Scan(&c.ID, &c.CreatedAt)
}

func (p *Postgres) GetChannel(ctx context.Context, id int64) (*Channel, error) {
	var c Channel
	err := p.pool.QueryRow(ctx,
		`SELECT id, name, type, config, enabled, created_at FROM channels WHERE id = $1`, id,
	).Scan(&c.ID, &c.Name, &c.Type, &c.Config, &c.Enabled, &c.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &c, nil
}

func (p *Postgres) ListChannels(ctx context.Context) ([]Channel, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, name, type, config, enabled, created_at FROM channels ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanChannel)
}

func (p *Postgres) UpdateChannel(ctx context.Context, c *Channel) error {
	tag, err := p.pool.Exec(ctx,
		`UPDATE channels SET name = $2, type = $3, config = $4, enabled = $5 WHERE id = $1`,
		c.ID, c.Name, c.Type, jsonOrEmpty(c.Config), c.Enabled)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) DeleteChannel(ctx context.Context, id int64) error {
	return p.deleteByID(ctx, "channels", id)
}

func (p *Postgres) ListChannelsForMonitor(ctx context.Context, monitorID int64) ([]Channel, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT c.id, c.name, c.type, c.config, c.enabled, c.created_at
		 FROM channels c
		 JOIN monitor_channels mc ON mc.channel_id = c.id
		 WHERE mc.monitor_id = $1 AND c.enabled
		 ORDER BY c.id`, monitorID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanChannel)
}

func scanChannel(row pgx.CollectableRow) (Channel, error) {
	var c Channel
	err := row.Scan(&c.ID, &c.Name, &c.Type, &c.Config, &c.Enabled, &c.CreatedAt)
	return c, err
}

// --- alerts ---

func (p *Postgres) CreateAlert(ctx context.Context, a *Alert) (bool, error) {
	err := p.pool.QueryRow(ctx,
		`INSERT INTO alerts (monitor_id, rule_id, event_id, payload) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (rule_id, event_id) DO NOTHING
		 RETURNING id, created_at`,
		a.MonitorID, a.RuleID, a.EventID, jsonOrEmpty(a.Payload),
	).Scan(&a.ID, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // duplicate (rule_id, event_id): deduped
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (p *Postgres) ListAlerts(ctx context.Context, f AlertFilter) ([]Alert, error) {
	q := `SELECT id, monitor_id, rule_id, event_id, payload, created_at FROM alerts WHERE TRUE`
	args := []any{}
	n := 0
	arg := func(v any) string {
		n++
		args = append(args, v)
		return fmt.Sprintf("$%d", n)
	}
	if f.MonitorID != 0 {
		q += ` AND monitor_id = ` + arg(f.MonitorID)
	}
	if !f.From.IsZero() {
		q += ` AND created_at >= ` + arg(f.From)
	}
	if !f.To.IsZero() {
		q += ` AND created_at < ` + arg(f.To)
	}
	if f.AfterID != 0 {
		q += ` AND id < ` + arg(f.AfterID)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	q += ` ORDER BY id DESC LIMIT ` + arg(limit)

	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Alert, error) {
		var a Alert
		err := row.Scan(&a.ID, &a.MonitorID, &a.RuleID, &a.EventID, &a.Payload, &a.CreatedAt)
		return a, err
	})
}

func (p *Postgres) RecordDeliveryAttempt(ctx context.Context, d *DeliveryAttempt) error {
	return p.pool.QueryRow(ctx,
		`INSERT INTO delivery_attempts (alert_id, channel_id, status, response_snippet)
		 VALUES ($1, $2, $3, $4) RETURNING id, attempted_at`,
		d.AlertID, d.ChannelID, d.Status, d.ResponseSnippet,
	).Scan(&d.ID, &d.AttemptedAt)
}

func (p *Postgres) ListDeliveryAttempts(ctx context.Context, alertID int64) ([]DeliveryAttempt, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, alert_id, channel_id, status, response_snippet, attempted_at
		 FROM delivery_attempts WHERE alert_id = $1 ORDER BY id`, alertID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (DeliveryAttempt, error) {
		var d DeliveryAttempt
		err := row.Scan(&d.ID, &d.AlertID, &d.ChannelID, &d.Status, &d.ResponseSnippet, &d.AttemptedAt)
		return d, err
	})
}

// --- ingest state ---

func (p *Postgres) GetIngestState(ctx context.Context) (IngestState, error) {
	var s IngestState
	var lastLedger int64
	err := p.pool.QueryRow(ctx,
		`SELECT last_ledger, last_cursor, updated_at FROM ingest_state WHERE id = 1`,
	).Scan(&lastLedger, &s.LastCursor, &s.UpdatedAt)
	if err != nil {
		return s, mapErr(err)
	}
	s.LastLedger = uint32(lastLedger)
	return s, nil
}

func (p *Postgres) SetIngestState(ctx context.Context, s IngestState) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE ingest_state SET last_ledger = $1, last_cursor = $2, updated_at = now() WHERE id = 1`,
		int64(s.LastLedger), s.LastCursor)
	return err
}

// --- stats ---

func (p *Postgres) GetStats(ctx context.Context) (Stats, error) {
	var s Stats
	err := p.pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM monitors),
			(SELECT count(*) FROM rules),
			(SELECT count(*) FROM channels),
			(SELECT count(*) FROM alerts),
			(SELECT count(*) FROM alerts WHERE created_at > now() - interval '24 hours'),
			(SELECT last_ledger FROM ingest_state WHERE id = 1),
			(SELECT updated_at FROM ingest_state WHERE id = 1)`,
	).Scan(&s.Monitors, &s.Rules, &s.Channels, &s.Alerts, &s.AlertsLast24, &s.LastLedger, &s.LastPollAt)
	return s, err
}

// --- helpers ---

func (p *Postgres) deleteByID(ctx context.Context, table string, id int64) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM `+table+` WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func jsonOrEmpty(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return []byte(`{}`)
	}
	return raw
}

// mapErr converts pgx sentinel and FK errors into store-level errors.
func mapErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" { // foreign_key_violation
		return fmt.Errorf("%w: %s", ErrNotFound, pgErr.ConstraintName)
	}
	return err
}
