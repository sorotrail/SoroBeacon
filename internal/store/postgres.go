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

	"github.com/sorotrail/sorobeacon/internal/telemetry"
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

// --- monitors ---

func (p *Postgres) CreateMonitor(ctx context.Context, m *Monitor) error {
	ids, err := json.Marshal(m.ContractIDs)
	if err != nil {
		return err
	}
	return p.pool.QueryRow(ctx,
		`INSERT INTO monitors (name, contract_ids, enabled, priority) VALUES ($1, $2, $3, $4)
		 RETURNING id, created_at`,
		m.Name, ids, m.Enabled, m.Priority.Normalized(),
	).Scan(&m.ID, &m.CreatedAt)
}

func (p *Postgres) GetMonitor(ctx context.Context, id int64) (*Monitor, error) {
	m, err := scanMonitor(p.pool.QueryRow(ctx,
		`SELECT id, name, contract_ids, enabled, created_at, last_matched_at, priority FROM monitors WHERE id = $1`, id))
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

func (p *Postgres) ListMonitors(ctx context.Context, enabledOnly bool) ([]Monitor, error) {
	q := `SELECT id, name, contract_ids, enabled, created_at, last_matched_at, priority FROM monitors`
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

func (p *Postgres) ListMonitorsPage(ctx context.Context, f ListFilter) ([]Monitor, error) {
	q := `SELECT id, name, contract_ids, enabled, created_at, last_matched_at, priority FROM monitors WHERE TRUE`
	args := []any{}
	n := 0
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
		switch sort {
		case "name":
			q += ` AND (lower(name), id) > (SELECT lower(name), id FROM monitors WHERE id = ` + arg(f.AfterID) + `)`
		case "created_at":
			q += ` AND (created_at, id) < (SELECT created_at, id FROM monitors WHERE id = ` + arg(f.AfterID) + `)`
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
		`UPDATE monitors SET name = $2, contract_ids = $3, enabled = $4, priority = $5 WHERE id = $1`,
		m.ID, m.Name, ids, m.Enabled, m.Priority.Normalized())
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
		`UPDATE monitors SET enabled = $1 WHERE id = ANY($2) RETURNING id`,
		enabled, uniq)
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
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	src, err := scanMonitor(tx.QueryRow(ctx,
		`SELECT id, name, contract_ids, enabled, created_at, last_matched_at, priority FROM monitors WHERE id = $1`, id))
	if err != nil {
		return nil, err
	}
	chRows, err := tx.Query(ctx,
		`SELECT channel_id FROM monitor_channels WHERE monitor_id = $1 ORDER BY channel_id`, id)
	if err != nil {
		return nil, err
	}
	src.ChannelIDs, err = pgx.CollectRows(chRows, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	ruleRows, err := tx.Query(ctx,
		`SELECT type, params, enabled FROM rules WHERE monitor_id = $1 ORDER BY id`, id)
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
	nameRows, err := tx.Query(ctx, `SELECT name FROM monitors`)
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
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO monitors (name, contract_ids, enabled, priority) VALUES ($1, $2, $3, $4)
		 RETURNING id, created_at`,
		copy.Name, ids, copy.Enabled, copy.Priority,
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
	var priority string
	if err := r.Scan(&m.ID, &m.Name, &ids, &m.Enabled, &m.CreatedAt, &m.LastMatchedAt, &priority); err != nil {
		return nil, mapErr(err)
	}
	m.Priority = Priority(priority).Normalized()
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

func (p *Postgres) CreateRules(ctx context.Context, rules []*Rule) error {
	if len(rules) == 0 {
		return nil
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	for _, r := range rules {
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
	config, err := configForWrite(p.cipher, c.ID, c.Name, c.Config)
	if err != nil {
		return err
	}
	return p.pool.QueryRow(ctx,
		`INSERT INTO channels (name, type, config, enabled, digest_mode, digest_window_seconds)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, created_at`,
		c.Name, c.Type, config, c.Enabled, c.DigestMode, c.DigestWindowSeconds,
	).Scan(&c.ID, &c.CreatedAt)
}

func (p *Postgres) GetChannel(ctx context.Context, id int64) (*Channel, error) {
	var c Channel
	err := p.pool.QueryRow(ctx,
		`SELECT id, name, type, config, enabled, created_at, digest_mode, digest_window_seconds FROM channels WHERE id = $1`, id,
	).Scan(&c.ID, &c.Name, &c.Type, &c.Config, &c.Enabled, &c.CreatedAt, &c.DigestMode, &c.DigestWindowSeconds)
	if err != nil {
		return nil, mapErr(err)
	}
	if err := decryptChannel(p.cipher, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (p *Postgres) ListChannels(ctx context.Context, enabledOnly bool) ([]Channel, error) {
	q := `SELECT id, name, type, config, enabled, created_at, digest_mode, digest_window_seconds FROM channels`
	if enabledOnly {
		q += ` WHERE enabled`
	}
	q += ` ORDER BY id`
	rows, err := p.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, p.scanChannel)
}

func (p *Postgres) ListChannelsPage(ctx context.Context, f ListFilter) ([]Channel, error) {
	q := `SELECT id, name, type, config, enabled, created_at, digest_mode, digest_window_seconds FROM channels WHERE TRUE`
	args := []any{}
	n := 0
	arg := func(v any) string {
		n++
		args = append(args, v)
		return fmt.Sprintf("$%d", n)
	}
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
	tag, err := p.pool.Exec(ctx,
		`UPDATE channels SET name = $2, type = $3, config = $4, enabled = $5, digest_mode = $6, digest_window_seconds = $7 WHERE id = $1`,
		c.ID, c.Name, c.Type, config, c.Enabled, c.DigestMode, c.DigestWindowSeconds)
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

func (p *Postgres) ListMonitorsForChannel(ctx context.Context, channelID int64) ([]Monitor, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT m.id, m.name, m.contract_ids, m.enabled, m.created_at, m.last_matched_at, m.priority
		 FROM monitors m
		 JOIN monitor_channels mc ON mc.monitor_id = m.id
		 WHERE mc.channel_id = $1
		 ORDER BY m.id`, channelID)
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
	rows, err := p.pool.Query(ctx,
		`SELECT c.id, c.name, c.type, c.config, c.enabled, c.created_at, c.digest_mode, c.digest_window_seconds
		 FROM channels c
		 JOIN monitor_channels mc ON mc.channel_id = c.id
		 WHERE mc.monitor_id = $1 AND c.enabled
		 ORDER BY c.id`, monitorID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, p.scanChannel)
}

// scanChannel reads one channels row and decrypts its config, so every
// caller up the stack (API, dashboard, dispatcher) sees plaintext.
func (p *Postgres) scanChannel(row pgx.CollectableRow) (Channel, error) {
	var c Channel
	if err := row.Scan(&c.ID, &c.Name, &c.Type, &c.Config, &c.Enabled, &c.CreatedAt, &c.DigestMode, &c.DigestWindowSeconds); err != nil {
		return c, err
	}
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
		`INSERT INTO alerts (monitor_id, rule_id, event_id, payload, backfilled, ledger) VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, created_at`,
		a.MonitorID, a.RuleID, a.EventID, jsonOrEmpty(a.Payload), a.Backfilled, int64(a.Ledger),
	).Scan(&a.ID, &a.CreatedAt)
	if err != nil {
		return "", err
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
		`SELECT id, monitor_id, rule_id, event_id, payload, created_at, ledger, retracted_at, backfilled
		   FROM alerts WHERE id = $1`, id,
	).Scan(&a.ID, &a.MonitorID, &a.RuleID, &a.EventID, &a.Payload, &a.CreatedAt, &ledger, &a.RetractedAt, &a.Backfilled)
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
	q, args := buildAlertQuery(f)
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
	q, args := buildAlertQuery(f)
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanAlert)
}

// buildAlertQuery builds the ListAlerts statement and its arguments. Both
// readers call it so the routed and primary-bound forms cannot drift into
// returning different pages.
func buildAlertQuery(f AlertFilter) (string, []any) {
	q := `SELECT id, monitor_id, rule_id, event_id, payload, created_at, ledger, retracted_at
	q := `SELECT id, monitor_id, rule_id, event_id, payload, created_at, ledger, retracted_at, backfilled
		 FROM alerts WHERE TRUE`
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
	if f.RuleID != 0 {
		q += ` AND rule_id = ` + arg(f.RuleID)
	}
	if f.ContractID != "" {
		q += ` AND payload->>'contract_id' = ` + arg(f.ContractID)
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

// scanAlert reads one alerts row. It is shared by ListAlerts and ExpiredAlerts
// so the column order and the ledger/retracted_at mapping cannot drift.
func scanAlert(row pgx.CollectableRow) (Alert, error) {
	var a Alert
	var ledger int64
	err := row.Scan(&a.ID, &a.MonitorID, &a.RuleID, &a.EventID, &a.Payload, &a.CreatedAt, &ledger, &a.RetractedAt, &a.Backfilled)
	a.Ledger = uint32(ledger)
	return a, err
}

// ExpiredAlerts returns up to limit alerts older than cutoff, oldest first,
// with the same ordering DeleteExpiredAlerts uses so the row an archiver reads
// is the row the delete removes.
func (p *Postgres) ExpiredAlerts(ctx context.Context, cutoff time.Time, limit int) ([]Alert, error) {
	if limit <= 0 {
		limit = DefaultPruneBatch
	}
	rows, err := p.pool.Query(ctx,
		`SELECT id, monitor_id, rule_id, event_id, payload, created_at, ledger, retracted_at, backfilled
		   FROM alerts WHERE created_at < $1 ORDER BY created_at ASC, id ASC LIMIT $2`,
		cutoff, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanAlert)
}

// --- ledger hashes and reorg retraction ---

func (p *Postgres) RecordLedgerHashes(ctx context.Context, hashes []LedgerHash) error {
	if len(hashes) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, h := range hashes {
		batch.Queue(`
			INSERT INTO ledger_hashes (ledger, hash) VALUES ($1, $2)
			ON CONFLICT (ledger) DO UPDATE SET hash = EXCLUDED.hash, observed_at = now()
			WHERE ledger_hashes.hash IS DISTINCT FROM EXCLUDED.hash`, int64(h.Ledger), h.Hash)
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

func (p *Postgres) LedgerHashes(ctx context.Context, from, to uint32) ([]LedgerHash, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT ledger, hash FROM ledger_hashes WHERE ledger >= $1 AND ledger <= $2 ORDER BY ledger`,
		int64(from), int64(to))
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

func (p *Postgres) PruneLedgerHashes(ctx context.Context, before uint32) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM ledger_hashes WHERE ledger < $1`, int64(before))
	return err
}

func (p *Postgres) RetractAlertsFromLedger(ctx context.Context, ledger uint32, at time.Time) (int64, error) {
	tag, err := p.pool.Exec(ctx,
		`UPDATE alerts SET retracted_at = $1 WHERE ledger >= $2 AND retracted_at IS NULL`,
		at.UTC(), int64(ledger))
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
	return mapErr(p.pool.QueryRow(ctx,
		`INSERT INTO delivery_attempts (alert_id, alert_created_at, channel_id, status, response_snippet)
		 SELECT a.id, a.created_at, $2, $3, $4 FROM alerts a WHERE a.id = $1
		 RETURNING id, attempted_at`,
		d.AlertID, d.ChannelID, d.Status, d.ResponseSnippet,
	).Scan(&d.ID, &d.AttemptedAt))
}

func (p *Postgres) ListDeliveryAttempts(ctx context.Context, alertID int64, status string) ([]DeliveryAttempt, error) {
	q := `SELECT id, alert_id, channel_id, status, response_snippet, attempted_at
		 FROM delivery_attempts WHERE alert_id = $1`
	args := []any{alertID}
	if status != "" {
		// Applied in SQL so a busy alert does not ship every attempt just
		// so the client can throw most of them away.
		q += ` AND status = $2`
		args = append(args, status)
	}
	q += ` ORDER BY id`
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
			(SELECT count(*) FROM monitors),
			(SELECT count(*) FROM rules),
			(SELECT count(*) FROM channels),
			(SELECT count(*) FROM alerts),
			(SELECT count(*) FROM alerts WHERE created_at > now() - interval '24 hours'),
			(SELECT last_ledger FROM ingest_state WHERE id = 1),
			(SELECT updated_at FROM ingest_state WHERE id = 1)`)
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
		GROUP BY days.day
		ORDER BY days.day`, days)
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

// --- helpers ---

// --- saved searches ---

func (p *Postgres) CreateSavedSearch(ctx context.Context, s *SavedSearch) error {
	filter, _ := json.Marshal(s.Filter)
	if s.IsDefault {
		_, _ = p.pool.Exec(ctx, `UPDATE saved_searches SET is_default = FALSE WHERE is_default = TRUE`)
	}
	return p.pool.QueryRow(ctx,
		`INSERT INTO saved_searches (name, filter, is_default) VALUES ($1, $2, $3) RETURNING id, created_at`,
		s.Name, filter, s.IsDefault).Scan(&s.ID, &s.CreatedAt)
}

func (p *Postgres) ListSavedSearches(ctx context.Context) ([]SavedSearch, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, name, filter, is_default, created_at FROM saved_searches ORDER BY name`)
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
		`SELECT id, name, filter, is_default, created_at FROM saved_searches WHERE id = $1`, id).
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
	_, _ = tx.Exec(ctx, `UPDATE saved_searches SET is_default = FALSE WHERE is_default = TRUE`)
	tag, err := tx.Exec(ctx, `UPDATE saved_searches SET is_default = TRUE WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}

func (p *Postgres) ClearDefaultSearch(ctx context.Context, id int64) error {
	tag, err := p.pool.Exec(ctx, `UPDATE saved_searches SET is_default = FALSE WHERE id = $1`, id)
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
		`INSERT INTO monitor_templates (name, description, rules, channel_ids, parameters) VALUES ($1, $2, $3, $4, $5) RETURNING id, created_at`,
		t.Name, t.Description, rulesJSON, t.ChannelIDs, paramsJSON).Scan(&t.ID, &t.CreatedAt)
}

func (p *Postgres) GetMonitorTemplate(ctx context.Context, id int64) (*MonitorTemplate, error) {
	var t MonitorTemplate
	var rulesJSON, paramsJSON []byte
	err := p.pool.QueryRow(ctx,
		`SELECT id, name, description, rules, channel_ids, parameters, created_at FROM monitor_templates WHERE id = $1`, id).
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
		`SELECT id, name, description, rules, channel_ids, parameters, created_at FROM monitor_templates ORDER BY name`)
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
		`UPDATE monitor_templates SET name=$1, description=$2, rules=$3, channel_ids=$4, parameters=$5 WHERE id=$6`,
		t.Name, t.Description, rulesJSON, t.ChannelIDs, paramsJSON, t.ID)
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
