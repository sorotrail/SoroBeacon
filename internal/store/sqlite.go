package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// sqliteDriver is the database/sql driver name registered by
// modernc.org/sqlite. It is pure Go (no cgo), so the existing CGO_ENABLED=0
// build in the Dockerfile keeps working.
const sqliteDriver = "sqlite"

// sqliteTimeLayout is the single on-disk timestamp format. Every time column
// is TEXT in this fixed-width, always-UTC shape, which makes lexicographic
// comparison equal chronological comparison in SQL (so keyset pagination and
// retention cutoffs are correct) and round-trips exactly through Go. It
// matches SQLite's strftime('%Y-%m-%dT%H:%M:%fZ', 'now').
const sqliteTimeLayout = "2006-01-02T15:04:05.000Z"

// sqliteConstraintForeignKey is sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY
// (SQLITE_CONSTRAINT | 3<<8). It is spelled out rather than imported from
// modernc.org/sqlite/lib so the low-level package stays out of the store.
const sqliteConstraintForeignKey = 19 | (3 << 8)

// SQLite implements Store on a single SQLite database file through
// database/sql and modernc.org/sqlite. It exists for the one-contract-on-a-
// Raspberry-Pi deployment where requiring a Postgres server is too much:
// DATABASE_URL=sqlite:///path/to/sorobeacon.db needs no external service.
//
// Concurrency: SQLite serialises writers, so this store opens a single
// connection (SetMaxOpenConns(1)) and begins every explicit transaction with
// BEGIN IMMEDIATE (the driver's _txlock=immediate). That replaces Postgres'
// SELECT ... FOR UPDATE row lock: the whole-database write lock is held for
// the transaction, so read-then-write decisions such as the alert cooldown
// stay race-safe. The price is that only one write happens at a time, which
// is the honest limit of this backend; WAL mode is enabled so readers are
// never blocked by a writer.
type SQLite struct {
	db *sql.DB
	// cipher encrypts and decrypts channels.config at rest; nil keeps the
	// plaintext behaviour, exactly as the Postgres store does.
	cipher ConfigCipher
}

var _ Store = (*SQLite)(nil)

// WithConfigCipher sets the cipher used to encrypt Channel.Config at rest and
// returns the store for chaining. A nil cipher (or never calling it) stores
// config as plaintext, matching the Postgres backend.
func (s *SQLite) WithConfigCipher(c ConfigCipher) *SQLite {
	s.cipher = c
	return s
}

// NewSQLite opens (creating if necessary) the SQLite database named by
// databaseURL. The URL is sqlite://<path>; databaseURL holds no credentials,
// so unlike the Postgres path there is nothing to redact. Migrations are not
// applied here — call Migrate first, as the Postgres path does.
func NewSQLite(ctx context.Context, databaseURL string) (*SQLite, error) {
	if err := ensureSQLiteDir(databaseURL); err != nil {
		return nil, err
	}
	dsn, err := sqliteDSN(databaseURL)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(sqliteDriver, dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// One connection, so writers queue in Go rather than failing with
	// SQLITE_BUSY, and a transaction that took the write lock keeps it until
	// it commits. WAL still lets a separate reader (e.g. sqlite3(1)) read.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	return &SQLite{db: db}, nil
}

func (s *SQLite) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
func (s *SQLite) Close()                         { _ = s.db.Close() }

// ensureSQLiteDir creates the directory holding the database file, so
// `sqlite:///var/lib/sorobeacon/db.sqlite` works on a fresh host without a
// manual mkdir — the point of this backend being a one-command quickstart.
// Both Migrate and NewSQLite call it; migrations run before the store opens.
func ensureSQLiteDir(databaseURL string) error {
	path, err := sqliteFilePath(databaseURL)
	if err != nil {
		return err
	}
	if strings.HasPrefix(path, ":memory:") {
		return nil
	}
	dir := filepath.Dir(path)
	if dir == "." || dir == "/" || dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create sqlite directory %q: %w", dir, err)
	}
	return nil
}

// sqliteFilePath extracts the database file path from a sqlite:// URL.
// sqlite:///abs/path.db, sqlite://relative/path.db and sqlite:path.db are all
// accepted; sqlite://:memory: is honoured for tests.
func sqliteFilePath(databaseURL string) (string, error) {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return "", fmt.Errorf("parse sqlite DATABASE_URL: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "sqlite") {
		return "", fmt.Errorf("DATABASE_URL scheme %q is not sqlite", u.Scheme)
	}
	path := u.Opaque
	if path == "" {
		path = u.Path
		if u.Host != "" {
			// url.Parse puts the first segment of sqlite://relative/path.db
			// in Host; rejoin it so a relative path survives.
			path = u.Host + u.Path
		}
	}
	if path == "" {
		return "", errors.New("sqlite DATABASE_URL is missing a database file path (e.g. sqlite:///var/lib/sorobeacon/sorobeacon.db)")
	}
	return path, nil
}

// sqliteDSN builds the modernc.org/sqlite DSN: WAL journalling, foreign-key
// enforcement (off by default in SQLite, and the schema relies on
// ON DELETE CASCADE), a busy timeout, and immediate transactions so a write
// lock is taken up front instead of at first write.
func sqliteDSN(databaseURL string) (string, error) {
	path, err := sqliteFilePath(databaseURL)
	if err != nil {
		return "", err
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Set("_txlock", "immediate")
	return "file:" + path + "?" + q.Encode(), nil
}

// sqliteTimeString renders a time in the canonical on-disk format. Times are
// always normalised to UTC so comparisons never depend on the host's zone.
func sqliteTimeString(t time.Time) string { return t.UTC().Format(sqliteTimeLayout) }

// parseSQLiteTime reverses sqliteTimeString. An empty string is the zero time
// so a NULL / absent timestamp never becomes an error.
func parseSQLiteTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(sqliteTimeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse sqlite time %q: %w", s, err)
	}
	return t, nil
}

// boolToInt maps a Go bool onto SQLite's INTEGER 0/1 storage.
func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// placeholders returns "?, ?, ..." for n bound parameters.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// mapSQLiteErr converts driver sentinels into store-level errors. database/sql
// reports a missing row as sql.ErrNoRows; a foreign-key violation is mapped to
// ErrNotFound for parity with the Postgres backend, which does the same.
func mapSQLiteErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	var se *sqlite.Error
	if errors.As(err, &se) && se.Code() == sqliteConstraintForeignKey {
		return fmt.Errorf("%w: foreign key constraint", ErrNotFound)
	}
	return err
}

// EnsureWorkspace implements the Workspaces half of Store. See the interface
// for why this is the only write to the table.
func (s *SQLite) EnsureWorkspace(ctx context.Context, id workspace.ID) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO workspaces (id, name) VALUES (?, ?) ON CONFLICT (id) DO NOTHING`,
		string(id), string(id))
	return mapSQLiteErr(err)
}

// --- monitors ---

func (s *SQLite) CreateMonitor(ctx context.Context, m *Monitor) error {
	ids, err := json.Marshal(m.ContractIDs)
	if err != nil {
		return err
	}
	var created string
	if err := s.db.QueryRowContext(ctx,
		`INSERT INTO monitors (name, contract_ids, enabled, priority, workspace_id, network) VALUES (?, ?, ?, ?, ?, ?)
		 RETURNING id, created_at`,
		m.Name, string(ids), boolToInt(m.Enabled), string(m.Priority.Normalized()), workspaceID(ctx), m.Network,
	).Scan(&m.ID, &created); err != nil {
		return mapSQLiteErr(err)
	}
	m.CreatedAt, err = parseSQLiteTime(created)
	return err
}

func (s *SQLite) GetMonitor(ctx context.Context, id int64) (*Monitor, error) {
	ws := workspaceID(ctx)
	m, err := scanSQLiteMonitor(s.db.QueryRowContext(ctx,
		`SELECT id, name, contract_ids, enabled, created_at, last_matched_at, priority, network FROM monitors WHERE id = ? AND workspace_id = ?`, id, ws))
	if err != nil {
		return nil, err
	}
	m.ChannelIDs, err = s.monitorChannelIDs(ctx, id, ws)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// monitorChannelIDs reads one monitor's attached channel ids. monitor_channels
// carries no workspace column of its own — it is reachable only through a
// monitor that does — so the join is what scopes the read.
func (s *SQLite) monitorChannelIDs(ctx context.Context, monitorID int64, ws workspace.ID) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT mc.channel_id FROM monitor_channels mc
		   JOIN monitors m ON m.id = mc.monitor_id
		  WHERE mc.monitor_id = ? AND m.workspace_id = ? ORDER BY mc.channel_id`, monitorID, ws)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListMonitors is the poller's watch list as well as the dashboard's, so it
// honours the cross-tenant system scope: an ingest loop must see every
// workspace's enabled monitors or a monitor in a second tenant would silently
// stop being polled.
func (s *SQLite) ListMonitors(ctx context.Context, enabledOnly bool) ([]Monitor, error) {
	q := `SELECT id, name, contract_ids, enabled, created_at, last_matched_at, priority, network FROM monitors WHERE 1 = 1`
	args := []any{}
	if ws, scoped := tenantWorkspace(ctx); scoped {
		q += ` AND workspace_id = ?`
		args = append(args, ws)
	}
	if enabledOnly {
		q += ` AND enabled = 1`
	}
	q += ` ORDER BY id`
	return s.queryMonitors(ctx, q, args...)
}

func (s *SQLite) queryMonitors(ctx context.Context, q string, args ...any) ([]Monitor, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Monitor
	for rows.Next() {
		m, err := scanSQLiteMonitor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (s *SQLite) ListMonitorsPage(ctx context.Context, f ListFilter) ([]Monitor, error) {
	ws := workspaceID(ctx)
	q := `SELECT id, name, contract_ids, enabled, created_at, last_matched_at, priority, network FROM monitors WHERE workspace_id = ?`
	args := []any{ws}
	if f.Query != "" {
		// instr + lower is a parameterized substring match without LIKE
		// metacharacters, so a search for "100%" cannot become a wildcard.
		q += ` AND instr(lower(name), lower(?)) > 0`
		args = append(args, f.Query)
	}
	if f.Network != "" {
		q += ` AND network = ?`
		args = append(args, f.Network)
	}
	switch {
	case f.Enabled != nil && *f.Enabled:
		q += ` AND enabled = 1`
	case f.Enabled != nil && !*f.Enabled:
		q += ` AND enabled = 0`
	case f.EnabledOnly:
		q += ` AND enabled = 1`
	}
	sort := monitorSort(f.Sort)
	if f.AfterID != 0 {
		// The cursor row is read under the same workspace predicate as the
		// page, so an id belonging to another tenant cannot even be used to
		// probe that tenant's ordering.
		switch sort {
		case "name":
			q += ` AND (lower(name), id) > (SELECT lower(name), id FROM monitors WHERE id = ? AND workspace_id = ?)`
			args = append(args, f.AfterID, ws)
		case "created_at":
			q += ` AND (created_at, id) < (SELECT created_at, id FROM monitors WHERE id = ? AND workspace_id = ?)`
			args = append(args, f.AfterID, ws)
		default:
			q += ` AND id < ?`
			args = append(args, f.AfterID)
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
	q += ` LIMIT ?`
	args = append(args, pageLimit(f.Limit))
	return s.queryMonitors(ctx, q, args...)
}

func (s *SQLite) UpdateMonitor(ctx context.Context, m *Monitor) error {
	ids, err := json.Marshal(m.ContractIDs)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE monitors SET name = ?, contract_ids = ?, enabled = ?, priority = ? WHERE id = ? AND workspace_id = ?`,
		m.Name, string(ids), boolToInt(m.Enabled), string(m.Priority.Normalized()), m.ID, workspaceID(ctx))
	if err != nil {
		return mapSQLiteErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLite) SetMonitorsEnabled(ctx context.Context, ids []int64, enabled bool) (int, []int64, error) {
	uniq := uniqueIDs(ids)
	if len(uniq) == 0 {
		return 0, []int64{}, nil
	}
	args := make([]any, 0, len(uniq)+2)
	args = append(args, boolToInt(enabled))
	for _, id := range uniq {
		args = append(args, id)
	}
	args = append(args, workspaceID(ctx))
	rows, err := s.db.QueryContext(ctx,
		`UPDATE monitors SET enabled = ? WHERE id IN (`+placeholders(len(uniq))+`) AND workspace_id = ? RETURNING id`, args...)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = rows.Close() }()
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

func (s *SQLite) DeleteMonitor(ctx context.Context, id int64) error {
	return s.deleteByID(ctx, "monitors", id)
}

// deleteByID removes one row by id from a workspace-bearing table. The table
// name is not a parameter, so it is constrained to deletableTables rather than
// taken from a caller; a row outside the caller's workspace reports 0 rows
// affected, which is ErrNotFound — the same answer an id that does not exist
// gives, so a delete cannot probe other tenants.
func (s *SQLite) deleteByID(ctx context.Context, table string, id int64) error {
	if !deletableTables[table] {
		return errUnallowlistedTable
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE id = ? AND workspace_id = ?`, id, workspaceID(ctx))
	if err != nil {
		return mapSQLiteErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetMonitorChannels replaces one monitor's notification targets. Both sides
// must belong to the caller's workspace: the monitor check is what stops one
// tenant re-pointing another's monitor, and the channel check is what stops a
// tenant attaching a channel it cannot see, which would route this workspace's
// alerts to a webhook owned by someone else.
func (s *SQLite) SetMonitorChannels(ctx context.Context, monitorID int64, channelIDs []int64) error {
	ws := workspaceID(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // rollback after commit is a no-op

	if err := s.requireMonitorWorkspace(ctx, tx, monitorID, ws); err != nil {
		return err
	}
	if len(channelIDs) > 0 {
		uniq := uniqueIDs(channelIDs)
		args := make([]any, 0, len(uniq)+1)
		for _, id := range uniq {
			args = append(args, id)
		}
		args = append(args, ws)
		var count int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM channels WHERE id IN (`+placeholders(len(uniq))+`) AND workspace_id = ?`, args...,
		).Scan(&count); err != nil {
			return err
		}
		if count != len(uniq) {
			return ErrNotFound
		}
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM monitor_channels
		  WHERE monitor_id IN (SELECT id FROM monitors WHERE id = ? AND workspace_id = ?)`,
		monitorID, ws); err != nil {
		return err
	}
	for _, cid := range channelIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO monitor_channels (monitor_id, channel_id) VALUES (?, ?) ON CONFLICT DO NOTHING`,
			monitorID, cid); err != nil {
			return mapSQLiteErr(err)
		}
	}
	return tx.Commit()
}

func (s *SQLite) DuplicateMonitor(ctx context.Context, id int64) (*Monitor, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }() // rollback after commit is a no-op

	ws := workspaceID(ctx)
	src, err := scanSQLiteMonitor(tx.QueryRowContext(ctx,
		`SELECT id, name, contract_ids, enabled, created_at, last_matched_at, priority, network FROM monitors WHERE id = ? AND workspace_id = ?`, id, ws))
	if err != nil {
		return nil, err
	}
	// Both sides of the attachment are checked, not just the monitor. A row
	// attached before tenancy existed (or by a path that has since been scoped)
	// must not be carried into the copy: that would move a channel this tenant
	// cannot see — and whose webhook target it therefore should not start
	// firing — onto a monitor it can. The list read here is the list copied, so
	// the returned snapshot cannot promise an attachment the copy lacks.
	src.ChannelIDs, err = queryInt64Column(ctx, tx,
		`SELECT mc.channel_id FROM monitor_channels mc
		   JOIN monitors m ON m.id = mc.monitor_id
		   JOIN channels c ON c.id = mc.channel_id
		  WHERE mc.monitor_id = ? AND m.workspace_id = ? AND c.workspace_id = ?
		  ORDER BY mc.channel_id`, id, ws, ws)
	if err != nil {
		return nil, err
	}
	rules, err := querySQLiteRules(ctx, tx,
		`SELECT r.id, r.monitor_id, r.type, r.params, r.enabled FROM rules r
		   JOIN monitors m ON m.id = r.monitor_id
		  WHERE r.monitor_id = ? AND m.workspace_id = ? ORDER BY r.id`, id, ws)
	if err != nil {
		return nil, err
	}
	// Name uniqueness is a workspace question, not a global one: two teams may
	// each have a monitor called "Vault minter", and a copy must not be forced
	// to "(copy 2)" because another tenant used the plain name first.
	names, err := queryStringColumn(ctx, tx, `SELECT name FROM monitors WHERE workspace_id = ?`, ws)
	if err != nil {
		return nil, err
	}

	ids, err := json.Marshal(src.ContractIDs)
	if err != nil {
		return nil, err
	}
	dup := Monitor{
		Name:        CopyMonitorName(src.Name, names),
		ContractIDs: src.ContractIDs,
		Enabled:     false,                     // never inherit enabled: a duplicate must be reviewed first
		Priority:    src.Priority.Normalized(), // priority is queue position, not a safety switch, so the copy keeps it
		Network:     src.Network,               // the copy watches the same contract ids, which only exist on this chain
	}
	var created string
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO monitors (name, contract_ids, enabled, priority, workspace_id, network) VALUES (?, ?, ?, ?, ?, ?) RETURNING id, created_at`,
		dup.Name, string(ids), boolToInt(dup.Enabled), string(dup.Priority), ws, dup.Network,
	).Scan(&dup.ID, &created); err != nil {
		return nil, mapSQLiteErr(err)
	}
	if dup.CreatedAt, err = parseSQLiteTime(created); err != nil {
		return nil, err
	}
	for _, r := range rules {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO rules (monitor_id, type, params, enabled) VALUES (?, ?, ?, ?)`,
			dup.ID, r.Type, string(jsonOrEmpty(r.Params)), boolToInt(r.Enabled)); err != nil {
			return nil, mapSQLiteErr(err)
		}
	}
	for _, cid := range src.ChannelIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO monitor_channels (monitor_id, channel_id) VALUES (?, ?)`,
			dup.ID, cid); err != nil {
			return nil, mapSQLiteErr(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	dup.ChannelIDs = src.ChannelIDs
	return &dup, nil
}

// queryInt64Column runs a single-column int64 query against a queryer.
func queryInt64Column(ctx context.Context, q queryer, query string, args ...any) ([]int64, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// queryStringColumn runs a single-column string query against a queryer.
func queryStringColumn(ctx context.Context, q queryer, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// queryer is the subset of *sql.DB, *sql.Tx and *sql.Conn the read helpers
// need, so they work inside and outside a transaction.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// scanSQLiteMonitor reads one monitors row. Timestamps are scanned as text and
// parsed; last_matched_at may be NULL, which stays a nil pointer rather than a
// fabricated time.
func scanSQLiteMonitor(r rowScanner) (*Monitor, error) {
	var m Monitor
	var ids string
	var enabled int64
	var created string
	var lastMatched sql.NullString
	var priority string
	if err := r.Scan(&m.ID, &m.Name, &ids, &enabled, &created, &lastMatched, &priority, &m.Network); err != nil {
		return nil, mapSQLiteErr(err)
	}
	m.Priority = Priority(priority).Normalized()
	m.Enabled = enabled != 0
	if err := json.Unmarshal([]byte(ids), &m.ContractIDs); err != nil {
		return nil, fmt.Errorf("monitor %d: bad contract_ids: %w", m.ID, err)
	}
	var err error
	if m.CreatedAt, err = parseSQLiteTime(created); err != nil {
		return nil, err
	}
	if lastMatched.Valid && lastMatched.String != "" {
		t, err := parseSQLiteTime(lastMatched.String)
		if err != nil {
			return nil, err
		}
		m.LastMatchedAt = &t
	}
	return &m, nil
}

// --- rules ---
//
// Rules carry no workspace column of their own: their monitor_id foreign key
// is the tenant link, so every rule query here reaches monitors. Denormalising
// the workspace onto rules would give a row two ways to say who owns it, and
// they can disagree.

// rowQuerier is the subset of *sql.DB and *sql.Tx that reads one row, so the
// workspace check below runs unchanged inside or outside a transaction.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// requireMonitorWorkspace fails with ErrNotFound unless monitorID is a monitor
// in ws. Creating a rule on a monitor the caller cannot see fails exactly as
// creating one on a monitor that does not exist does, so the two are
// indistinguishable to the caller.
func (s *SQLite) requireMonitorWorkspace(ctx context.Context, q rowQuerier, monitorID int64, ws workspace.ID) error {
	var ok int64
	if err := q.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM monitors WHERE id = ? AND workspace_id = ?)`,
		monitorID, ws).Scan(&ok); err != nil {
		return mapSQLiteErr(err)
	}
	if ok == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLite) CreateRule(ctx context.Context, r *Rule) error {
	if err := s.requireMonitorWorkspace(ctx, s.db, r.MonitorID, workspaceID(ctx)); err != nil {
		return err
	}
	return mapSQLiteErr(s.db.QueryRowContext(ctx,
		`INSERT INTO rules (monitor_id, type, params, enabled) VALUES (?, ?, ?, ?) RETURNING id`,
		r.MonitorID, r.Type, string(jsonOrEmpty(r.Params)), boolToInt(r.Enabled),
	).Scan(&r.ID))
}

func (s *SQLite) CreateRules(ctx context.Context, rules []*Rule) error {
	if len(rules) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // rollback after commit is a no-op

	ws := workspaceID(ctx)
	for _, r := range rules {
		if err := s.requireMonitorWorkspace(ctx, tx, r.MonitorID, ws); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO rules (monitor_id, type, params, enabled) VALUES (?, ?, ?, ?) RETURNING id`,
			r.MonitorID, r.Type, string(jsonOrEmpty(r.Params)), boolToInt(r.Enabled),
		).Scan(&r.ID); err != nil {
			return mapSQLiteErr(err)
		}
	}
	return tx.Commit()
}

func (s *SQLite) GetRule(ctx context.Context, id int64) (*Rule, error) {
	rules, err := querySQLiteRules(ctx, s.db,
		`SELECT r.id, r.monitor_id, r.type, r.params, r.enabled FROM rules r
		   JOIN monitors m ON m.id = r.monitor_id
		  WHERE r.id = ? AND m.workspace_id = ?`, id, workspaceID(ctx))
	if err != nil {
		return nil, err
	}
	if len(rules) == 0 {
		return nil, ErrNotFound
	}
	return &rules[0], nil
}

func (s *SQLite) ListRules(ctx context.Context, monitorID int64, enabledOnly bool) ([]Rule, error) {
	// The predicate is conditional because ListRules serves two callers with
	// different scopes: the API (one workspace) and the poller's per-monitor
	// rule loading, which runs under the cross-tenant system scope and would
	// otherwise find no rules for a monitor outside the default workspace.
	q := `SELECT r.id, r.monitor_id, r.type, r.params, r.enabled FROM rules r
	         JOIN monitors m ON m.id = r.monitor_id
	        WHERE r.monitor_id = ?`
	args := []any{monitorID}
	if ws, scoped := tenantWorkspace(ctx); scoped {
		q += ` AND m.workspace_id = ?`
		args = append(args, ws)
	}
	if enabledOnly {
		q += ` AND r.enabled = 1`
	}
	q += ` ORDER BY r.id`
	return querySQLiteRules(ctx, s.db, q, args...)
}

func querySQLiteRules(ctx context.Context, q queryer, query string, args ...any) ([]Rule, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Rule
	for rows.Next() {
		r, err := scanSQLiteRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanSQLiteRule(r rowScanner) (Rule, error) {
	var rule Rule
	var params string
	var enabled int64
	if err := r.Scan(&rule.ID, &rule.MonitorID, &rule.Type, &params, &enabled); err != nil {
		return rule, mapSQLiteErr(err)
	}
	rule.Enabled = enabled != 0
	rule.Params = json.RawMessage(params)
	return rule, nil
}

func (s *SQLite) UpdateRule(ctx context.Context, r *Rule) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE rules SET type = ?, params = ?, enabled = ? WHERE id = ?
		  AND monitor_id IN (SELECT id FROM monitors WHERE workspace_id = ?)`,
		r.Type, string(jsonOrEmpty(r.Params)), boolToInt(r.Enabled), r.ID, workspaceID(ctx))
	if err != nil {
		return mapSQLiteErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteRule scopes through the rule's monitor, because rules carry no
// workspace column of their own. SQLite has no DELETE ... USING, so the join is
// written as a subquery — the same two-table check Postgres does with USING.
func (s *SQLite) DeleteRule(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM rules
		  WHERE id = ? AND monitor_id IN (SELECT id FROM monitors WHERE workspace_id = ?)`,
		id, workspaceID(ctx))
	if err != nil {
		return mapSQLiteErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- channels ---

func (s *SQLite) CreateChannel(ctx context.Context, c *Channel) error {
	config, err := configForWrite(s.cipher, c.ID, c.Name, c.Config)
	if err != nil {
		return err
	}
	var created string
	if err := s.db.QueryRowContext(ctx,
		`INSERT INTO channels (name, type, config, enabled, workspace_id) VALUES (?, ?, ?, ?, ?)
		 RETURNING id, created_at`,
		c.Name, c.Type, string(config), boolToInt(c.Enabled), workspaceID(ctx),
	).Scan(&c.ID, &created); err != nil {
		return mapSQLiteErr(err)
	}
	c.CreatedAt, err = parseSQLiteTime(created)
	return err
}

func (s *SQLite) GetChannel(ctx context.Context, id int64) (*Channel, error) {
	channels, err := s.queryChannels(ctx,
		`SELECT id, name, type, config, enabled, created_at FROM channels WHERE id = ? AND workspace_id = ?`,
		id, workspaceID(ctx))
	if err != nil {
		return nil, err
	}
	if len(channels) == 0 {
		return nil, ErrNotFound
	}
	return &channels[0], nil
}

// ListChannels serves the dashboard listing and the notifier's startup
// validation, so it honours the cross-tenant system scope like ListMonitors.
func (s *SQLite) ListChannels(ctx context.Context, enabledOnly bool) ([]Channel, error) {
	q := `SELECT id, name, type, config, enabled, created_at FROM channels WHERE 1 = 1`
	args := []any{}
	if ws, scoped := tenantWorkspace(ctx); scoped {
		q += ` AND workspace_id = ?`
		args = append(args, ws)
	}
	if enabledOnly {
		q += ` AND enabled = 1`
	}
	q += ` ORDER BY id`
	return s.queryChannels(ctx, q, args...)
}

func (s *SQLite) ListChannelsPage(ctx context.Context, f ListFilter) ([]Channel, error) {
	q := `SELECT id, name, type, config, enabled, created_at FROM channels WHERE workspace_id = ?`
	args := []any{workspaceID(ctx)}
	if f.EnabledOnly {
		q += ` AND enabled = 1`
	}
	if f.Type != "" {
		q += ` AND type = ?`
		args = append(args, f.Type)
	}
	if f.AfterID != 0 {
		q += ` AND id < ?`
		args = append(args, f.AfterID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, pageLimit(f.Limit))
	return s.queryChannels(ctx, q, args...)
}

// ListChannelsForMonitor returns the enabled channels one monitor alerts to.
// Both sides are checked: the monitor's workspace (so a caller cannot read
// another tenant's notification targets by id) and the channel's own, which
// stops a stale attachment from routing this workspace's alerts into a channel
// it does not own. The dispatcher runs cross-tenant, so the predicate is
// conditional here as it is in ListMonitors.
func (s *SQLite) ListChannelsForMonitor(ctx context.Context, monitorID int64) ([]Channel, error) {
	q := `SELECT c.id, c.name, c.type, c.config, c.enabled, c.created_at
	     FROM channels c
	     JOIN monitor_channels mc ON mc.channel_id = c.id
	     JOIN monitors m ON m.id = mc.monitor_id
	     WHERE mc.monitor_id = ? AND c.enabled = 1`
	args := []any{monitorID}
	if ws, scoped := tenantWorkspace(ctx); scoped {
		q += ` AND c.workspace_id = ? AND m.workspace_id = ?`
		args = append(args, ws, ws)
	}
	q += ` ORDER BY c.id`
	return s.queryChannels(ctx, q, args...)
}

func (s *SQLite) queryChannels(ctx context.Context, query string, args ...any) ([]Channel, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Channel
	for rows.Next() {
		c, err := s.scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// scanChannel reads one channels row and decrypts its config, so every caller
// up the stack (API, dashboard, dispatcher) sees plaintext.
func (s *SQLite) scanChannel(r rowScanner) (Channel, error) {
	var c Channel
	var config string
	var enabled int64
	var created string
	if err := r.Scan(&c.ID, &c.Name, &c.Type, &config, &enabled, &created); err != nil {
		return c, mapSQLiteErr(err)
	}
	var err error
	if c.CreatedAt, err = parseSQLiteTime(created); err != nil {
		return c, err
	}
	c.Enabled = enabled != 0
	c.Config = json.RawMessage(config)
	if err := decryptChannel(s.cipher, &c); err != nil {
		return c, err
	}
	return c, nil
}

func (s *SQLite) UpdateChannel(ctx context.Context, c *Channel) error {
	config, err := configForWrite(s.cipher, c.ID, c.Name, c.Config)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE channels SET name = ?, type = ?, config = ?, enabled = ? WHERE id = ? AND workspace_id = ?`,
		c.Name, c.Type, string(config), boolToInt(c.Enabled), c.ID, workspaceID(ctx))
	if err != nil {
		return mapSQLiteErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLite) DeleteChannel(ctx context.Context, id int64) error {
	return s.deleteByID(ctx, "channels", id)
}

// --- alerts ---

// CreateAlert mirrors the Postgres implementation, including the cooldown
// semantics. Where Postgres locks the rule row with SELECT ... FOR UPDATE, the
// SQLite store relies on BEGIN IMMEDIATE (the DSN's _txlock) plus the single
// connection: the database-wide write lock is held for the whole read-then-
// write decision, so two callers can never both fire inside one window.
func (s *SQLite) CreateAlert(ctx context.Context, a *Alert) (AlertOutcome, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }() // rollback after commit is a no-op

	if a.Cooldown > 0 {
		var suppressed int64
		var lastAlert sql.NullString
		if err := tx.QueryRowContext(ctx,
			`SELECT suppressed_since_last, last_alert_at FROM rules WHERE id = ?`,
			a.RuleID).Scan(&suppressed, &lastAlert); err != nil {
			return "", mapSQLiteErr(err)
		}

		inCooldown := false
		if lastAlert.Valid && lastAlert.String != "" {
			last, err := parseSQLiteTime(lastAlert.String)
			if err != nil {
				return "", err
			}
			inCooldown = last.Add(a.Cooldown).After(time.Now().UTC())
		}

		// A replayed event is a duplicate, not a fresh match, so it must not
		// inflate the suppressed count.
		var duplicate int64
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM alerts WHERE rule_id = ? AND event_id = ?)`,
			a.RuleID, a.EventID).Scan(&duplicate); err != nil {
			return "", err
		}
		if duplicate != 0 {
			return AlertDuplicate, nil
		}

		if inCooldown {
			if _, err := tx.ExecContext(ctx,
				`UPDATE rules SET suppressed_since_last = suppressed_since_last + 1 WHERE id = ?`,
				a.RuleID); err != nil {
				return "", err
			}
			if err := tx.Commit(); err != nil {
				return "", err
			}
			return AlertSuppressed, nil
		}

		// The count is read before the insert so it can ride on the payload the
		// alert is written with, and the window is closed in the same
		// transaction so a crash cannot lose a suppression count.
		a.SuppressedSinceLast = suppressed
		a.Payload = WithSuppressed(a.Payload, suppressed)
	}

	var id int64
	var created string
	err = tx.QueryRowContext(ctx,
		// The alert's workspace and network come from its monitor rather than
		// from ctx, exactly as in the Postgres store: the poller writes under a
		// cross-tenant context, so a caller-supplied scope would be wrong here,
		// and the network is what a reorg on that chain may retract and no other
		// chain's reorg may touch.
		// The rules join also makes the write fail when the named rule belongs
		// to a different monitor — alerts' two foreign keys are independent, so
		// without it an alert could name another monitor's rule and pass both.
		`INSERT INTO alerts (monitor_id, rule_id, event_id, payload, ledger, workspace_id, network)
		 SELECT ?, ?, ?, ?, ?, m.workspace_id, m.network
		   FROM monitors m
		   JOIN rules r ON r.id = ? AND r.monitor_id = m.id
		  WHERE m.id = ?
		 ON CONFLICT (rule_id, event_id) DO NOTHING
		 RETURNING id, created_at, network`,
		a.MonitorID, a.RuleID, a.EventID, string(jsonOrEmpty(a.Payload)), int64(a.Ledger),
		a.RuleID, a.MonitorID,
	).Scan(&id, &created, &a.Network)
	if errors.Is(err, sql.ErrNoRows) {
		// No row can mean two different things: the event was already alerted
		// on (the dedup unique index swallowed the insert), or the monitor and
		// rule do not form a real pair. Only the first is a success, so the
		// cooldown path's existence check runs again here for the rare case.
		var dup int64
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM alerts WHERE rule_id = ? AND event_id = ?)`,
			a.RuleID, a.EventID).Scan(&dup); err != nil {
			return "", err
		}
		if dup != 0 {
			return AlertDuplicate, nil
		}
		return "", ErrNotFound
	}
	if err != nil {
		return "", mapSQLiteErr(err)
	}
	a.ID = id
	if a.CreatedAt, err = parseSQLiteTime(created); err != nil {
		return "", err
	}

	if a.Cooldown > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE rules SET last_alert_at = ?, suppressed_since_last = 0 WHERE id = ?`,
			sqliteTimeString(time.Now()), a.RuleID); err != nil {
			return "", err
		}
	}

	// Stamp last_matched_at with the event's ledger close time, never wall
	// clock, and only move it forward so a replayed older event cannot make a
	// live monitor look stale.
	if !a.LedgerClosedAt.IsZero() {
		closed := sqliteTimeString(a.LedgerClosedAt)
		if _, err := tx.ExecContext(ctx,
			`UPDATE monitors SET last_matched_at = ?
			 WHERE id = ? AND (last_matched_at IS NULL OR last_matched_at < ?)`,
			closed, a.MonitorID, closed); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return AlertCreated, nil
}

func (s *SQLite) GetAlert(ctx context.Context, id int64) (*Alert, error) {
	a, err := scanSQLiteAlert(s.db.QueryRowContext(ctx,
		`SELECT id, monitor_id, rule_id, event_id, payload, created_at, ledger, retracted_at, network
		   FROM alerts WHERE id = ? AND workspace_id = ?`, id, workspaceID(ctx)))
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *SQLite) ListAlerts(ctx context.Context, f AlertFilter) ([]Alert, error) {
	q := `SELECT id, monitor_id, rule_id, event_id, payload, created_at, ledger, retracted_at, network FROM alerts WHERE 1 = 1`
	args := []any{}
	// Conditional because ListAlerts serves two callers with different scopes:
	// the API listing (one workspace) and the frequency rule's match-log
	// rebuild, which runs inside the poller's cross-tenant context keyed to one
	// rule.
	if ws, scoped := tenantWorkspace(ctx); scoped {
		q += ` AND workspace_id = ?`
		args = append(args, ws)
	}
	if f.MonitorID != 0 {
		q += ` AND monitor_id = ?`
		args = append(args, f.MonitorID)
	}
	if f.RuleID != 0 {
		q += ` AND rule_id = ?`
		args = append(args, f.RuleID)
	}
	if f.ContractID != "" {
		q += ` AND json_extract(payload, '$.contract_id') = ?`
		args = append(args, f.ContractID)
	}
	if f.Network != "" {
		q += ` AND network = ?`
		args = append(args, f.Network)
	}
	if !f.From.IsZero() {
		q += ` AND created_at >= ?`
		args = append(args, sqliteTimeString(f.From))
	}
	if !f.To.IsZero() {
		q += ` AND created_at < ?`
		args = append(args, sqliteTimeString(f.To))
	}
	sort := alertSort(f.Sort)
	if f.AfterID != 0 {
		// Subquery the cursor row so the comparison uses the same
		// (created_at, id) pair the ORDER BY does; a one-sided id comparison
		// would skip or repeat rows once two alerts share a timestamp. The
		// lookup stays keyed by primary key alone: this statement's workspace
		// predicate is conditional (the poller lists across tenants), and a
		// tenant-scoped cursor can only ever name a row the caller was already
		// shown, so it cannot pull another workspace's rows into the page.
		cursor := `(SELECT created_at, id FROM alerts WHERE id = ?)`
		if sort == "created_at_asc" {
			q += ` AND (created_at, id) > ` + cursor
		} else {
			q += ` AND (created_at, id) < ` + cursor
		}
		args = append(args, f.AfterID)
	}
	if sort == "created_at_asc" {
		q += ` ORDER BY created_at ASC, id ASC`
	} else {
		q += ` ORDER BY created_at DESC, id DESC`
	}
	q += ` LIMIT ?`
	args = append(args, pageLimit(f.Limit))

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Alert
	for rows.Next() {
		a, err := scanSQLiteAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func scanSQLiteAlert(r rowScanner) (Alert, error) {
	var a Alert
	var payload string
	var created string
	var ledger int64
	var retracted sql.NullString
	if err := r.Scan(&a.ID, &a.MonitorID, &a.RuleID, &a.EventID, &payload, &created, &ledger, &retracted, &a.Network); err != nil {
		return a, mapSQLiteErr(err)
	}
	a.Payload = json.RawMessage(payload)
	a.Ledger = uint32(ledger)
	var err error
	if a.CreatedAt, err = parseSQLiteTime(created); err != nil {
		return a, err
	}
	if retracted.Valid && retracted.String != "" {
		t, err := parseSQLiteTime(retracted.String)
		if err != nil {
			return a, err
		}
		a.RetractedAt = &t
	}
	return a, nil
}

// RecordDeliveryAttempt attaches an attempt to its alert. The alert id is
// re-read under the caller's workspace instead of being trusted, so an attempt
// cannot be filed against another tenant's alert, and the single-column
// foreign key that made the row valid still reports a missing alert.
//
// The predicate is conditional because the dispatcher records attempts from the
// poller's cross-tenant context.
func (s *SQLite) RecordDeliveryAttempt(ctx context.Context, d *DeliveryAttempt) error {
	q := `INSERT INTO delivery_attempts (alert_id, channel_id, status, response_snippet)
		  SELECT a.id, ?, ?, ? FROM alerts a WHERE a.id = ?`
	args := []any{d.ChannelID, d.Status, d.ResponseSnippet, d.AlertID}
	if ws, scoped := tenantWorkspace(ctx); scoped {
		q += ` AND a.workspace_id = ?`
		args = append(args, ws)
	}
	var attempted string
	if err := s.db.QueryRowContext(ctx, q+`
		 RETURNING id, attempted_at`, args...).Scan(&d.ID, &attempted); err != nil {
		return mapSQLiteErr(err)
	}
	t, err := parseSQLiteTime(attempted)
	if err != nil {
		return err
	}
	d.AttemptedAt = t
	return nil
}

// ListDeliveryAttempts scopes through its alert: delivery_attempts carries no
// workspace column, and an alert id only means anything inside the workspace
// that can see the alert behind it.
func (s *SQLite) ListDeliveryAttempts(ctx context.Context, alertID int64, status string) ([]DeliveryAttempt, error) {
	q := `SELECT da.id, da.alert_id, da.channel_id, da.status, da.response_snippet, da.attempted_at
	     FROM delivery_attempts da
	     JOIN alerts a ON a.id = da.alert_id
	     WHERE da.alert_id = ? AND a.workspace_id = ?`
	args := []any{alertID, workspaceID(ctx)}
	if status != "" {
		// Applied in SQL so a busy alert does not ship every attempt just so
		// the client can throw most of them away.
		q += ` AND da.status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY da.id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DeliveryAttempt
	for rows.Next() {
		var d DeliveryAttempt
		var attempted string
		if err := rows.Scan(&d.ID, &d.AlertID, &d.ChannelID, &d.Status, &d.ResponseSnippet, &attempted); err != nil {
			return nil, err
		}
		if d.AttemptedAt, err = parseSQLiteTime(attempted); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeleteExpiredAlerts removes up to limit alerts older than cutoff.
// delivery_attempts follow via ON DELETE CASCADE, which the DSN's
// foreign_keys(1) pragma enforces.
func (s *SQLite) DeleteExpiredAlerts(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if limit <= 0 {
		limit = DefaultPruneBatch
	}
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM alerts
		WHERE id IN (
			SELECT id FROM alerts
			WHERE created_at < ?
			ORDER BY created_at ASC, id ASC
			LIMIT ?
		)`, sqliteTimeString(cutoff), limit)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ExpiredAlerts returns up to limit alerts older than cutoff, oldest first,
// with the same ordering DeleteExpiredAlerts uses.
func (s *SQLite) ExpiredAlerts(ctx context.Context, cutoff time.Time, limit int) ([]Alert, error) {
	if limit <= 0 {
		limit = DefaultPruneBatch
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, monitor_id, rule_id, event_id, payload, created_at, ledger, retracted_at, network
		   FROM alerts WHERE created_at < ? ORDER BY created_at ASC, id ASC LIMIT ?`,
		sqliteTimeString(cutoff), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Alert
	for rows.Next() {
		a, err := scanSQLiteAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// --- ledger hashes and reorg retraction ---

// The empty network keeps the original single-column ledger_hashes window and
// every named network shares network_ledger_hashes, keyed (network, ledger).
// Two chains number their ledgers independently, so one shared window would
// report a reorganisation on nearly every cycle — and a false positive here
// retracts real alerts. See the 0013_networks migration for the same note on
// the Postgres side, including why starting a named network's window empty is
// safe rather than lossy.

func (s *SQLite) RecordLedgerHashes(ctx context.Context, network string, hashes []LedgerHash) error {
	if len(hashes) == 0 {
		return nil
	}
	const legacyInsert = `INSERT INTO ledger_hashes (ledger, hash) VALUES (?, ?)
		ON CONFLICT (ledger) DO UPDATE SET hash = excluded.hash,
		                                   observed_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE ledger_hashes.hash IS NOT excluded.hash`
	const scopedInsert = `INSERT INTO network_ledger_hashes (network, ledger, hash) VALUES (?, ?, ?)
		ON CONFLICT (network, ledger) DO UPDATE SET hash = excluded.hash,
		                                   observed_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE network_ledger_hashes.hash IS NOT excluded.hash`

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // rollback after commit is a no-op
	for _, h := range hashes {
		var err error
		if network == "" {
			_, err = tx.ExecContext(ctx, legacyInsert, int64(h.Ledger), h.Hash)
		} else {
			_, err = tx.ExecContext(ctx, scopedInsert, network, int64(h.Ledger), h.Hash)
		}
		if err != nil {
			return mapSQLiteErr(err)
		}
	}
	return tx.Commit()
}

func (s *SQLite) LedgerHashes(ctx context.Context, network string, from, to uint32) ([]LedgerHash, error) {
	q := `SELECT ledger, hash FROM network_ledger_hashes WHERE network = ? AND ledger >= ? AND ledger <= ? ORDER BY ledger`
	args := []any{network, int64(from), int64(to)}
	if network == "" {
		q = `SELECT ledger, hash FROM ledger_hashes WHERE ledger >= ? AND ledger <= ? ORDER BY ledger`
		args = args[1:]
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []LedgerHash
	for rows.Next() {
		var h LedgerHash
		var ledger int64
		if err := rows.Scan(&ledger, &h.Hash); err != nil {
			return nil, err
		}
		h.Ledger = uint32(ledger)
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *SQLite) PruneLedgerHashes(ctx context.Context, network string, before uint32) error {
	if network == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM ledger_hashes WHERE ledger < ?`, int64(before))
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM network_ledger_hashes WHERE network = ? AND ledger < ?`,
		network, int64(before))
	return err
}

// RetractAlertsFromLedger retracts one network's alerts from `ledger` up. The
// network predicate is what stops a testnet reorg from retracting mainnet
// alerts that happen to sit at the same height.
func (s *SQLite) RetractAlertsFromLedger(ctx context.Context, network string, ledger uint32, at time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE alerts SET retracted_at = ? WHERE network = ? AND ledger >= ? AND retracted_at IS NULL`,
		sqliteTimeString(at), network, int64(ledger))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- ingest state ---

// GetIngestState reads one network's checkpoint. The empty network is the
// pre-multi-network row and behaves exactly as it always did; a named network
// reads network_ingest_state, which is seeded from that legacy row the first
// time any network asks, so an instance that moves from one NETWORK to a
// network list resumes at the ledger it stopped on instead of cold-starting at
// the tip and silently missing everything in between.
func (s *SQLite) GetIngestState(ctx context.Context, network string) (IngestState, error) {
	var st IngestState
	var lastLedger int64
	var updated string

	q := `SELECT last_ledger, last_cursor, updated_at FROM network_ingest_state WHERE network = ?`
	args := []any{network}
	if network == "" {
		q = `SELECT last_ledger, last_cursor, updated_at FROM ingest_state WHERE id = 1`
		args = nil
	} else {
		// Only while the per-network table is empty does the legacy checkpoint
		// get claimed, so a second network added later cold-starts on its own
		// chain rather than inheriting a cursor from a chain that has nothing to
		// do with it. Both statements are DO NOTHING, so two pollers starting at
		// the same moment cannot race into an error.
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO network_ingest_state (network, last_ledger, last_cursor)
			SELECT ?, last_ledger, last_cursor FROM ingest_state
			 WHERE id = 1 AND NOT EXISTS (SELECT 1 FROM network_ingest_state)
			ON CONFLICT (network) DO NOTHING`, network); err != nil {
			return st, mapSQLiteErr(err)
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO network_ingest_state (network) VALUES (?) ON CONFLICT (network) DO NOTHING`,
			network); err != nil {
			return st, mapSQLiteErr(err)
		}
	}

	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&lastLedger, &st.LastCursor, &updated); err != nil {
		return st, mapSQLiteErr(err)
	}
	st.LastLedger = uint32(lastLedger)
	var err error
	if st.UpdatedAt, err = parseSQLiteTime(updated); err != nil {
		return st, err
	}
	return st, nil
}

// SetIngestState advances one network's checkpoint. Writing a named network is
// an upsert because the row may not exist yet: the first cycle of a newly
// configured network has nothing to update.
func (s *SQLite) SetIngestState(ctx context.Context, network string, st IngestState) error {
	if network == "" {
		_, err := s.db.ExecContext(ctx,
			`UPDATE ingest_state SET last_ledger = ?, last_cursor = ?, updated_at = ? WHERE id = 1`,
			int64(st.LastLedger), st.LastCursor, sqliteTimeString(time.Now()))
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO network_ingest_state (network, last_ledger, last_cursor, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (network) DO UPDATE
		    SET last_ledger = excluded.last_ledger,
		        last_cursor = excluded.last_cursor,
		        updated_at  = excluded.updated_at`,
		network, int64(st.LastLedger), st.LastCursor, sqliteTimeString(time.Now()))
	return mapSQLiteErr(err)
}

// --- stats ---

// GetStats reports the counts for one workspace. Every figure is scoped,
// including the rules count (rules reach a workspace through their monitor).
// The ingest checkpoints are instance-wide and stay unscoped: they are the
// pollers' cursors, not a tenant's data.
func (s *SQLite) GetStats(ctx context.Context) (Stats, error) {
	var st Stats
	// The 24h window is computed in Go and compared as text; the fixed-width
	// timestamp format makes that a valid chronological comparison.
	cutoff := sqliteTimeString(time.Now().UTC().Add(-24 * time.Hour))
	ws := workspaceID(ctx)
	err := s.db.QueryRowContext(ctx, `
		SELECT
			(SELECT count(*) FROM monitors WHERE workspace_id = ?),
			(SELECT count(*) FROM rules r JOIN monitors m ON m.id = r.monitor_id WHERE m.workspace_id = ?),
			(SELECT count(*) FROM channels WHERE workspace_id = ?),
			(SELECT count(*) FROM alerts WHERE workspace_id = ?),
			(SELECT count(*) FROM alerts WHERE workspace_id = ? AND created_at > ?)`, ws, ws, ws, ws, ws, cutoff).
		Scan(&st.Monitors, &st.Rules, &st.Channels, &st.Alerts, &st.AlertsLast24)
	if err != nil {
		return st, err
	}
	cps, err := s.ingestCheckpoints(ctx)
	if err != nil {
		return st, err
	}
	summarizeCheckpoints(&st, cps)
	return st, nil
}

// ingestCheckpoints reads every network's cursor, newest poll first. The legacy
// row competes for that ordering on the same terms as a named one, so whichever
// checkpoint polled most recently is the one that reports where this instance
// actually is.
func (s *SQLite) ingestCheckpoints(ctx context.Context) ([]NetworkStats, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT network, last_ledger, updated_at FROM network_ingest_state
		UNION ALL
		SELECT '', last_ledger, updated_at FROM ingest_state WHERE id = 1
		ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []NetworkStats
	for rows.Next() {
		var c NetworkStats
		var lastLedger int64
		var updated string
		if err := rows.Scan(&c.Network, &lastLedger, &updated); err != nil {
			return nil, err
		}
		c.LastLedger = uint32(lastLedger)
		if c.LastPollAt, err = parseSQLiteTime(updated); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AssignLegacyNetwork labels the rows written before the network column
// existed, once, at startup, in the instance's own cross-tenant scope: an
// operator upgrading a single-network deployment must not watch every monitor
// vanish from the network-filtered listing. Idempotent because the predicate
// only matches the empty value.
func (s *SQLite) AssignLegacyNetwork(ctx context.Context, network string) (int64, error) {
	if network == "" {
		return 0, nil // nothing to label: no network configured
	}
	var total int64
	for _, q := range []string{
		`UPDATE monitors SET network = ? WHERE network = ''`,
		`UPDATE alerts SET network = ? WHERE network = ''`,
	} {
		res, err := s.db.ExecContext(ctx, q, network)
		if err != nil {
			return total, mapSQLiteErr(err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// AlertCountsByDay returns `days` consecutive UTC calendar days ending today,
// zero-filled. Bucketing runs in SQL: substr(created_at, 1, 10) is the UTC day
// of the fixed-format timestamp, and a recursive CTE generates the window so a
// quiet day is an explicit 0 rather than a missing bar. SQLite has no
// generate_series, hence the CTE.
//
// The workspace filter belongs in the LEFT JOIN's ON clause, not the WHERE
// clause: filtering the joined table after the fact would drop the days with no
// alerts in this workspace and reintroduce the chart gaps the CTE exists to
// prevent.
func (s *SQLite) AlertCountsByDay(ctx context.Context, days int) ([]AlertDayCount, error) {
	days = ClampAlertSeriesDays(days)
	rows, err := s.db.QueryContext(ctx, `
		WITH RECURSIVE window(day) AS (
			SELECT date('now')
			UNION ALL
			SELECT date(day, '-1 day') FROM window WHERE day > date('now', ?)
		)
		SELECT window.day, COUNT(a.id)
		FROM window
		LEFT JOIN alerts a ON substr(a.created_at, 1, 10) = window.day
			AND a.workspace_id = ?
		GROUP BY window.day
		ORDER BY window.day`, fmt.Sprintf("-%d days", days-1), workspaceID(ctx))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]AlertDayCount, 0, days)
	for rows.Next() {
		var day string
		var count int64
		if err := rows.Scan(&day, &count); err != nil {
			return nil, err
		}
		out = append(out, AlertDayCount{Day: day, Count: count})
	}
	return out, rows.Err()
}

// --- saved searches ---

func (s *SQLite) CreateSavedSearch(ctx context.Context, ss *SavedSearch) error {
	filter, err := json.Marshal(ss.Filter)
	if err != nil {
		return err
	}
	ws := workspaceID(ctx)
	if ss.IsDefault {
		// Scoped, or making one workspace's search the default would clear every
		// other workspace's. The same reasoning applies to the two default-search
		// methods below.
		if _, err := s.db.ExecContext(ctx,
			`UPDATE saved_searches SET is_default = 0 WHERE is_default = 1 AND workspace_id = ?`, ws); err != nil {
			return err
		}
	}
	var created string
	if err := s.db.QueryRowContext(ctx,
		`INSERT INTO saved_searches (name, filter, is_default, workspace_id) VALUES (?, ?, ?, ?) RETURNING id, created_at`,
		ss.Name, string(filter), boolToInt(ss.IsDefault), ws).Scan(&ss.ID, &created); err != nil {
		return mapSQLiteErr(err)
	}
	ss.CreatedAt, err = parseSQLiteTime(created)
	return err
}

func (s *SQLite) ListSavedSearches(ctx context.Context) ([]SavedSearch, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, filter, is_default, created_at FROM saved_searches WHERE workspace_id = ? ORDER BY name`,
		workspaceID(ctx))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SavedSearch
	for rows.Next() {
		ss, err := scanSQLiteSavedSearch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ss)
	}
	return out, rows.Err()
}

func (s *SQLite) GetSavedSearch(ctx context.Context, id int64) (*SavedSearch, error) {
	ss, err := scanSQLiteSavedSearch(s.db.QueryRowContext(ctx,
		`SELECT id, name, filter, is_default, created_at FROM saved_searches WHERE id = ? AND workspace_id = ?`,
		id, workspaceID(ctx)))
	if err != nil {
		return nil, err
	}
	return &ss, nil
}

func scanSQLiteSavedSearch(r rowScanner) (SavedSearch, error) {
	var ss SavedSearch
	var filter string
	var isDefault int64
	var created string
	if err := r.Scan(&ss.ID, &ss.Name, &filter, &isDefault, &created); err != nil {
		return ss, mapSQLiteErr(err)
	}
	ss.IsDefault = isDefault != 0
	_ = json.Unmarshal([]byte(filter), &ss.Filter)
	var err error
	if ss.CreatedAt, err = parseSQLiteTime(created); err != nil {
		return ss, err
	}
	return ss, nil
}

func (s *SQLite) DeleteSavedSearch(ctx context.Context, id int64) error {
	return s.deleteByID(ctx, "saved_searches", id)
}

// SetDefaultSearch clears any existing default and sets the requested row in
// one transaction, so the partial unique index never sees two defaults.
func (s *SQLite) SetDefaultSearch(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // rollback after commit is a no-op

	ws := workspaceID(ctx)
	if _, err := tx.ExecContext(ctx,
		`UPDATE saved_searches SET is_default = 0 WHERE is_default = 1 AND workspace_id = ?`, ws); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE saved_searches SET is_default = 1 WHERE id = ? AND workspace_id = ?`, id, ws)
	if err != nil {
		return mapSQLiteErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *SQLite) ClearDefaultSearch(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE saved_searches SET is_default = 0 WHERE id = ? AND workspace_id = ?`, id, workspaceID(ctx))
	if err != nil {
		return mapSQLiteErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- monitor templates ---

func (s *SQLite) CreateMonitorTemplate(ctx context.Context, t *MonitorTemplate) error {
	rulesJSON, _ := json.Marshal(t.Rules)
	channelJSON, _ := json.Marshal(t.ChannelIDs)
	paramsJSON, _ := json.Marshal(t.Parameters)
	var created string
	if err := s.db.QueryRowContext(ctx,
		`INSERT INTO monitor_templates (name, description, rules, channel_ids, parameters, workspace_id) VALUES (?, ?, ?, ?, ?, ?) RETURNING id, created_at`,
		t.Name, t.Description, string(rulesJSON), string(channelJSON), string(paramsJSON), workspaceID(ctx)).Scan(&t.ID, &created); err != nil {
		return mapSQLiteErr(err)
	}
	var err error
	t.CreatedAt, err = parseSQLiteTime(created)
	return err
}

func (s *SQLite) GetMonitorTemplate(ctx context.Context, id int64) (*MonitorTemplate, error) {
	t, err := scanSQLiteTemplate(s.db.QueryRowContext(ctx,
		`SELECT id, name, description, rules, channel_ids, parameters, created_at FROM monitor_templates WHERE id = ? AND workspace_id = ?`,
		id, workspaceID(ctx)))
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *SQLite) ListMonitorTemplates(ctx context.Context) ([]MonitorTemplate, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, description, rules, channel_ids, parameters, created_at FROM monitor_templates WHERE workspace_id = ? ORDER BY name`,
		workspaceID(ctx))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []MonitorTemplate
	for rows.Next() {
		t, err := scanSQLiteTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func scanSQLiteTemplate(r rowScanner) (MonitorTemplate, error) {
	var t MonitorTemplate
	var rulesJSON, channelJSON, paramsJSON, created string
	if err := r.Scan(&t.ID, &t.Name, &t.Description, &rulesJSON, &channelJSON, &paramsJSON, &created); err != nil {
		return t, mapSQLiteErr(err)
	}
	_ = json.Unmarshal([]byte(rulesJSON), &t.Rules)
	_ = json.Unmarshal([]byte(channelJSON), &t.ChannelIDs)
	_ = json.Unmarshal([]byte(paramsJSON), &t.Parameters)
	if t.ChannelIDs == nil {
		t.ChannelIDs = []int64{}
	}
	var err error
	if t.CreatedAt, err = parseSQLiteTime(created); err != nil {
		return t, err
	}
	return t, nil
}

func (s *SQLite) UpdateMonitorTemplate(ctx context.Context, t *MonitorTemplate) error {
	rulesJSON, _ := json.Marshal(t.Rules)
	channelJSON, _ := json.Marshal(t.ChannelIDs)
	paramsJSON, _ := json.Marshal(t.Parameters)
	res, err := s.db.ExecContext(ctx,
		`UPDATE monitor_templates SET name = ?, description = ?, rules = ?, channel_ids = ?, parameters = ? WHERE id = ? AND workspace_id = ?`,
		t.Name, t.Description, string(rulesJSON), string(channelJSON), string(paramsJSON), t.ID, workspaceID(ctx))
	if err != nil {
		return mapSQLiteErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLite) DeleteMonitorTemplate(ctx context.Context, id int64) error {
	return s.deleteByID(ctx, "monitor_templates", id)
}

// --- api tokens ---
//
// The SQLite mirror of the Postgres section in postgres.go, including why the
// digest arrives pre-hashed and why TokenByHash is the one read that is not
// workspace-scoped.

func (s *SQLite) CreateAPIToken(ctx context.Context, t *auth.Token) error {
	var expiresAt any
	if !t.ExpiresAt.IsZero() {
		expiresAt = sqliteTimeString(t.ExpiresAt)
	}
	ws := workspaceID(ctx)
	var created string
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO api_tokens (workspace_id, name, token_hash, prefix, scopes, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?) RETURNING id, created_at`,
		ws, t.Name, t.Hash, t.Prefix, auth.JoinScopes(t.Scopes), expiresAt).
		Scan(&t.ID, &created)
	if err != nil {
		return mapSQLiteErr(err)
	}
	if t.CreatedAt, err = parseSQLiteTime(created); err != nil {
		return err
	}
	t.Workspace = ws
	return nil
}

func (s *SQLite) TokenByHash(ctx context.Context, hash string) (*auth.Token, bool, error) {
	t, err := scanSQLiteAPIToken(s.db.QueryRowContext(ctx,
		`SELECT id, workspace_id, name, token_hash, prefix, scopes, expires_at, last_used_at, revoked_at, created_at FROM api_tokens WHERE token_hash = ?`, hash))
	if errors.Is(err, ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &t, true, nil
}

func (s *SQLite) ListAPITokens(ctx context.Context) ([]auth.Token, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, workspace_id, name, token_hash, prefix, scopes, expires_at, last_used_at, revoked_at, created_at FROM api_tokens WHERE workspace_id = ? ORDER BY id DESC`,
		workspaceID(ctx))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []auth.Token
	for rows.Next() {
		t, err := scanSQLiteAPIToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *SQLite) RevokeAPIToken(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE api_tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ? AND workspace_id = ?`,
		sqliteTimeString(time.Now()), id, workspaceID(ctx))
	if err != nil {
		return mapSQLiteErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLite) TouchAPIToken(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE api_tokens SET last_used_at = ? WHERE id = ? AND workspace_id = ?`,
		sqliteTimeString(at), id, workspaceID(ctx))
	return mapSQLiteErr(err)
}

// scanSQLiteAPIToken reads one api_tokens row; the two listings spell the
// column order identically to it, as the alert scans do.
// The nullable timestamps are sql.NullString because database/sql has no
// pointer-to-pointer destination; an absent value becomes the zero time, which
// is what auth.Token.Live and "never used" both test.
func scanSQLiteAPIToken(r rowScanner) (auth.Token, error) {
	var t auth.Token
	var scopes string
	var created string
	var expiresAt, lastUsedAt, revokedAt sql.NullString
	if err := r.Scan(&t.ID, &t.Workspace, &t.Name, &t.Hash, &t.Prefix, &scopes,
		&expiresAt, &lastUsedAt, &revokedAt, &created); err != nil {
		return t, mapSQLiteErr(err)
	}
	t.Scopes = auth.SplitScopes(scopes)
	for _, col := range []struct {
		dst *time.Time
		val sql.NullString
	}{
		{&t.ExpiresAt, expiresAt}, {&t.LastUsedAt, lastUsedAt}, {&t.RevokedAt, revokedAt},
	} {
		if !col.val.Valid {
			continue
		}
		v, err := parseSQLiteTime(col.val.String)
		if err != nil {
			return t, err
		}
		*col.dst = v
	}
	var err error
	if t.CreatedAt, err = parseSQLiteTime(created); err != nil {
		return t, err
	}
	return t, nil
}
