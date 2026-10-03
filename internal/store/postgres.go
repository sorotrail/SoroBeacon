package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/telemetry"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// PoolSettings tunes the pgx connection pool, and the read routing the
// Postgres backend layers on top of it. A zero value in any field leaves the
// corresponding pgx default in place so deployments that do not set the env
// vars keep the same behaviour as before.
//
// The SQLite backend ignores this struct entirely: it has no pool, and it
// serves reads and writes from the same file handle, so there is no replica to
// route to.
type PoolSettings struct {
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	// ReplicaURL is a second Postgres connection string used for the
	// read-only queries replica.go routes (REPLICA_DATABASE_URL). Empty — the
	// default, and every deployment that does not set it — keeps all reads on
	// the primary.
	//
	// It rides in this struct rather than a constructor argument because
	// store.New already threads PoolSettings from config to the Postgres
	// backend, and a replica URL is pool selection in exactly the same sense
	// the primary's own URL is.
	ReplicaURL string
	// Metrics receives the per-pool read counters that make routing visible.
	// Nil records nothing, which is what tests pass and what an embedder with
	// no instrumentation wants; every method is nil-safe, see internal/metrics.
	Metrics *metrics.Metrics
}

// Postgres implements Store on top of a pgx connection pool.
type Postgres struct {
	pool *pgxpool.Pool
	// replica serves the read-only queries listed in replica.go. Nil (the
	// default) means REPLICA_DATABASE_URL was unset and every query — read or
	// write — runs on pool, which is the behaviour of every deployment that
	// predates replica routing.
	replica *pgxpool.Pool
	// cipher encrypts and decrypts channels.config at rest. Nil (the
	// default) keeps the pre-encryption plaintext behaviour.
	cipher ConfigCipher
	// telemetry is optional tracing; nil (the default) writes no spans.
	telemetry *telemetry.Provider
	// metrics is optional instrumentation for routed reads; nil-safe.
	metrics *metrics.Metrics
}

// WithTelemetry attaches tracing to the store's write paths. Only ids go
// into span attributes; the alert payload and channel config never do.
func (p *Postgres) WithTelemetry(t *telemetry.Provider) *Postgres {
	p.telemetry = t
	return p
}

var _ Store = (*Postgres)(nil)

// WithConfigCipher sets the cipher used to encrypt Channel.Config at rest
// and returns the store for chaining. Call it before serving traffic; a nil
// cipher (or never calling it) stores config as plaintext. With no cipher,
// existing plaintext rows keep working unchanged.
func (p *Postgres) WithConfigCipher(c ConfigCipher) *Postgres {
	p.cipher = c
	return p
}

// NewPostgres connects to databaseURL and verifies the connection.
// Call Migrate before using the store on a fresh database. Pass a zero
// PoolSettings to keep pgx's own pool defaults.
//
// With settings.ReplicaURL set it also opens the replica pool and routes the
// reads replica.go lists to it; a replica that cannot be opened or pinged is a
// startup error, see connectReplica.
func NewPostgres(ctx context.Context, databaseURL string, settings PoolSettings) (*Postgres, error) {
	cfg, err := buildPoolConfig(databaseURL, settings)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	p := &Postgres{pool: pool, metrics: settings.Metrics}
	if err := p.connectReplica(ctx, settings); err != nil {
		pool.Close()
		return nil, err
	}
	return p, nil
}

// buildPoolConfig parses databaseURL and overlays any non-zero pool
// settings. Zero means "leave the pgx default" so unset env vars do
// not change MaxConns / MinConns / lifetimes.
func buildPoolConfig(databaseURL string, settings PoolSettings) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	if settings.MaxConns > 0 {
		cfg.MaxConns = settings.MaxConns
	}
	if settings.MinConns > 0 {
		cfg.MinConns = settings.MinConns
	}
	if settings.MaxConnLifetime > 0 {
		cfg.MaxConnLifetime = settings.MaxConnLifetime
	}
	if settings.MaxConnIdleTime > 0 {
		cfg.MaxConnIdleTime = settings.MaxConnIdleTime
	}
	return cfg, nil
}

// Ping checks the primary only. It is the readiness probe's dependency, and
// the question readiness asks is "can this process serve traffic?" — which it
// can while the primary is up, because a replica that is down is covered by
// the fallback in replica.go. Pinging the replica here would take the instance
// out of rotation over a read replica, turning a degradable condition into an
// outage. A replica that is failing shows up as
// sorobeacon_store_replica_fallbacks_total instead.
func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

// Close releases both pools. The replica goes first so a query cannot be
// issued against a pool that is being torn down mid-shutdown.
func (p *Postgres) Close() {
	if p.replica != nil {
		p.replica.Close()
	}
	p.pool.Close()
}

// pageLimit matches ListAlerts: a missing or out-of-range limit becomes
// 50 rather than being rejected, so omitting pagination params still
// returns a bounded first page.
func pageLimit(limit int) int {
	if limit <= 0 || limit > 500 {
		return 50
	}
	return limit
}

// --- workspaces ---

// EnsureWorkspace implements the Workspaces half of Store. See the interface
// for why this is the only write to the table.
func (p *Postgres) EnsureWorkspace(ctx context.Context, id workspace.ID) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO workspaces (id, name) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`,
		string(id), string(id))
	return err
}

// --- monitors ---

func (p *Postgres) CreateMonitor(ctx context.Context, m *Monitor) error {
	ids, err := json.Marshal(m.ContractIDs)
	if err != nil {
		return err
	}
	return p.pool.QueryRow(ctx,
		`INSERT INTO monitors (name, contract_ids, enabled, priority, workspace_id, network) VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, created_at`,
		m.Name, ids, m.Enabled, m.Priority.Normalized(), workspaceID(ctx), m.Network,
	).Scan(&m.ID, &m.CreatedAt)
}

func (p *Postgres) GetMonitor(ctx context.Context, id int64) (*Monitor, error) {
	ws := workspaceID(ctx)
	m, err := scanMonitor(p.pool.QueryRow(ctx,
		`SELECT id, name, contract_ids, enabled, created_at, last_matched_at, priority, network FROM monitors WHERE id = $1 AND workspace_id = $2`, id, ws))
	if err != nil {
		return nil, err
	}
	m.ChannelIDs, err = p.monitorChannelIDs(ctx, id)
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (p *Postgres) monitorChannelIDs(ctx context.Context, monitorID int64) ([]int64, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT channel_id FROM monitor_channels WHERE monitor_id = $1 ORDER BY channel_id`, monitorID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// ListMonitors is the poller's watch list as well as the dashboard's, so it
// honours the cross-tenant system scope: an ingest loop must see every
// workspace's enabled monitors or monitors in a second tenant would silently
// stop being polled.
func (p *Postgres) ListMonitors(ctx context.Context, enabledOnly bool) ([]Monitor, error) {
	q := `SELECT id, name, contract_ids, enabled, created_at, last_matched_at, priority, network FROM monitors WHERE TRUE`
	args := []any{}
	ws, scoped := tenantWorkspace(ctx)
	if scoped {
		args = append(args, ws)
		q += ` AND workspace_id = $1`
	}
	if enabledOnly {
		q += ` AND enabled`
	}
	q += ` ORDER BY id`
	rows, err := p.pool.Query(ctx, q, args...)
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

func (p *Postgres) ListMonitorsPage(ctx context.Context, f ListFilter) ([]Monitor, error) {
	q := `SELECT id, name, contract_ids, enabled, created_at, last_matched_at, priority, network FROM monitors WHERE workspace_id = $1`
	args := []any{workspaceID(ctx)}
	n := 1
	arg := func(v any) string {
		n++
		args = append(args, v)
		return fmt.Sprintf("$%d", n)
	}
	if f.Query != "" {
		// strpos + lower is a parameterized substring match without LIKE
		// metacharacters, so a search for "100%" cannot become a wildcard.
		q += ` AND strpos(lower(name), lower(` + arg(f.Query) + `)) > 0`
	}
	if f.Network != "" {
		q += ` AND network = ` + arg(f.Network)
	}
	switch {
	case f.Enabled != nil && *f.Enabled:
		q += ` AND enabled`
	case f.Enabled != nil && !*f.Enabled:
		q += ` AND NOT enabled`
	case f.EnabledOnly:
		q += ` AND enabled`
	}
	sort := monitorSort(f.Sort)
	if f.AfterID != 0 {
		// The cursor row is read under the same workspace predicate as the page,
		// so an id belonging to another tenant cannot even be used to probe that
		// tenant's ordering.
		ws := workspaceID(ctx)
		switch sort {
		case "name":
			q += ` AND (lower(name), id) > (SELECT lower(name), id FROM monitors WHERE id = ` + arg(f.AfterID) + ` AND workspace_id = ` + arg(ws) + `)`
		case "created_at":
			q += ` AND (created_at, id) < (SELECT created_at, id FROM monitors WHERE id = ` + arg(f.AfterID) + ` AND workspace_id = ` + arg(ws) + `)`
		default:
			q += ` AND id < ` + arg(f.AfterID)
		}
	}
	switch sort {
	case "name":
		q += ` ORDER BY lower(name) ASC, id ASC`
	case "created_at":
		q += ` ORDER BY created_at DESC, id DESC`
	default:
		q += ` ORDER BY id DESC`
	}
	q += ` LIMIT ` + arg(pageLimit(f.Limit))
	rows, err := p.queryRows(ctx, q, args...)
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

// monitorSort maps ListFilter.Sort onto the allowlist. Unknown / empty
// values become "name" so a typo cannot change the ORDER BY shape.
func monitorSort(s string) string {
	switch s {
	case "id", "created_at", "name":
		return s
	default:
		return "name"
	}
}

func (p *Postgres) UpdateMonitor(ctx context.Context, m *Monitor) error {
	ids, err := json.Marshal(m.ContractIDs)
	if err != nil {
		return err
	}
	tag, err := p.pool.Exec(ctx,
		`UPDATE monitors SET name = $3, contract_ids = $4, enabled = $5, priority = $6 WHERE id = $1 AND workspace_id = $2`,
		m.ID, workspaceID(ctx), m.Name, ids, m.Enabled, m.Priority.Normalized())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func uniqueIDs(ids []int64) []int64 {
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func (p *Postgres) SetMonitorsEnabled(ctx context.Context, ids []int64, enabled bool) (int, []int64, error) {
	uniq := uniqueIDs(ids)
	if len(uniq) == 0 {
		return 0, []int64{}, nil
	}
	rows, err := p.pool.Query(ctx,
		`UPDATE monitors SET enabled = $1 WHERE id = ANY($2) AND workspace_id = $3 RETURNING id`,
		enabled, uniq, workspaceID(ctx))
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	found := make(map[int64]struct{}, len(uniq))
	updated := 0
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, nil, err
		}
		found[id] = struct{}{}
		updated++
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	unknown := make([]int64, 0)
	for _, id := range uniq {
		if _, ok := found[id]; !ok {
			unknown = append(unknown, id)
		}
	}
	return updated, unknown, nil
}

func (p *Postgres) DeleteMonitor(ctx context.Context, id int64) error {
	return p.deleteByID(ctx, "monitors", id)
}

func (p *Postgres) DuplicateMonitor(ctx context.Context, id int64) (*Monitor, error) {
	ws := workspaceID(ctx)
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	src, err := scanMonitor(tx.QueryRow(ctx,
		`SELECT id, name, contract_ids, enabled, created_at, last_matched_at, priority, network FROM monitors WHERE id = $1 AND workspace_id = $2`, id, ws))
	if err != nil {
		return nil, err
	}
	// Both sides of the attachment are checked, not just the monitor. A row
	// attached before tenancy existed (or by a path that has since been scoped)
	// must not be carried into the copy: that would move a channel this tenant
	// cannot see — and whose webhook target it therefore should not start
	// firing — onto a monitor it can. The list read here is the list copied, so
	// the returned snapshot cannot promise an attachment the copy lacks.
	chRows, err := tx.Query(ctx,
		`SELECT mc.channel_id FROM monitor_channels mc
		   JOIN monitors m ON m.id = mc.monitor_id
		   JOIN channels c ON c.id = mc.channel_id
		  WHERE mc.monitor_id = $1 AND m.workspace_id = $2 AND c.workspace_id = $2
		  ORDER BY mc.channel_id`, id, ws)
	if err != nil {
		return nil, err
	}
	src.ChannelIDs, err = pgx.CollectRows(chRows, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	ruleRows, err := tx.Query(ctx,
		`SELECT r.type, r.params, r.enabled FROM rules r
		   JOIN monitors m ON m.id = r.monitor_id
		  WHERE r.monitor_id = $1 AND m.workspace_id = $2 ORDER BY r.id`, id, ws)
	if err != nil {
		return nil, err
	}
	rules, err := pgx.CollectRows(ruleRows, func(row pgx.CollectableRow) (Rule, error) {
		var r Rule
		err := row.Scan(&r.Type, &r.Params, &r.Enabled)
		return r, err
	})
	if err != nil {
		return nil, err
	}
	// Name uniqueness is a workspace question, not a global one: two teams may
	// each have a monitor called "Vault minter", and a copy must not be forced
	// to "(copy 2)" because another tenant used the plain name first.
	nameRows, err := tx.Query(ctx, `SELECT name FROM monitors WHERE workspace_id = $1`, ws)
	if err != nil {
		return nil, err
	}
	names, err := pgx.CollectRows(nameRows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}

	ids, err := json.Marshal(src.ContractIDs)
	if err != nil {
		return nil, err
	}
	copy := Monitor{
		Name:        CopyMonitorName(src.Name, names),
		ContractIDs: src.ContractIDs,
		Enabled:     false,                     // never inherit enabled: a duplicate must be reviewed first
		Priority:    src.Priority.Normalized(), // priority is queue position, not a safety switch, so the copy keeps it
		// The network is not a reviewable setting: the copy watches the same
		// contract ids, which only exist on the source's chain.
		Network: src.Network,
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO monitors (name, contract_ids, enabled, priority, workspace_id, network) VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, created_at`,
		copy.Name, ids, copy.Enabled, copy.Priority, ws, copy.Network,
	).Scan(&copy.ID, &copy.CreatedAt); err != nil {
		return nil, err
	}
	for _, r := range rules {
		if _, err := tx.Exec(ctx,
			`INSERT INTO rules (monitor_id, type, params, enabled) VALUES ($1, $2, $3, $4)`,
			copy.ID, r.Type, jsonOrEmpty(r.Params), r.Enabled); err != nil {
			return nil, err
		}
	}
	for _, cid := range src.ChannelIDs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO monitor_channels (monitor_id, channel_id) VALUES ($1, $2)`,
			copy.ID, cid); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	copy.ChannelIDs = src.ChannelIDs
	return &copy, nil
}

func (p *Postgres) SetMonitorChannels(ctx context.Context, monitorID int64, channelIDs []int64) error {
	ws := workspaceID(ctx)
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	// Both sides must belong to the caller's workspace: the monitor check is
	// what stops one tenant re-pointing another's monitor, and the channel check
	// is what stops a tenant attaching a channel it cannot see, which would
	// route this workspace's alerts to a webhook owned by someone else.
	if err := p.requireMonitorWorkspace(ctx, tx, monitorID, ws); err != nil {
		return err
	}
	if uniq := uniqueIDs(channelIDs); len(uniq) > 0 {
		var count int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM channels WHERE id = ANY($1) AND workspace_id = $2`,
			uniq, ws).Scan(&count); err != nil {
			return err
		}
		if count != len(uniq) {
			return ErrNotFound
		}
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM monitor_channels
		  WHERE monitor_id IN (SELECT id FROM monitors WHERE id = $1 AND workspace_id = $2)`,
		monitorID, ws); err != nil {
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
	var priority string
	if err := r.Scan(&m.ID, &m.Name, &ids, &m.Enabled, &m.CreatedAt, &m.LastMatchedAt, &priority, &m.Network); err != nil {
		return nil, mapErr(err)
	}
	m.Priority = Priority(priority).Normalized()
	if err := json.Unmarshal(ids, &m.ContractIDs); err != nil {
		return nil, fmt.Errorf("monitor %d: bad contract_ids: %w", m.ID, err)
	}
	return &m, nil
}

// --- rules ---
//
// Rules carry no workspace column of their own: their monitor_id foreign key
// is the tenant link, so every rule query here reaches monitors. Denormalising
// the workspace onto rules would give a row two ways to say who owns it, and
// they can disagree.

// requireMonitorWorkspace fails with ErrNotFound unless monitorID is a monitor
// in ws. It accepts either the pool or an open transaction so CreateRule,
// CreateRules and SetMonitorChannels share one check. A monitor the caller
// cannot see fails exactly as a monitor that does not exist does, so the two
// are indistinguishable to the caller.
func (p *Postgres) requireMonitorWorkspace(ctx context.Context, q rowQueryer, monitorID int64, ws workspace.ID) error {
	var ok bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM monitors WHERE id = $1 AND workspace_id = $2)`,
		monitorID, ws).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// rowQueryer is the one method both *pgxpool.Pool and pgx.Tx provide for a
// single-row read, so a helper can run inside or outside a transaction.
type rowQueryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (p *Postgres) CreateRule(ctx context.Context, r *Rule) error {
	if err := p.requireMonitorWorkspace(ctx, p.pool, r.MonitorID, workspaceID(ctx)); err != nil {
		return err
	}
	return mapErr(p.pool.QueryRow(ctx,
		`INSERT INTO rules (monitor_id, type, params, enabled) VALUES ($1, $2, $3, $4) RETURNING id`,
		r.MonitorID, r.Type, jsonOrEmpty(r.Params), r.Enabled,
	).Scan(&r.ID))
}

func (p *Postgres) CreateRules(ctx context.Context, rules []*Rule) error {
	if len(rules) == 0 {
		return nil
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	ws := workspaceID(ctx)
	for _, r := range rules {
		if err := p.requireMonitorWorkspace(ctx, tx, r.MonitorID, ws); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO rules (monitor_id, type, params, enabled) VALUES ($1, $2, $3, $4) RETURNING id`,
			r.MonitorID, r.Type, jsonOrEmpty(r.Params), r.Enabled,
		).Scan(&r.ID); err != nil {
			return mapErr(err)
		}
	}
	return tx.Commit(ctx)
}

func (p *Postgres) GetRule(ctx context.Context, id int64) (*Rule, error) {
	var r Rule
	err := p.pool.QueryRow(ctx,
		`SELECT r.id, r.monitor_id, r.type, r.params, r.enabled FROM rules r
		   JOIN monitors m ON m.id = r.monitor_id
		  WHERE r.id = $1 AND m.workspace_id = $2`, id, workspaceID(ctx),
	).Scan(&r.ID, &r.MonitorID, &r.Type, &r.Params, &r.Enabled)
	if err != nil {
		return nil, mapErr(err)
	}
	return &r, nil
}

func (p *Postgres) ListRules(ctx context.Context, monitorID int64, enabledOnly bool) ([]Rule, error) {
	// The predicate is conditional because ListRules serves two callers with
	// different scopes: the API (one workspace) and the poller's per-monitor
	// rule loading, which runs under the cross-tenant system scope and would
	// otherwise find no rules for a monitor outside the default workspace.
	q := `SELECT r.id, r.monitor_id, r.type, r.params, r.enabled FROM rules r
	         JOIN monitors m ON m.id = r.monitor_id
	        WHERE r.monitor_id = $1`
	args := []any{monitorID}
	if ws, scoped := tenantWorkspace(ctx); scoped {
		q += ` AND m.workspace_id = $2`
		args = append(args, ws)
	}
	if enabledOnly {
		q += ` AND r.enabled`
	}
	q += ` ORDER BY r.id`
	rows, err := p.pool.Query(ctx, q, args...)
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
		`UPDATE rules SET type = $3, params = $4, enabled = $5 WHERE id = $1
		   AND monitor_id IN (SELECT id FROM monitors WHERE workspace_id = $2)`,
		r.ID, workspaceID(ctx), r.Type, jsonOrEmpty(r.Params), r.Enabled)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) DeleteRule(ctx context.Context, id int64) error {
	tag, err := p.pool.Exec(ctx,
		`DELETE FROM rules r USING monitors m
		  WHERE r.id = $1 AND m.id = r.monitor_id AND m.workspace_id = $2`,
		id, workspaceID(ctx))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// --- absence-of-event state ---

func (p *Postgres) ListAbsenceState(ctx context.Context) ([]AbsenceState, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT rule_id, event_name, last_seen_at FROM rule_absence_state`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (AbsenceState, error) {
		var s AbsenceState
		err := row.Scan(&s.RuleID, &s.EventName, &s.LastSeen)
		return s, err
	})
}

func (p *Postgres) RecordAbsenceSeen(ctx context.Context, ruleID int64, eventName string, at time.Time) error {
	// GREATEST is what makes the clock monotonic in one statement: the sweep
	// and the rearm path both write here, and neither may move a clock that a
	// newer event already advanced.
	_, err := p.pool.Exec(ctx,
		`INSERT INTO rule_absence_state (rule_id, event_name, last_seen_at, updated_at)
		 VALUES ($1, $2, $3, now())
		 ON CONFLICT (rule_id, event_name) DO UPDATE
		 SET last_seen_at = GREATEST(rule_absence_state.last_seen_at, EXCLUDED.last_seen_at),
		     updated_at   = now()`,
		ruleID, eventName, at.UTC())
	return err
}

// --- channels ---

func (p *Postgres) CreateChannel(ctx context.Context, c *Channel) error {
	config, err := configForWrite(p.cipher, c.ID, c.Name, c.Config)
	if err != nil {
		return err
	}
	return p.pool.QueryRow(ctx,
		`INSERT INTO channels (name, type, config, enabled, digest_mode, digest_window_seconds, timeout, workspace_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING id, created_at`,
		c.Name, c.Type, config, c.Enabled, c.DigestMode, c.DigestWindowSeconds, c.TimeoutSeconds(), workspaceID(ctx),
	).Scan(&c.ID, &c.CreatedAt)
}

func (p *Postgres) GetChannel(ctx context.Context, id int64) (*Channel, error) {
	var c Channel
	var timeoutSec int
	err := p.pool.QueryRow(ctx,
		`SELECT id, name, type, config, enabled, created_at, digest_mode, digest_window_seconds, timeout,
		        consecutive_failures, consecutive_permanent_failures, last_error,
		        last_error_at, last_success_at, disabled_at
		 FROM channels WHERE id = $1 AND workspace_id = $2`, id, workspaceID(ctx),
	).Scan(&c.ID, &c.Name, &c.Type, &c.Config, &c.Enabled, &c.CreatedAt, &c.DigestMode, &c.DigestWindowSeconds, &timeoutSec,
		&c.ConsecutiveFailures, &c.ConsecutivePermanentFailures, &c.LastError,
		&c.LastErrorAt, &c.LastSuccessAt, &c.DisabledAt)
	if err != nil {
		return nil, mapErr(err)
	}
	c.Timeout = time.Duration(timeoutSec) * time.Second
	if err := decryptChannel(p.cipher, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// ListChannels serves the dashboard listing and the notifier's startup
// validation, so it honours the cross-tenant system scope like ListMonitors.
func (p *Postgres) ListChannels(ctx context.Context, enabledOnly bool) ([]Channel, error) {
	q := `SELECT id, name, type, config, enabled, created_at, digest_mode, digest_window_seconds, timeout,
		        consecutive_failures, consecutive_permanent_failures, last_error,
		        last_error_at, last_success_at, disabled_at FROM channels WHERE TRUE`
	args := []any{}
	if ws, scoped := tenantWorkspace(ctx); scoped {
		args = append(args, ws)
		q += ` AND workspace_id = $1`
	}
	if enabledOnly {
		q += ` AND enabled`
	}
	q += ` ORDER BY id`
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, p.scanChannel)
}

func (p *Postgres) ListChannelsPage(ctx context.Context, f ListFilter) ([]Channel, error) {
	q := `SELECT id, name, type, config, enabled, created_at, digest_mode, digest_window_seconds, timeout,
		        consecutive_failures, consecutive_permanent_failures, last_error,
		        last_error_at, last_success_at, disabled_at FROM channels WHERE TRUE`
	args := []any{}
	n := 0
	arg := func(v any) string {
		n++
		args = append(args, v)
		return fmt.Sprintf("$%d", n)
	}
	q += ` AND workspace_id = ` + arg(workspaceID(ctx))
	if f.EnabledOnly {
		q += ` AND enabled`
	}
	if f.Type != "" {
		q += ` AND type = ` + arg(f.Type)
	}
	if f.AfterID != 0 {
		q += ` AND id < ` + arg(f.AfterID)
	}
	q += ` ORDER BY id DESC LIMIT ` + arg(pageLimit(f.Limit))
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, p.scanChannel)
}

func (p *Postgres) UpdateChannel(ctx context.Context, c *Channel) error {
	config, err := configForWrite(p.cipher, c.ID, c.Name, c.Config)
	if err != nil {
		return err
	}
	// Turning a channel back on clears the health state that auto-disable
	// set, in the same statement, so the channel cannot re-disable on the
	// next failure because of counters accumulated before it was fixed. The
	// `NOT enabled` test reads the pre-update value, so this fires on a
	// genuine off-to-on transition only: renaming a channel that is still
	// failing must not quietly wipe the evidence. This write is the one way
	// back from auto-disable, which is what makes re-enabling explicit.
	tag, err := p.pool.Exec(ctx,
		`UPDATE channels SET
		   name = $2, type = $3, config = $4, enabled = $5,
		   digest_mode = $6, digest_window_seconds = $7, timeout = $8,
		   consecutive_failures = CASE WHEN $5 AND NOT enabled THEN 0 ELSE consecutive_failures END,
		   consecutive_permanent_failures = CASE WHEN $5 AND NOT enabled THEN 0 ELSE consecutive_permanent_failures END,
		   last_error = CASE WHEN $5 AND NOT enabled THEN '' ELSE last_error END,
		   last_error_at = CASE WHEN $5 AND NOT enabled THEN NULL ELSE last_error_at END,
		   disabled_at = CASE WHEN $5 AND NOT enabled THEN NULL ELSE disabled_at END
		 WHERE id = $1 AND workspace_id = $9`,
		c.ID, c.Name, c.Type, config, c.Enabled, c.DigestMode, c.DigestWindowSeconds, c.TimeoutSeconds(), workspaceID(ctx))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordChannelHealth applies one delivery outcome to a channel's health
// counters. Everything happens in a single statement so concurrent
// dispatches cannot lose an increment, and the auto-disable decision is made
// against the value the row actually has rather than one read earlier.
func (p *Postgres) RecordChannelHealth(ctx context.Context, channelID int64, u ChannelHealthUpdate) error {
	if u.At.IsZero() {
		u.At = time.Now()
	}
	permanent := u.Permanent && !u.Success
	if _, err := p.pool.Exec(ctx,
		`UPDATE channels SET
		   consecutive_failures = CASE WHEN $2 THEN 0 ELSE consecutive_failures + 1 END,
		   consecutive_permanent_failures = CASE
		     WHEN $2 THEN 0
		     WHEN $3 THEN consecutive_permanent_failures + 1
		     ELSE consecutive_permanent_failures
		   END,
		   -- The casts are load-bearing: without them the parameter is offered a
		   -- NULL branch to resolve against and Postgres reads $5 as text.
		   last_error = CASE WHEN $2 THEN '' ELSE $4::text END,
		   last_error_at = CASE WHEN $2 THEN NULL ELSE $5::timestamptz END,
		   last_success_at = CASE WHEN $2 THEN $5::timestamptz ELSE last_success_at END,
		   -- A success clears the auto-disable marker only once the channel is
		   -- actually back on, so a test send through a still-disabled channel
		   -- cannot make the dashboard claim an operator turned it off.
		   disabled_at = CASE
		     WHEN $2 THEN CASE WHEN enabled THEN NULL ELSE disabled_at END
		     WHEN $3 AND $6::int > 0 AND consecutive_permanent_failures + 1 >= $6::int THEN COALESCE(disabled_at, $5::timestamptz)
		     ELSE disabled_at
		   END,
		   enabled = CASE
		     WHEN $2 THEN enabled
		     WHEN $3 AND $6::int > 0 AND consecutive_permanent_failures + 1 >= $6::int THEN false
		     ELSE enabled
		   END
		 WHERE id = $1`,
		channelID, u.Success, permanent, u.Error, u.At, u.DisableAfter); err != nil {
		return err
	}
	return nil
}

// DeleteChannel removes a channel, refusing when an escalation policy step
// still references it. The reference is checked explicitly so the caller gets
// ErrChannelInUse (mapped to a 409) rather than a raw foreign-key error; the
// migration's ON DELETE RESTRICT is the backstop for a concurrent policy write.
func (p *Postgres) DeleteChannel(ctx context.Context, id int64) error {
	var inUse bool
	if err := p.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM escalation_step_channels WHERE channel_id = $1)`, id,
	).Scan(&inUse); err != nil {
		return err
	}
	if inUse {
		return ErrChannelInUse
	}
	return p.deleteByID(ctx, "channels", id)
}

func (p *Postgres) ListMonitorsForChannel(ctx context.Context, channelID int64) ([]Monitor, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT m.id, m.name, m.contract_ids, m.enabled, m.created_at, m.last_matched_at, m.priority
		 FROM monitors m
		 JOIN monitor_channels mc ON mc.monitor_id = m.id
		 WHERE mc.channel_id = $1 AND m.workspace_id = $2
		 ORDER BY m.id`, channelID, workspaceID(ctx))
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
		m.ChannelIDs, err = p.monitorChannelIDs(ctx, m.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (p *Postgres) ListChannelsForMonitor(ctx context.Context, monitorID int64) ([]Channel, error) {
	// Scoped when the caller names a tenant, open when it does not: the
	// dispatcher delivers in the poller's cross-tenant context for a monitor
	// it already resolved, while the dashboard and the API must not see another
	// workspace's attachments.
	scopeClause := ""
	args := []any{monitorID}
	if ws, scoped := tenantWorkspace(ctx); scoped {
		scopeClause = ` AND m.workspace_id = $2`
		args = append(args, ws)
	}
	rows, err := p.pool.Query(ctx,
		`SELECT c.id, c.name, c.type, c.config, c.enabled, c.created_at, c.digest_mode, c.digest_window_seconds, c.timeout,
		        c.consecutive_failures, c.consecutive_permanent_failures, c.last_error,
		        c.last_error_at, c.last_success_at, c.disabled_at
		 FROM channels c
		 JOIN monitor_channels mc ON mc.channel_id = c.id
		 JOIN monitors m ON m.id = mc.monitor_id
		 WHERE mc.monitor_id = $1 AND c.enabled`+scopeClause+`
		 ORDER BY c.id`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, p.scanChannel)
}

// ListChannelsByIDs returns the enabled channels among ids, ordered by id, so
// an escalation step can resolve its channel set in one query. Disabled
// channels are omitted rather than erroring, matching ListChannelsForMonitor:
// a paused destination simply stops receiving alerts.
func (p *Postgres) ListChannelsByIDs(ctx context.Context, ids []int64) ([]Channel, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := p.pool.Query(ctx,
		`SELECT id, name, type, config, enabled, created_at, digest_mode, digest_window_seconds, timeout,
		        consecutive_failures, consecutive_permanent_failures, last_error,
		        last_error_at, last_success_at, disabled_at FROM channels
		  WHERE id = ANY($1) AND enabled ORDER BY id`, uniqueIDs(ids))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, p.scanChannel)
}

// scanChannel reads one channels row and decrypts its config, so every
// caller up the stack (API, dashboard, dispatcher) sees plaintext.
func (p *Postgres) scanChannel(row pgx.CollectableRow) (Channel, error) {
	var c Channel
	var timeoutSec int
	if err := row.Scan(&c.ID, &c.Name, &c.Type, &c.Config, &c.Enabled, &c.CreatedAt, &c.DigestMode, &c.DigestWindowSeconds, &timeoutSec,
		&c.ConsecutiveFailures, &c.ConsecutivePermanentFailures, &c.LastError,
		&c.LastErrorAt, &c.LastSuccessAt, &c.DisabledAt); err != nil {
		return c, err
	}
	c.Timeout = time.Duration(timeoutSec) * time.Second
	if err := decryptChannel(p.cipher, &c); err != nil {
		return c, err
	}
	return c, nil
}

// --- alerts ---

// CreateAlert wraps the transaction with the store.create_alert span, so
// the "was it the database write?" half of the slow-alert question has a
// timeline. The outcome (created | duplicate | suppressed) is an attribute,
// turning the dedup and cooldown gates into something visible per trace.
// Only row ids land in attributes; the payload can embed operator data and
// stays out.
func (p *Postgres) CreateAlert(ctx context.Context, a *Alert) (outcome AlertOutcome, err error) {
	if p.telemetry == nil {
		return p.createAlert(ctx, a)
	}
	ctx, span := p.telemetry.WithRequestID(ctx, "store.create_alert",
		trace.WithAttributes(
			attribute.Int64(telemetry.AttrMonitorID, a.MonitorID),
			attribute.Int64(telemetry.AttrRuleID, a.RuleID),
			attribute.String(telemetry.AttrEventID, a.EventID),
		),
	)
	defer func() {
		if err != nil {
			telemetry.RecordError(span, err)
		} else {
			telemetry.SetAttrs(span, telemetry.AttrOutcome, string(outcome))
		}
		span.End()
	}()
	return p.createAlert(ctx, a)
}

func (p *Postgres) createAlert(ctx context.Context, a *Alert) (AlertOutcome, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	// The rule and the monitor are separate foreign keys, so the database
	// alone cannot stop an alert that names this tenant's monitor and another
	// tenant's rule. The join is the check, and it runs inside the same
	// transaction as the insert. A system context skips it: the poller
	// resolved both from one monitor it already read.
	if ws, scoped := tenantWorkspace(ctx); scoped {
		var ok bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (
			   SELECT 1 FROM rules r
			     JOIN monitors m ON m.id = r.monitor_id
			    WHERE r.id = $1 AND r.monitor_id = $2 AND m.workspace_id = $3)`,
			a.RuleID, a.MonitorID, ws).Scan(&ok); err != nil {
			return "", mapErr(err)
		}
		if !ok {
			return "", ErrNotFound
		}
	}

	// The cooldown decision is taken under a lock on the rule row, so two
	// poller instances cannot both fire inside one window. A zero cooldown
	// skips all of it and leaves the dedup guard as the only gate, exactly as
	// before this option existed.
	if a.Cooldown > 0 {
		var suppressed int64
		var inCooldown bool
		err := tx.QueryRow(ctx,
			`SELECT suppressed_since_last,
			        COALESCE(last_alert_at + make_interval(secs => $2) > now(), false)
			   FROM rules WHERE id = $1 FOR UPDATE`,
			a.RuleID, a.Cooldown.Seconds()).Scan(&suppressed, &inCooldown)
		if err != nil {
			return "", mapErr(err)
		}

		// A replayed event is a duplicate, not a fresh match, so it must not
		// inflate the suppressed count. The dedup guard lives in alert_dedup
		// (the partitioned alerts table cannot carry its unique index).
		var duplicate bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM alert_dedup WHERE rule_id = $1 AND event_id = $2)`,
			a.RuleID, a.EventID).Scan(&duplicate); err != nil {
			return "", err
		}
		if duplicate {
			return AlertDuplicate, nil
		}

		if inCooldown {
			if _, err := tx.Exec(ctx,
				`UPDATE rules SET suppressed_since_last = suppressed_since_last + 1 WHERE id = $1`,
				a.RuleID); err != nil {
				return "", err
			}
			if err := tx.Commit(ctx); err != nil {
				return "", err
			}
			return AlertSuppressed, nil
		}

		// The count is read before the insert so it can ride on the payload
		// the alert is written with, and the window is closed at the same
		// time: one transaction, so a crash cannot lose a suppression count or
		// leave the window open.
		a.SuppressedSinceLast = suppressed
		a.Payload = WithSuppressed(a.Payload, suppressed)
	}

	// Reserve the dedup key before inserting. This is the race-safe guard that
	// the unique index on alerts used to provide: a conflicting reservation
	// means another writer already took this event, and the transaction is
	// rolled back, so a failed alert insert cannot leave a stranded key.
	var reserved int
	err = tx.QueryRow(ctx,
		`INSERT INTO alert_dedup (rule_id, event_id, alert_created_at) VALUES ($1, $2, now())
		 ON CONFLICT (rule_id, event_id) DO NOTHING
		 RETURNING 1`,
		a.RuleID, a.EventID).Scan(&reserved)
	if errors.Is(err, pgx.ErrNoRows) {
		return AlertDuplicate, nil // duplicate (rule_id, event_id): deduped
	}
	if err != nil {
		return "", err
	}

	err = tx.QueryRow(ctx,
		// The network is read from the monitor rather than taken from the
		// caller: a contract id only means anything on the chain it was
		// deployed to, so the monitor owns the answer and an alert cannot be
		// filed against a chain its monitor does not watch.
		`INSERT INTO alerts (monitor_id, rule_id, event_id, payload, enrichment, backfilled, ledger, network, workspace_id)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, (SELECT network FROM monitors WHERE id = $1), $8)
			 RETURNING id, created_at, network`,
		a.MonitorID, a.RuleID, a.EventID, jsonOrEmpty(a.Payload), nullableJSON(a.Enrichment), a.Backfilled, int64(a.Ledger),
		workspaceID(ctx),
	).Scan(&a.ID, &a.CreatedAt, &a.Network)
	if err != nil {
		return "", mapErr(err)
	}

	// Link the reserved key to the row it produced, and carry the alert's
	// created_at so retention can drop dedup rows by time.
	if _, err := tx.Exec(ctx,
		`UPDATE alert_dedup SET alert_id = $1, alert_created_at = $2 WHERE rule_id = $3 AND event_id = $4`,
		a.ID, a.CreatedAt, a.RuleID, a.EventID); err != nil {
		return "", err
	}

	if a.Cooldown > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE rules SET last_alert_at = now(), suppressed_since_last = 0 WHERE id = $1`,
			a.RuleID); err != nil {
			return "", err
		}
	}

	// Stamp last_matched_at with the event's ledger close time, never wall
	// clock. Only move the column forward so a replayed older event cannot
	// make a live monitor look stale.
	if !a.LedgerClosedAt.IsZero() {
		if _, err := tx.Exec(ctx,
			`UPDATE monitors SET last_matched_at = $1
			 WHERE id = $2 AND (last_matched_at IS NULL OR last_matched_at < $1)`,
			a.LedgerClosedAt.UTC(), a.MonitorID); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return AlertCreated, nil
}

// CreateAlertGroup creates or increments the alert group identified
// by key and windowStart. On conflict it increments the count; on
// first insertion it initializes count to 1. Returns the new count.
func (p *Postgres) CreateAlertGroup(ctx context.Context, key string, windowStart time.Time) (int64, error) {
	var count int64
	err := p.pool.QueryRow(ctx,
		`INSERT INTO alert_groups (group_key, window_start, count, first_alert_id)
		 VALUES ($1, $2, 1, NULL)
		 ON CONFLICT (group_key, window_start) DO UPDATE
		 SET count = alert_groups.count + 1
		 RETURNING count`,
		key, windowStart,
	).Scan(&count)
	if err != nil {
		return 0, mapErr(err)
	}
	return count, nil
}

// GroupAlerts creates or increments the alert group for key with
// windowStart and returns whether the alert should be delivered
// immediately (first alert in the window) and the current count.
func (p *Postgres) GroupAlerts(ctx context.Context, key string, windowStart time.Time) (shouldDeliver bool, currentCount int64, err error) {
	count, err := p.CreateAlertGroup(ctx, key, windowStart)
	if err != nil {
		return false, 0, err
	}
	return count == 1, count, nil
}

func (p *Postgres) GetAlert(ctx context.Context, id int64) (*Alert, error) {
	var a Alert
	var ledger int64
	err := p.pool.QueryRow(ctx,
		`SELECT id, monitor_id, rule_id, event_id, payload, enrichment, created_at, ledger, retracted_at, backfilled, inhibited_by_rule_id, suppressed, suppression_reason, acknowledged_at, network
		   FROM alerts WHERE id = $1 AND workspace_id = $2`, id, workspaceID(ctx),
	).Scan(&a.ID, &a.MonitorID, &a.RuleID, &a.EventID, &a.Payload, &a.Enrichment, &a.CreatedAt, &ledger, &a.RetractedAt, &a.Backfilled, &a.InhibitedByRuleID, &a.Suppressed, &a.SuppressionReason, &a.AcknowledgedAt, &a.Network)
	if err != nil {
		return nil, mapErr(err)
	}
	a.Ledger = uint32(ledger)
	return &a, nil
}

// alertSort maps AlertFilter.Sort onto the allowlist. Unknown / empty
// values become created_at_desc so a typo cannot change the ORDER BY shape.
func alertSort(s string) string {
	if s == "created_at_asc" {
		return "created_at_asc"
	}
	return "created_at_desc"
}

// ListAlerts searches alerts. It is a routable read: the alert list is a
// search over history, so a replica a few milliseconds behind shows the caller
// the same page minus rows written in that window, which is the trade the
// replica exists to make. A caller that cannot tolerate it — the rules engine
// rebuilding a match log — uses ListAlertsPrimary.
func (p *Postgres) ListAlerts(ctx context.Context, f AlertFilter) ([]Alert, error) {
	q, args := buildAlertQuery(ctx, f)
	rows, err := p.queryRows(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanAlert)
}

// ListAlertsPrimary is ListAlerts served by the primary, whatever
// REPLICA_DATABASE_URL says. It exists for readers that must see this
// process's own writes — see PrimaryReader.
func (p *Postgres) ListAlertsPrimary(ctx context.Context, f AlertFilter) ([]Alert, error) {
	q, args := buildAlertQuery(ctx, f)
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanAlert)
}

// CreateDeadLetter records a delivery that exhausted its retry budget.
func (p *Postgres) CreateDeadLetter(ctx context.Context, d *DeadLetter) error {
	// alert_created_at is read from the alert rather than passed in: the
	// composite foreign key needs it, and taking it from the row means a
	// caller cannot supply one that does not match. A missing alert selects
	// no rows and comes back as ErrNotFound.
	return mapErr(p.pool.QueryRow(ctx,
		`INSERT INTO dead_letters (alert_id, alert_created_at, channel_id, last_error, attempt_count, last_status)
		 SELECT a.id, a.created_at, $2, $3, $4, $5 FROM alerts a WHERE a.id = $1
		 RETURNING id, created_at`,
		d.AlertID, d.ChannelID, d.LastError, d.AttemptCount, d.LastStatus,
	).Scan(&d.ID, &d.CreatedAt))
}

// GetDeadLetter returns one dead-letter record by id.
func (p *Postgres) GetDeadLetter(ctx context.Context, id int64) (*DeadLetter, error) {
	var d DeadLetter
	err := p.pool.QueryRow(ctx,
		`SELECT id, alert_id, channel_id, last_error, attempt_count, last_status, created_at
		 FROM dead_letters WHERE id = $1`, id,
	).Scan(&d.ID, &d.AlertID, &d.ChannelID, &d.LastError, &d.AttemptCount, &d.LastStatus, &d.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &d, nil
}

// ListDeadLetters returns dead-letter records in id order, applying the
// optional channel, alert, and pagination filters.
func (p *Postgres) ListDeadLetters(ctx context.Context, f DeadLetterFilter) ([]DeadLetter, error) {
	q := `SELECT id, alert_id, channel_id, last_error, attempt_count, last_status, created_at
		FROM dead_letters WHERE TRUE`
	args := []any{}
	n := 0
	arg := func(v any) string {
		n++
		args = append(args, v)
		return fmt.Sprintf("$%d", n)
	}
	if f.ChannelID != 0 {
		q += ` AND channel_id = ` + arg(f.ChannelID)
	}
	if f.AlertID != 0 {
		q += ` AND alert_id = ` + arg(f.AlertID)
	}
	if f.AfterID != 0 {
		q += ` AND id > ` + arg(f.AfterID)
	}
	q += ` ORDER BY id ASC LIMIT ` + arg(pageLimit(f.Limit))

	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (DeadLetter, error) {
		var d DeadLetter
		err := row.Scan(&d.ID, &d.AlertID, &d.ChannelID, &d.LastError, &d.AttemptCount, &d.LastStatus, &d.CreatedAt)
		return d, err
	})
}

// DeleteDeadLetter removes one record, which is what a successful redrive does.
// It does not go through deleteByID: dead_letters carries no workspace_id of
// its own, so the tenant predicate reaches it through the alert it belongs to.
// A caller without a tenant (the dispatcher's own context) deletes by id alone.
func (p *Postgres) DeleteDeadLetter(ctx context.Context, id int64) error {
	q := `DELETE FROM dead_letters WHERE id = $1`
	args := []any{id}
	if ws, scoped := tenantWorkspace(ctx); scoped {
		q += ` AND EXISTS (SELECT 1 FROM alerts a WHERE a.id = dead_letters.alert_id AND a.workspace_id = $2)`
		args = append(args, ws)
	}
	tag, err := p.pool.Exec(ctx, q, args...)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// buildAlertQuery builds the ListAlerts statement and its arguments. Both
// readers call it so the routed and primary-bound forms cannot drift into
// returning different pages.
func buildAlertQuery(ctx context.Context, f AlertFilter) (string, []any) {
	q := `SELECT id, monitor_id, rule_id, event_id, payload, enrichment, created_at, ledger, retracted_at, backfilled, inhibited_by_rule_id, suppressed, suppression_reason, acknowledged_at, network
		 FROM alerts WHERE TRUE`
	args := []any{}
	n := 0
	arg := func(v any) string {
		n++
		args = append(args, v)
		return fmt.Sprintf("$%d", n)
	}
	// Conditional because ListAlerts serves two callers with different scopes:
	// the API listing (one tenant) and the frequency rule's match-log rebuild
	// (which runs inside the poller's cross-tenant context, keyed to one rule).
	if ws, scoped := tenantWorkspace(ctx); scoped {
		q += ` AND workspace_id = ` + arg(ws)
	}
	if f.MonitorID != 0 {
		q += ` AND monitor_id = ` + arg(f.MonitorID)
	}
	if f.RuleID != 0 {
		q += ` AND rule_id = ` + arg(f.RuleID)
	}
	if f.ContractID != "" {
		q += ` AND payload->>'contract_id' = ` + arg(f.ContractID)
	}
	if f.Network != "" {
		q += ` AND network = ` + arg(f.Network)
	}
	if pattern := AlertSearchPattern(f.Query); pattern != "" {
		// One bound pattern, both columns. The payload is cast to text so the
		// search reaches contract_id, event_name and every other field a rule
		// type stores without the store knowing each type's shape — the cost
		// of that generality is that it cannot use an index, which is why the
		// term is capped (MaxAlertSearchLen) rather than long by default.
		// ESCAPE makes the pattern's own wildcards literal.
		p := arg(pattern)
		q += ` AND (event_id ILIKE ` + p + ` ESCAPE '\' OR payload::text ILIKE ` + p + ` ESCAPE '\')`
	}
	if !f.From.IsZero() {
		q += ` AND created_at >= ` + arg(f.From)
	}
	if !f.To.IsZero() {
		q += ` AND created_at < ` + arg(f.To)
	}
	sort := alertSort(f.Sort)
	if f.AfterID != 0 {
		// Subquery the cursor row so the comparison uses the same
		// (created_at, id) pair the ORDER BY does. The inequality
		// flips with direction; a one-sided id < AfterID would
		// skip/repeat once two rows share a timestamp.
		cursor := `(SELECT created_at, id FROM alerts WHERE id = ` + arg(f.AfterID) + `)`
		if sort == "created_at_asc" {
			q += ` AND (created_at, id) > ` + cursor
		} else {
			q += ` AND (created_at, id) < ` + cursor
		}
	}
	if sort == "created_at_asc" {
		q += ` ORDER BY created_at ASC, id ASC`
	} else {
		q += ` ORDER BY created_at DESC, id DESC`
	}
	q += ` LIMIT ` + arg(pageLimit(f.Limit))
	return q, args
}

// ListAlertsStream streams alerts matching the filter, calling the callback
// for each one. This is a simple implementation that loads all alerts and
// iterates; a true streaming implementation would use a cursor.
// scanAlert reads one alerts row. It is shared by ListAlerts and ExpiredAlerts
// so the column order and the ledger/retracted_at mapping cannot drift.
func scanAlert(row pgx.CollectableRow) (Alert, error) {
	var a Alert
	var ledger int64
	err := row.Scan(&a.ID, &a.MonitorID, &a.RuleID, &a.EventID, &a.Payload, &a.Enrichment, &a.CreatedAt, &ledger, &a.RetractedAt, &a.Backfilled, &a.InhibitedByRuleID, &a.Suppressed, &a.SuppressionReason, &a.AcknowledgedAt, &a.Network)
	a.Ledger = uint32(ledger)
	return a, err
}

// ListAlertsStream streams alerts matching the filter to the callback.
// It is used for large exports where loading all rows into memory is not feasible.
// ExpiredAlerts returns up to limit alerts older than cutoff, oldest first,
// with the same ordering DeleteExpiredAlerts uses so the row an archiver reads
// is the row the delete removes.
func (p *Postgres) ExpiredAlerts(ctx context.Context, cutoff time.Time, limit int) ([]Alert, error) {
	if limit <= 0 {
		limit = DefaultPruneBatch
	}
	rows, err := p.pool.Query(ctx,
		`SELECT id, monitor_id, rule_id, event_id, payload, enrichment, created_at, ledger, retracted_at, backfilled, inhibited_by_rule_id, suppressed, suppression_reason, acknowledged_at, network
		   FROM alerts WHERE created_at < $1 ORDER BY created_at ASC, id ASC LIMIT $2`,
		cutoff, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanAlert)
}

// --- ledger hashes and reorg retraction ---

// The empty network is the pre-multi-network case and keeps its original
// single-column ledger_hashes table; every named network shares
// network_ledger_hashes, whose primary key is (network, ledger). Two chains
// number their ledgers independently, so one window would report a divergence
// on almost every cycle — mainnet ledger 100 overwriting testnet ledger 100 is
// not a reorg — and a false positive here retracts real alerts.
//
// A named network therefore starts with an empty window. That is safe rather
// than lossy: reorg detection compares a hash it has stored, so a ledger with
// no stored row is recorded and skipped, never read as a divergence.

func (p *Postgres) RecordLedgerHashes(ctx context.Context, network string, hashes []LedgerHash) error {
	if len(hashes) == 0 {
		return nil
	}
	const legacyInsert = `
		INSERT INTO ledger_hashes (ledger, hash) VALUES ($1, $2)
		ON CONFLICT (ledger) DO UPDATE SET hash = EXCLUDED.hash, observed_at = now()
		WHERE ledger_hashes.hash IS DISTINCT FROM EXCLUDED.hash`
	const scopedInsert = `
		INSERT INTO network_ledger_hashes (network, ledger, hash) VALUES ($1, $2, $3)
		ON CONFLICT (network, ledger) DO UPDATE SET hash = EXCLUDED.hash, observed_at = now()
		WHERE network_ledger_hashes.hash IS DISTINCT FROM EXCLUDED.hash`

	batch := &pgx.Batch{}
	for _, h := range hashes {
		if network == "" {
			batch.Queue(legacyInsert, int64(h.Ledger), h.Hash)
			continue
		}
		batch.Queue(scopedInsert, network, int64(h.Ledger), h.Hash)
	}
	br := p.pool.SendBatch(ctx, batch)
	defer br.Close() //nolint:errcheck // errors surface on the per-command Exec below
	for range hashes {
		if _, err := br.Exec(); err != nil {
			return err
		}
	}
	return nil
}

func (p *Postgres) LedgerHashes(ctx context.Context, network string, from, to uint32) ([]LedgerHash, error) {
	query := `SELECT ledger, hash FROM network_ledger_hashes WHERE network = $1 AND ledger >= $2 AND ledger <= $3 ORDER BY ledger`
	args := []any{network, int64(from), int64(to)}
	if network == "" {
		query = `SELECT ledger, hash FROM ledger_hashes WHERE ledger >= $1 AND ledger <= $2 ORDER BY ledger`
		args = args[1:]
	}
	rows, err := p.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (LedgerHash, error) {
		var h LedgerHash
		var ledger int64
		err := row.Scan(&ledger, &h.Hash)
		h.Ledger = uint32(ledger)
		return h, err
	})
}

func (p *Postgres) PruneLedgerHashes(ctx context.Context, network string, before uint32) error {
	if network == "" {
		_, err := p.pool.Exec(ctx, `DELETE FROM ledger_hashes WHERE ledger < $1`, int64(before))
		return err
	}
	_, err := p.pool.Exec(ctx,
		`DELETE FROM network_ledger_hashes WHERE network = $1 AND ledger < $2`,
		network, int64(before))
	return err
}

// RetractAlertsFromLedger retracts one network's alerts from `ledger` up. The
// network predicate is what stops a testnet reorg from retracting mainnet
// alerts that happen to sit at the same height.
func (p *Postgres) RetractAlertsFromLedger(ctx context.Context, network string, ledger uint32, at time.Time) (int64, error) {
	tag, err := p.pool.Exec(ctx,
		`UPDATE alerts SET retracted_at = $1 WHERE network = $3 AND ledger >= $2 AND retracted_at IS NULL`,
		at.UTC(), int64(ledger), network)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (p *Postgres) RecordDeliveryAttempt(ctx context.Context, d *DeliveryAttempt) error {
	// The FK to the partitioned alerts table is (alert_id, alert_created_at),
	// so the parent's created_at is read in the same statement. A missing
	// alert yields no row, which mapErr turns into ErrNotFound just as the
	// old single-column FK violation did.
	// The dispatcher records attempts from the poller's cross-tenant context,
	// so the workspace predicate is conditional; the alert id is already the
	// caller's own, having come from a scoped read.
	q := `INSERT INTO delivery_attempts (alert_id, alert_created_at, channel_id, status, response_snippet)
		 SELECT a.id, a.created_at, $2, $3, $4 FROM alerts a WHERE a.id = $1`
	args := []any{d.AlertID, d.ChannelID, d.Status, d.ResponseSnippet}
	if ws, scoped := tenantWorkspace(ctx); scoped {
		q += ` AND a.workspace_id = $5`
		args = append(args, ws)
	}
	return mapErr(p.pool.QueryRow(ctx, q+`
		 RETURNING id, attempted_at`, args...).Scan(&d.ID, &d.AttemptedAt))
}

// ListDeliveryAttempts scopes through its alert: delivery_attempts carries no
// workspace column, and an alert id only means anything inside the tenant that
// can see the alert behind it. The join includes created_at because that pair
// is the alerts primary key once the table is partitioned.
func (p *Postgres) ListDeliveryAttempts(ctx context.Context, alertID int64, status string) ([]DeliveryAttempt, error) {
	q := `SELECT da.id, da.alert_id, da.channel_id, da.status, da.response_snippet, da.attempted_at
		 FROM delivery_attempts da
		 JOIN alerts a ON a.id = da.alert_id AND a.created_at = da.alert_created_at
		 WHERE da.alert_id = $1 AND a.workspace_id = $2`
	args := []any{alertID, workspaceID(ctx)}
	if status != "" {
		// Applied in SQL so a busy alert does not ship every attempt just
		// so the client can throw most of them away.
		q += ` AND da.status = $3`
		args = append(args, status)
	}
	q += ` ORDER BY da.id`
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (DeliveryAttempt, error) {
		var d DeliveryAttempt
		err := row.Scan(&d.ID, &d.AlertID, &d.ChannelID, &d.Status, &d.ResponseSnippet, &d.AttemptedAt)
		return d, err
	})
}

// --- maintenance windows ---

func (p *Postgres) CreateMaintenanceWindow(ctx context.Context, w *MaintenanceWindow) error {
	return mapErr(p.pool.QueryRow(ctx,
		`INSERT INTO maintenance_windows (reason, scope, monitor_id, contract_id, start_at, end_at)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, created_at`,
		w.Reason, w.Scope, w.MonitorID, w.ContractID, w.StartAt.UTC(), w.EndAt.UTC(),
	).Scan(&w.ID, &w.CreatedAt))
}

func (p *Postgres) GetMaintenanceWindow(ctx context.Context, id int64) (*MaintenanceWindow, error) {
	return scanMaintenanceWindow(p.pool.QueryRow(ctx,
		`SELECT id, reason, scope, monitor_id, contract_id, start_at, end_at, created_at
		 FROM maintenance_windows WHERE id = $1`, id))
}

func (p *Postgres) ListMaintenanceWindows(ctx context.Context, f MaintenanceWindowFilter) ([]MaintenanceWindow, error) {
	q := `SELECT id, reason, scope, monitor_id, contract_id, start_at, end_at, created_at
		 FROM maintenance_windows WHERE TRUE`
	args := []any{}
	n := 0
	arg := func(v any) string {
		n++
		args = append(args, v)
		return fmt.Sprintf("$%d", n)
	}
	if f.Active || f.Upcoming {
		at := f.At
		if at.IsZero() {
			at = time.Now()
		}
		if f.Active {
			q += ` AND start_at <= ` + arg(at.UTC()) + ` AND end_at > ` + arg(at.UTC())
		} else {
			q += ` AND start_at > ` + arg(at.UTC())
		}
	}
	q += ` ORDER BY start_at DESC, id DESC LIMIT ` + arg(pageLimit(f.Limit))
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (MaintenanceWindow, error) {
		w, err := scanMaintenanceWindow(row)
		if err != nil {
			return MaintenanceWindow{}, err
		}
		return *w, nil
	})
}

func (p *Postgres) UpdateMaintenanceWindow(ctx context.Context, w *MaintenanceWindow) error {
	tag, err := p.pool.Exec(ctx,
		`UPDATE maintenance_windows
		 SET reason = $2, scope = $3, monitor_id = $4, contract_id = $5, start_at = $6, end_at = $7
		 WHERE id = $1`,
		w.ID, w.Reason, w.Scope, w.MonitorID, w.ContractID, w.StartAt.UTC(), w.EndAt.UTC())
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteMaintenanceWindow removes one window. It does not go through
// deleteByID: that helper scopes its delete to the caller's workspace, and
// maintenance_windows carries no workspace_id. A window's scope is global,
// monitor or contract — it is instance-level under this model, the same way
// its listing and its creation are, so the delete matches them rather than
// inventing a tenancy the rest of the feature does not have.
func (p *Postgres) DeleteMaintenanceWindow(ctx context.Context, id int64) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM maintenance_windows WHERE id = $1`, id)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ActiveMaintenanceWindow is the delivery path's single indexed lookup: a
// window is active when [start_at, end_at) covers at and its scope covers
// the alert. The most specific scope wins (contract, then monitor, then
// global) so a contract's reason is preferred over a broader one's.
func (p *Postgres) ActiveMaintenanceWindow(ctx context.Context, monitorID int64, contractID string, at time.Time) (*MaintenanceWindow, error) {
	w, err := scanMaintenanceWindow(p.pool.QueryRow(ctx,
		`SELECT id, reason, scope, monitor_id, contract_id, start_at, end_at, created_at
		 FROM maintenance_windows
		 WHERE start_at <= $1 AND end_at > $1
		   AND (scope = 'global'
		        OR (scope = 'monitor' AND monitor_id = $2)
		        OR (scope = 'contract' AND contract_id = $3))
		 ORDER BY CASE scope WHEN 'contract' THEN 0 WHEN 'monitor' THEN 1 ELSE 2 END, start_at DESC
		 LIMIT 1`, at.UTC(), monitorID, contractID))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return w, nil
}

func (p *Postgres) SetAlertSuppressed(ctx context.Context, alertID int64, reason string) error {
	tag, err := p.pool.Exec(ctx,
		`UPDATE alerts SET suppressed = TRUE, suppression_reason = $2 WHERE id = $1`,
		alertID, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// --- inhibitions ---

func (p *Postgres) CreateInhibition(ctx context.Context, in *Inhibition) error {
	if in.FiringWindowSeconds <= 0 {
		in.FiringWindowSeconds = DefaultInhibitionWindowSeconds
	}
	return p.pool.QueryRow(ctx,
		`INSERT INTO alert_inhibitions (source_rule_id, target_rule_id, firing_window_seconds)
		 VALUES ($1, $2, $3) RETURNING created_at`,
		in.SourceRuleID, in.TargetRuleID, in.FiringWindowSeconds,
	).Scan(&in.CreatedAt)
}

func (p *Postgres) scanInhibition(row pgx.CollectableRow) (Inhibition, error) {
	var in Inhibition
	err := row.Scan(&in.SourceRuleID, &in.TargetRuleID, &in.FiringWindowSeconds, &in.CreatedAt)
	return in, err
}

func (p *Postgres) ListInhibitions(ctx context.Context) ([]Inhibition, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT source_rule_id, target_rule_id, firing_window_seconds, created_at
		 FROM alert_inhibitions ORDER BY source_rule_id, target_rule_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, p.scanInhibition)
}

func (p *Postgres) ListInhibitionsForTarget(ctx context.Context, targetRuleID int64) ([]Inhibition, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT source_rule_id, target_rule_id, firing_window_seconds, created_at
		 FROM alert_inhibitions WHERE target_rule_id = $1 ORDER BY source_rule_id`, targetRuleID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, p.scanInhibition)
}

func (p *Postgres) DeleteInhibition(ctx context.Context, sourceRuleID, targetRuleID int64) error {
	tag, err := p.pool.Exec(ctx,
		`DELETE FROM alert_inhibitions WHERE source_rule_id = $1 AND target_rule_id = $2`,
		sourceRuleID, targetRuleID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanMaintenanceWindow(r rowScanner) (*MaintenanceWindow, error) {
	var w MaintenanceWindow
	err := r.Scan(&w.ID, &w.Reason, &w.Scope, &w.MonitorID, &w.ContractID, &w.StartAt, &w.EndAt, &w.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &w, nil
}

func (p *Postgres) RuleFiredWithin(ctx context.Context, ruleID int64, window time.Duration) (bool, error) {
	// The cutoff is computed in Go so both backends share the decision;
	// Postgres compares timestamptz, SQLite compares the fixed-format TEXT.
	cutoff := time.Now().UTC().Truncate(time.Millisecond).Add(-window)
	var fired bool
	err := p.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM alerts WHERE rule_id = $1 AND created_at >= $2)`,
		ruleID, cutoff).Scan(&fired)
	return fired, err
}

func (p *Postgres) MarkAlertInhibited(ctx context.Context, alertID, sourceRuleID int64) error {
	// No row check: the alert may have been pruned between dispatch and
	// this write, and that must not fail the dispatch path.
	_, err := p.pool.Exec(ctx,
		`UPDATE alerts SET inhibited_by_rule_id = $1 WHERE id = $2`, sourceRuleID, alertID)
	return err
}

// --- ingest state ---

// GetIngestState reads one network's checkpoint. The empty network is the
// pre-multi-network row and behaves exactly as it always did; a named network
// reads network_ingest_state, which is seeded from that legacy row the first
// time any network asks. Seeding is what makes the upgrade transparent: an
// instance that moves from NETWORK=testnet to a network list resumes at the
// ledger it stopped on instead of cold-starting at the tip and silently missing
// everything in between.
func (p *Postgres) GetIngestState(ctx context.Context, network string) (IngestState, error) {
	var s IngestState
	if network == "" {
		var lastLedger int64
		err := p.pool.QueryRow(ctx,
			`SELECT last_ledger, last_cursor, updated_at FROM ingest_state WHERE id = 1`,
		).Scan(&lastLedger, &s.LastCursor, &s.UpdatedAt)
		s.LastLedger = uint32(lastLedger)
		return s, mapErr(err)
	}

	// Only while the per-network table is empty does the legacy checkpoint get
	// claimed, so a second network added later cold-starts on its own chain
	// rather than inheriting a cursor from a chain that has nothing to do with
	// it. Both statements are DO NOTHING, so two pollers starting at the same
	// moment cannot race into an error.
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO network_ingest_state (network, last_ledger, last_cursor)
		SELECT $1, last_ledger, last_cursor FROM ingest_state
		 WHERE id = 1 AND NOT EXISTS (SELECT 1 FROM network_ingest_state)
		ON CONFLICT (network) DO NOTHING`, network); err != nil {
		return s, err
	}
	if _, err := p.pool.Exec(ctx,
		`INSERT INTO network_ingest_state (network) VALUES ($1) ON CONFLICT (network) DO NOTHING`,
		network); err != nil {
		return s, err
	}

	var lastLedger int64
	err := p.pool.QueryRow(ctx,
		`SELECT last_ledger, last_cursor, updated_at FROM network_ingest_state WHERE network = $1`,
		network).Scan(&lastLedger, &s.LastCursor, &s.UpdatedAt)
	if err != nil {
		return s, mapErr(err)
	}
	s.LastLedger = uint32(lastLedger)
	return s, nil
}

// SetIngestState advances one network's checkpoint. Writing a named network is
// an upsert because the row may not exist yet: the first cycle of a newly
// configured network has nothing to update.
func (p *Postgres) SetIngestState(ctx context.Context, network string, s IngestState) error {
	if network == "" {
		_, err := p.pool.Exec(ctx,
			`UPDATE ingest_state SET last_ledger = $1, last_cursor = $2, updated_at = now() WHERE id = 1`,
			int64(s.LastLedger), s.LastCursor)
		return err
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO network_ingest_state (network, last_ledger, last_cursor, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (network) DO UPDATE
		    SET last_ledger = EXCLUDED.last_ledger,
		        last_cursor = EXCLUDED.last_cursor,
		        updated_at  = EXCLUDED.updated_at`,
		network, int64(s.LastLedger), s.LastCursor)
	return err
}

// --- digest queue ---

func (p *Postgres) PushDigestAlert(ctx context.Context, channelID int64, payload json.RawMessage) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO pending_digests (channel_id, payload) VALUES ($1, $2)`, channelID, payload)
	return err
}

func (p *Postgres) ListDigestAlerts(ctx context.Context, channelID int64) ([]DigestAlert, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, channel_id, payload, created_at FROM pending_digests WHERE channel_id = $1 ORDER BY id`, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DigestAlert
	for rows.Next() {
		var d DigestAlert
		var payload []byte
		if err := rows.Scan(&d.ID, &d.ChannelID, &payload, &d.CreatedAt); err != nil {
			return nil, err
		}
		d.Payload = json.RawMessage(payload)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (p *Postgres) DeleteDigestAlerts(ctx context.Context, channelID int64, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := p.pool.Exec(ctx,
		`DELETE FROM pending_digests WHERE channel_id = $1 AND id = ANY($2)`, channelID, ids)
	return err
}

// --- audit log ---

func (p *Postgres) CreateAuditEntry(ctx context.Context, e *AuditEntry) error {
	diff := e.Diff
	if len(diff) == 0 {
		diff = json.RawMessage(`{}`)
	}
	return p.pool.QueryRow(ctx,
		`INSERT INTO audit_log (actor, action, target_type, target_id, diff) VALUES ($1, $2, $3, $4, $5) RETURNING id, created_at`,
		e.Actor, e.Action, e.TargetType, e.TargetID, diff).Scan(&e.ID, &e.CreatedAt)
}

func (p *Postgres) ListAuditEntries(ctx context.Context, f AuditFilter) ([]AuditEntry, error) {
	where := []string{"TRUE"}
	args := []any{}
	if f.TargetType != "" {
		args = append(args, f.TargetType)
		where = append(where, fmt.Sprintf("target_type = $%d", len(args)))
	}
	if f.TargetID != 0 {
		args = append(args, f.TargetID)
		where = append(where, fmt.Sprintf("target_id = $%d", len(args)))
	}
	if !f.From.IsZero() {
		args = append(args, f.From)
		where = append(where, fmt.Sprintf("created_at >= $%d", len(args)))
	}
	if !f.To.IsZero() {
		args = append(args, f.To)
		where = append(where, fmt.Sprintf("created_at <= $%d", len(args)))
	}
	limit := clampAuditLimit(f.Limit)
	args = append(args, limit)
	q := `SELECT id, actor, action, target_type, target_id, diff, created_at FROM audit_log WHERE ` +
		strings.Join(where, " AND ") + fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", len(args))
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var diff []byte
		if err := rows.Scan(&e.ID, &e.Actor, &e.Action, &e.TargetType, &e.TargetID, &diff, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Diff = json.RawMessage(diff)
		out = append(out, e)
	}
	return out, rows.Err()
}

// clampAuditLimit bounds a caller-supplied page size, defaulting to 50 and
// capping at 500 so a typo cannot scan the whole log in one request.
func clampAuditLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	if limit > 500 {
		return 500
	}
	return limit
}

// --- backfills ---

func (p *Postgres) GetBackfill(ctx context.Context, monitorID int64) (Backfill, error) {
	var b Backfill
	var fromLedger, toLedger, nextLedger int64
	err := p.pool.QueryRow(ctx,
		`SELECT monitor_id, from_ledger, to_ledger, next_ledger, cursor, deliver, complete, updated_at
		   FROM backfills WHERE monitor_id = $1`, monitorID,
	).Scan(&b.MonitorID, &fromLedger, &toLedger, &nextLedger, &b.Cursor, &b.Deliver, &b.Complete, &b.UpdatedAt)
	if err != nil {
		return b, mapErr(err)
	}
	b.FromLedger = uint32(fromLedger)
	b.ToLedger = uint32(toLedger)
	b.NextLedger = uint32(nextLedger)
	return b, nil
}

// UpsertBackfill writes the run's resume point, replacing any previous row for
// the monitor. One row per monitor is what makes "resume where it stopped"
// unambiguous.
func (p *Postgres) UpsertBackfill(ctx context.Context, b *Backfill) error {
	return p.pool.QueryRow(ctx,
		`INSERT INTO backfills (monitor_id, from_ledger, to_ledger, next_ledger, cursor, deliver, complete)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (monitor_id) DO UPDATE SET
		     from_ledger = EXCLUDED.from_ledger,
		     to_ledger   = EXCLUDED.to_ledger,
		     next_ledger = EXCLUDED.next_ledger,
		     cursor      = EXCLUDED.cursor,
		     deliver     = EXCLUDED.deliver,
		     complete    = EXCLUDED.complete,
		     updated_at  = now()
		 RETURNING updated_at`,
		b.MonitorID, int64(b.FromLedger), int64(b.ToLedger), int64(b.NextLedger),
		b.Cursor, b.Deliver, b.Complete,
	).Scan(&b.UpdatedAt)
}

// --- stats ---

// GetStats returns the dashboard's headline counters. It is a routable read:
// the numbers are aggregate counts over a table that only ever grows, so a
// replica a moment behind reports a total that is a moment stale rather than
// wrong. Nothing is written back from these values, so a stale read cannot
// turn into a stale write.
func (p *Postgres) GetStats(ctx context.Context) (Stats, error) {
	var s Stats
	err := p.queryRowFallback(ctx, func(row pgx.Row) error {
		return row.Scan(&s.Monitors, &s.Rules, &s.Channels, &s.Alerts, &s.AlertsLast24, &s.LastLedger, &s.LastPollAt)
	}, `
		SELECT
			(SELECT count(*) FROM monitors WHERE workspace_id = $1),
			(SELECT count(*) FROM rules r JOIN monitors m ON m.id = r.monitor_id WHERE m.workspace_id = $1),
			(SELECT count(*) FROM channels WHERE workspace_id = $1),
			(SELECT count(*) FROM alerts WHERE workspace_id = $1),
			(SELECT count(*) FROM alerts WHERE workspace_id = $1 AND created_at > now() - interval '24 hours'),
			(SELECT last_ledger FROM ingest_state WHERE id = 1),
			(SELECT updated_at FROM ingest_state WHERE id = 1)`, workspaceID(ctx))
	return s, err
}

// AlertCountsByDay returns the daily alert totals behind the dashboard chart.
// Routable for the same reason as GetStats: a day's bar can lag a moment
// behind real time, and no decision is taken from the value.
func (p *Postgres) AlertCountsByDay(ctx context.Context, days int) ([]AlertDayCount, error) {
	days = ClampAlertSeriesDays(days)
	// generate_series fills every UTC calendar day in the window, including
	// zeroes, so a quiet day is an explicit 0 rather than a missing bar.
	// date_trunc / ::date run on (timestamptz AT TIME ZONE 'UTC') so the
	// session TimeZone cannot shift a late-UTC event into the next local day.
	rows, err := p.queryRows(ctx, `
		WITH days AS (
			SELECT generate_series(
				((now() AT TIME ZONE 'UTC')::date - ($1::int - 1)),
				(now() AT TIME ZONE 'UTC')::date,
				interval '1 day'
			)::date AS day
		)
		SELECT days.day, COUNT(a.id)::bigint
		FROM days
		LEFT JOIN alerts a ON (a.created_at AT TIME ZONE 'UTC')::date = days.day
			AND a.workspace_id = $2
		GROUP BY days.day
		ORDER BY days.day`, days, workspaceID(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AlertDayCount, 0, days)
	for rows.Next() {
		var day time.Time
		var count int64
		if err := rows.Scan(&day, &count); err != nil {
			return nil, err
		}
		out = append(out, AlertDayCount{Day: day.UTC().Format("2006-01-02"), Count: count})
	}
	return out, rows.Err()
}

// GetMonitorStats reports one monitor's alert, delivery and per-rule counts.
// Two statements, whatever the monitor's rule count: the aggregate row and one
// grouped per-rule count. Routable reads for the same reason as GetStats —
// every number here is a count over history that only ever grows, and nothing
// is written back from them.
//
// The existence probe rides along in the aggregate rather than being a third
// query, so a monitor deleted a moment ago reports ErrNotFound instead of a
// page of zeroes that looks like a healthy but quiet monitor.
func (p *Postgres) GetMonitorStats(ctx context.Context, monitorID int64) (MonitorStats, error) {
	var (
		ms     MonitorStats
		exists bool
	)
	ms.MonitorID = monitorID
	err := p.queryRowFallback(ctx, func(row pgx.Row) error {
		return row.Scan(&exists, &ms.Alerts, &ms.AlertsLast24h, &ms.AlertsLast7d,
			&ms.LastAlertAt, &ms.DeliveriesOK, &ms.DeliveriesFail)
	}, `
		SELECT
			(SELECT count(*) FROM monitors WHERE id = $1 AND workspace_id = $4) > 0,
			(SELECT count(*) FROM alerts WHERE monitor_id = $1 AND workspace_id = $4),
			(SELECT count(*) FROM alerts WHERE monitor_id = $1 AND workspace_id = $4 AND created_at > now() - interval '24 hours'),
			(SELECT count(*) FROM alerts WHERE monitor_id = $1 AND workspace_id = $4 AND created_at > now() - interval '7 days'),
			(SELECT max(created_at) FROM alerts WHERE monitor_id = $1 AND workspace_id = $4),
			(SELECT count(*) FROM delivery_attempts da JOIN alerts a ON a.id = da.alert_id
			   WHERE a.monitor_id = $1 AND a.workspace_id = $4 AND da.status = $2),
			(SELECT count(*) FROM delivery_attempts da JOIN alerts a ON a.id = da.alert_id
			   WHERE a.monitor_id = $1 AND a.workspace_id = $4 AND da.status = $3)`,
		monitorID, DeliveryStatusSuccess, DeliveryStatusFailed, workspaceID(ctx))
	if err != nil {
		return ms, err
	}
	if !exists {
		return ms, ErrNotFound
	}

	rows, err := p.queryRows(ctx, `
		SELECT r.id, r.type, count(a.id)
		FROM rules r
		LEFT JOIN alerts a ON a.rule_id = r.id
		WHERE r.monitor_id = $1
		GROUP BY r.id, r.type
		ORDER BY r.id`, monitorID)
	if err != nil {
		return ms, err
	}
	defer rows.Close()
	for rows.Next() {
		var rc RuleMatchCount
		if err := rows.Scan(&rc.RuleID, &rc.Type, &rc.Alerts); err != nil {
			return ms, err
		}
		ms.Rules = append(ms.Rules, rc)
	}
	return ms, rows.Err()
}

// --- helpers ---

// --- saved searches ---

func (p *Postgres) CreateSavedSearch(ctx context.Context, s *SavedSearch) error {
	filter, _ := json.Marshal(s.Filter)
	ws := workspaceID(ctx)
	if s.IsDefault {
		// Scoped, or making one workspace's search the default would clear
		// every other workspace's. The same reasoning applies to the two
		// default-search methods below.
		_, _ = p.pool.Exec(ctx, `UPDATE saved_searches SET is_default = FALSE WHERE is_default = TRUE AND workspace_id = $1`, ws)
	}
	return p.pool.QueryRow(ctx,
		`INSERT INTO saved_searches (name, filter, is_default, workspace_id) VALUES ($1, $2, $3, $4) RETURNING id, created_at`,
		s.Name, filter, s.IsDefault, ws).Scan(&s.ID, &s.CreatedAt)
}

func (p *Postgres) ListSavedSearches(ctx context.Context) ([]SavedSearch, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, name, filter, is_default, created_at FROM saved_searches WHERE workspace_id = $1 ORDER BY name`,
		workspaceID(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SavedSearch
	for rows.Next() {
		var s SavedSearch
		var filter []byte
		if err := rows.Scan(&s.ID, &s.Name, &filter, &s.IsDefault, &s.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(filter, &s.Filter)
		out = append(out, s)
	}
	return out, rows.Err()
}

func (p *Postgres) GetSavedSearch(ctx context.Context, id int64) (*SavedSearch, error) {
	var s SavedSearch
	var filter []byte
	err := p.pool.QueryRow(ctx,
		`SELECT id, name, filter, is_default, created_at FROM saved_searches WHERE id = $1 AND workspace_id = $2`,
		id, workspaceID(ctx)).
		Scan(&s.ID, &s.Name, &filter, &s.IsDefault, &s.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	_ = json.Unmarshal(filter, &s.Filter)
	return &s, nil
}

func (p *Postgres) DeleteSavedSearch(ctx context.Context, id int64) error {
	return p.deleteByID(ctx, "saved_searches", id)
}

func (p *Postgres) SetDefaultSearch(ctx context.Context, id int64) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op
	ws := workspaceID(ctx)
	_, _ = tx.Exec(ctx, `UPDATE saved_searches SET is_default = FALSE WHERE is_default = TRUE AND workspace_id = $1`, ws)
	tag, err := tx.Exec(ctx, `UPDATE saved_searches SET is_default = TRUE WHERE id = $1 AND workspace_id = $2`, id, ws)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}

func (p *Postgres) ClearDefaultSearch(ctx context.Context, id int64) error {
	tag, err := p.pool.Exec(ctx,
		`UPDATE saved_searches SET is_default = FALSE WHERE id = $1 AND workspace_id = $2`,
		id, workspaceID(ctx))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// --- monitor templates ---

func (p *Postgres) CreateMonitorTemplate(ctx context.Context, t *MonitorTemplate) error {
	rulesJSON, _ := json.Marshal(t.Rules)
	paramsJSON, _ := json.Marshal(t.Parameters)
	return p.pool.QueryRow(ctx,
		`INSERT INTO monitor_templates (name, description, rules, channel_ids, parameters, workspace_id) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, created_at`,
		t.Name, t.Description, rulesJSON, templateChannelIDs(t.ChannelIDs), paramsJSON, workspaceID(ctx)).Scan(&t.ID, &t.CreatedAt)
}

func (p *Postgres) GetMonitorTemplate(ctx context.Context, id int64) (*MonitorTemplate, error) {
	var t MonitorTemplate
	var rulesJSON, paramsJSON []byte
	err := p.pool.QueryRow(ctx,
		`SELECT id, name, description, rules, channel_ids, parameters, created_at FROM monitor_templates WHERE id = $1 AND workspace_id = $2`,
		id, workspaceID(ctx)).
		Scan(&t.ID, &t.Name, &t.Description, &rulesJSON, &t.ChannelIDs, &paramsJSON, &t.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	_ = json.Unmarshal(rulesJSON, &t.Rules)
	_ = json.Unmarshal(paramsJSON, &t.Parameters)
	if t.ChannelIDs == nil {
		t.ChannelIDs = []int64{}
	}
	return &t, nil
}

func (p *Postgres) ListMonitorTemplates(ctx context.Context) ([]MonitorTemplate, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, name, description, rules, channel_ids, parameters, created_at FROM monitor_templates WHERE workspace_id = $1 ORDER BY name`,
		workspaceID(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MonitorTemplate
	for rows.Next() {
		var t MonitorTemplate
		var rulesJSON, paramsJSON []byte
		if err := rows.Scan(&t.ID, &t.Name, &t.Description, &rulesJSON, &t.ChannelIDs, &paramsJSON, &t.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(rulesJSON, &t.Rules)
		_ = json.Unmarshal(paramsJSON, &t.Parameters)
		if t.ChannelIDs == nil {
			t.ChannelIDs = []int64{}
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (p *Postgres) UpdateMonitorTemplate(ctx context.Context, t *MonitorTemplate) error {
	rulesJSON, _ := json.Marshal(t.Rules)
	paramsJSON, _ := json.Marshal(t.Parameters)
	tag, err := p.pool.Exec(ctx,
		`UPDATE monitor_templates SET name=$1, description=$2, rules=$3, channel_ids=$4, parameters=$5 WHERE id=$6 AND workspace_id=$7`,
		t.Name, t.Description, rulesJSON, templateChannelIDs(t.ChannelIDs), paramsJSON, t.ID, workspaceID(ctx))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) DeleteMonitorTemplate(ctx context.Context, id int64) error {
	return p.deleteByID(ctx, "monitor_templates", id)
}

// AssignLegacyNetwork labels the rows written before the network column
// existed, once, at startup, in the instance's own cross-tenant scope: an
// operator upgrading a single-network deployment must not watch every monitor
// vanish from the network-filtered listing. Idempotent because the predicate
// only matches the empty value.
func (p *Postgres) AssignLegacyNetwork(ctx context.Context, network string) (int64, error) {
	if network == "" {
		return 0, nil // nothing to label: no network configured
	}
	var total int64
	for _, q := range []string{
		`UPDATE monitors SET network = $1 WHERE network = ''`,
		`UPDATE alerts SET network = $1 WHERE network = ''`,
	} {
		tag, err := p.pool.Exec(ctx, q, network)
		if err != nil {
			return total, mapErr(err)
		}
		total += tag.RowsAffected()
	}
	return total, nil
}

// --- api tokens ---

// CreateAPIToken inserts one token row. The caller passes a digest, never a
// secret: auth.Manager hashes before this point and drops the plaintext, so the
// string that reaches SQL (or a log, or an error) is already one-way.
func (p *Postgres) CreateAPIToken(ctx context.Context, t *auth.Token) error {
	var expiresAt any
	if !t.ExpiresAt.IsZero() {
		expiresAt = t.ExpiresAt
	}
	ws := workspaceID(ctx)
	err := p.pool.QueryRow(ctx,
		`INSERT INTO api_tokens (workspace_id, name, token_hash, prefix, scopes, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, created_at`,
		ws, t.Name, t.Hash, t.Prefix, auth.JoinScopes(t.Scopes), expiresAt).
		Scan(&t.ID, &t.CreatedAt)
	if err != nil {
		return mapErr(err)
	}
	t.Workspace = ws
	return nil
}

// TokenByHash is the authentication read. It is the one token method that does
// not scope itself to ctx's workspace: a request carrying a token has no tenant
// until this row answers, and the row's own workspace_id is what the request
// then gets — never anything the caller supplied.
func (p *Postgres) TokenByHash(ctx context.Context, hash string) (*auth.Token, bool, error) {
	t, err := scanAPIToken(p.pool.QueryRow(ctx,
		`SELECT id, workspace_id, name, token_hash, prefix, scopes, expires_at, last_used_at, revoked_at, created_at
		   FROM api_tokens WHERE token_hash = $1`, hash))
	if errors.Is(err, ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &t, true, nil
}

func (p *Postgres) ListAPITokens(ctx context.Context) ([]auth.Token, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, workspace_id, name, token_hash, prefix, scopes, expires_at, last_used_at, revoked_at, created_at
		   FROM api_tokens WHERE workspace_id = $1 ORDER BY id DESC`,
		workspaceID(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []auth.Token
	for rows.Next() {
		t, err := scanAPIToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeAPIToken retires one of the caller's tokens. COALESCE keeps the first
// revocation timestamp, so revoking twice is a no-op that still reports success
// rather than a 404 for a token the caller can already see is revoked; a row in
// another workspace is ErrNotFound, the same answer a missing id gives.
func (p *Postgres) RevokeAPIToken(ctx context.Context, id int64) error {
	tag, err := p.pool.Exec(ctx,
		`UPDATE api_tokens SET revoked_at = COALESCE(revoked_at, now()) WHERE id = $1 AND workspace_id = $2`,
		id, workspaceID(ctx))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchAPIToken records that a token authenticated. The write is unconditional
// because the caller throttles it (auth.Manager skips a token used within the
// last minute); a store-side guard would make the throttle's interval a second
// copy of the same constant.
func (p *Postgres) TouchAPIToken(ctx context.Context, id int64, at time.Time) error {
	_, err := p.pool.Exec(
		ctx,
		`UPDATE api_tokens SET last_used_at = $3 WHERE id = $1 AND workspace_id = $2`,
		id, workspaceID(ctx), at)
	return err
}

// scanAPIToken reads one api_tokens row through the same rowScanner the other
// scans use, so the single-row read and the listing share one column order.
// The three nullable timestamps come back as pointers and become zero times,
// which is what auth.Token's Live and the dashboard's "never used" both key off.
func scanAPIToken(row rowScanner) (auth.Token, error) {
	var t auth.Token
	var scopes string
	var expiresAt, lastUsedAt, revokedAt *time.Time
	err := row.Scan(&t.ID, &t.Workspace, &t.Name, &t.Hash, &t.Prefix, &scopes,
		&expiresAt, &lastUsedAt, &revokedAt, &t.CreatedAt)
	if err != nil {
		return t, mapErr(err)
	}
	t.Scopes = auth.SplitScopes(scopes)
	if expiresAt != nil {
		t.ExpiresAt = *expiresAt
	}
	if lastUsedAt != nil {
		t.LastUsedAt = *lastUsedAt
	}
	if revokedAt != nil {
		t.RevokedAt = *revokedAt
	}
	return t, nil
}

// deleteByID removes one row by id from a workspace-bearing table. The table
// name is not a parameter, so it is constrained to deletableTables rather than
// taken from a caller; a row outside the caller's workspace reports RowsAffected
// 0, which is ErrNotFound — the same answer an id that does not exist gives, so
// a delete cannot probe other tenants.
func (p *Postgres) deleteByID(ctx context.Context, table string, id int64) error {
	if !deletableTables[table] {
		return errUnallowlistedTable
	}
	tag, err := p.pool.Exec(ctx,
		`DELETE FROM `+table+` WHERE id = $1 AND workspace_id = $2`, id, workspaceID(ctx))
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

func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return raw
}

// WithSuppressed annotates an alert payload with the number of matches the
// rule's cooldown swallowed, so an operator sees the scale of what happened
// instead of a silent gap. A payload that is not a JSON object is returned
// untouched rather than replaced. Exported so alternative Stores and the
// poller's test fake can produce the identical payload shape.
func WithSuppressed(payload json.RawMessage, n int64) json.RawMessage {
	if n <= 0 {
		return payload
	}
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil || obj == nil {
		return payload
	}
	obj["suppressed_since_last"] = n
	merged, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return merged
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

// ListAlertsStream implements Store by paging ListAlerts with the keyset
// cursor, so peak memory is one page rather than the whole result set.
func (p *Postgres) ListAlertsStream(ctx context.Context, f AlertFilter, fn func(Alert) error) error {
	return streamAlerts(ctx, f, p.ListAlerts, fn)
}
