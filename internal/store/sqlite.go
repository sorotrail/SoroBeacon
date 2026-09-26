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

// --- monitors ---

func (s *SQLite) CreateMonitor(ctx context.Context, m *Monitor) error {
	ids, err := json.Marshal(m.ContractIDs)
	if err != nil {
		return err
	}
	var created string
	if err := s.db.QueryRowContext(ctx,
		`INSERT INTO monitors (name, contract_ids, enabled, priority) VALUES (?, ?, ?, ?)
		 RETURNING id, created_at`,
		m.Name, string(ids), boolToInt(m.Enabled), string(m.Priority.Normalized()),
	).Scan(&m.ID, &created); err != nil {
		return mapSQLiteErr(err)
	}
	m.CreatedAt, err = parseSQLiteTime(created)
	return err
}

func (s *SQLite) GetMonitor(ctx context.Context, id int64) (*Monitor, error) {
	m, err := scanSQLiteMonitor(s.db.QueryRowContext(ctx,
		`SELECT id, name, contract_ids, enabled, created_at, last_matched_at, priority FROM monitors WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	m.ChannelIDs, err = s.monitorChannelIDs(ctx, id)
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (s *SQLite) monitorChannelIDs(ctx context.Context, monitorID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT channel_id FROM monitor_channels WHERE monitor_id = ? ORDER BY channel_id`, monitorID)
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

func (s *SQLite) ListMonitors(ctx context.Context, enabledOnly bool) ([]Monitor, error) {
	q := `SELECT id, name, contract_ids, enabled, created_at, last_matched_at, priority FROM monitors`
	if enabledOnly {
		q += ` WHERE enabled = 1`
	}
	q += ` ORDER BY id`
	return s.queryMonitors(ctx, q)
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
	q := `SELECT id, name, contract_ids, enabled, created_at, last_matched_at, priority FROM monitors WHERE 1 = 1`
	args := []any{}
	if f.Query != "" {
		// instr + lower is a parameterized substring match without LIKE
		// metacharacters, so a search for "100%" cannot become a wildcard.
		q += ` AND instr(lower(name), lower(?)) > 0`
		args = append(args, f.Query)
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
		switch sort {
		case "name":
			q += ` AND (lower(name), id) > (SELECT lower(name), id FROM monitors WHERE id = ?)`
		case "created_at":
			q += ` AND (created_at, id) < (SELECT created_at, id FROM monitors WHERE id = ?)`
		default:
			q += ` AND id < ?`
		}
		args = append(args, f.AfterID)
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
		`UPDATE monitors SET name = ?, contract_ids = ?, enabled = ?, priority = ? WHERE id = ?`,
		m.Name, string(ids), boolToInt(m.Enabled), string(m.Priority.Normalized()), m.ID)
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
	args := make([]any, 0, len(uniq)+1)
	args = append(args, boolToInt(enabled))
	for _, id := range uniq {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx,
		`UPDATE monitors SET enabled = ? WHERE id IN (`+placeholders(len(uniq))+`) RETURNING id`, args...)
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

func (s *SQLite) deleteByID(ctx context.Context, table string, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE id = ?`, id)
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

func (s *SQLite) SetMonitorChannels(ctx context.Context, monitorID int64, channelIDs []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // rollback after commit is a no-op

	if _, err := tx.ExecContext(ctx, `DELETE FROM monitor_channels WHERE monitor_id = ?`, monitorID); err != nil {
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

	src, err := scanSQLiteMonitor(tx.QueryRowContext(ctx,
		`SELECT id, name, contract_ids, enabled, created_at, last_matched_at, priority FROM monitors WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	src.ChannelIDs, err = queryInt64Column(ctx, tx,
		`SELECT channel_id FROM monitor_channels WHERE monitor_id = ? ORDER BY channel_id`, id)
	if err != nil {
		return nil, err
	}
	rules, err := querySQLiteRules(ctx, tx,
		`SELECT id, monitor_id, type, params, enabled FROM rules WHERE monitor_id = ? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	names, err := queryStringColumn(ctx, tx, `SELECT name FROM monitors`)
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
	}
	var created string
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO monitors (name, contract_ids, enabled, priority) VALUES (?, ?, ?, ?) RETURNING id, created_at`,
		dup.Name, string(ids), boolToInt(dup.Enabled), string(dup.Priority),
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
	if err := r.Scan(&m.ID, &m.Name, &ids, &enabled, &created, &lastMatched, &priority); err != nil {
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

func (s *SQLite) CreateRule(ctx context.Context, r *Rule) error {
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

	for _, r := range rules {
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
		`SELECT id, monitor_id, type, params, enabled FROM rules WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	if len(rules) == 0 {
		return nil, ErrNotFound
	}
	return &rules[0], nil
}

func (s *SQLite) ListRules(ctx context.Context, monitorID int64, enabledOnly bool) ([]Rule, error) {
	q := `SELECT id, monitor_id, type, params, enabled FROM rules WHERE monitor_id = ?`
	if enabledOnly {
		q += ` AND enabled = 1`
	}
	q += ` ORDER BY id`
	return querySQLiteRules(ctx, s.db, q, monitorID)
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
		`UPDATE rules SET type = ?, params = ?, enabled = ? WHERE id = ?`,
		r.Type, string(jsonOrEmpty(r.Params)), boolToInt(r.Enabled), r.ID)
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

func (s *SQLite) DeleteRule(ctx context.Context, id int64) error {
	return s.deleteByID(ctx, "rules", id)
}

// --- channels ---

func (s *SQLite) CreateChannel(ctx context.Context, c *Channel) error {
	config, err := configForWrite(s.cipher, c.ID, c.Name, c.Config)
	if err != nil {
		return err
	}
	var created string
	if err := s.db.QueryRowContext(ctx,
		`INSERT INTO channels (name, type, config, enabled, digest_mode, digest_window_seconds)
		 VALUES (?, ?, ?, ?, ?, ?)
		 RETURNING id, created_at`,
		c.Name, c.Type, string(config), boolToInt(c.Enabled), c.DigestMode, c.DigestWindowSeconds,
	).Scan(&c.ID, &created); err != nil {
		return mapSQLiteErr(err)
	}
	c.CreatedAt, err = parseSQLiteTime(created)
	return err
}

func (s *SQLite) GetChannel(ctx context.Context, id int64) (*Channel, error) {
	channels, err := s.queryChannels(ctx,
		`SELECT id, name, type, config, enabled, created_at, digest_mode, digest_window_seconds FROM channels WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	if len(channels) == 0 {
		return nil, ErrNotFound
	}
	return &channels[0], nil
}

func (s *SQLite) ListChannels(ctx context.Context, enabledOnly bool) ([]Channel, error) {
	q := `SELECT id, name, type, config, enabled, created_at, digest_mode, digest_window_seconds FROM channels`
	if enabledOnly {
		q += ` WHERE enabled = 1`
	}
	q += ` ORDER BY id`
	return s.queryChannels(ctx, q)
}

func (s *SQLite) ListChannelsPage(ctx context.Context, f ListFilter) ([]Channel, error) {
	q := `SELECT id, name, type, config, enabled, created_at, digest_mode, digest_window_seconds FROM channels WHERE 1 = 1`
	args := []any{}
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

func (s *SQLite) ListChannelsForMonitor(ctx context.Context, monitorID int64) ([]Channel, error) {
	return s.queryChannels(ctx,
		`SELECT c.id, c.name, c.type, c.config, c.enabled, c.created_at, c.digest_mode, c.digest_window_seconds
		 FROM channels c
		 JOIN monitor_channels mc ON mc.channel_id = c.id
		 WHERE mc.monitor_id = ? AND c.enabled = 1
		 ORDER BY c.id`, monitorID)
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
	if err := r.Scan(&c.ID, &c.Name, &c.Type, &config, &enabled, &created, &c.DigestMode, &c.DigestWindowSeconds); err != nil {
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
		`UPDATE channels SET name = ?, type = ?, config = ?, enabled = ?, digest_mode = ?, digest_window_seconds = ? WHERE id = ?`,
		c.Name, c.Type, string(config), boolToInt(c.Enabled), c.DigestMode, c.DigestWindowSeconds, c.ID)
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

func (s *SQLite) ListMonitorsForChannel(ctx context.Context, channelID int64) ([]Monitor, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.id, m.name, m.contract_ids, m.enabled, m.created_at, m.last_matched_at, m.priority
		 FROM monitors m
		 JOIN monitor_channels mc ON mc.monitor_id = m.id
		 WHERE mc.channel_id = ?
		 ORDER BY m.id`, channelID)
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
		m.ChannelIDs, err = s.monitorChannelIDs(ctx, m.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
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
		`INSERT INTO alerts (monitor_id, rule_id, event_id, payload, ledger) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (rule_id, event_id) DO NOTHING
		 RETURNING id, created_at`,
		a.MonitorID, a.RuleID, a.EventID, string(jsonOrEmpty(a.Payload)), int64(a.Ledger),
	).Scan(&id, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return AlertDuplicate, nil // duplicate (rule_id, event_id): deduped
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
		`SELECT id, monitor_id, rule_id, event_id, payload, created_at, ledger, retracted_at FROM alerts WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *SQLite) ListAlerts(ctx context.Context, f AlertFilter) ([]Alert, error) {
	q := `SELECT id, monitor_id, rule_id, event_id, payload, created_at, ledger, retracted_at FROM alerts WHERE 1 = 1`
	args := []any{}
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
		// would skip or repeat rows once two alerts share a timestamp.
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
	if err := r.Scan(&a.ID, &a.MonitorID, &a.RuleID, &a.EventID, &payload, &created, &ledger, &retracted); err != nil {
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

func (s *SQLite) RecordDeliveryAttempt(ctx context.Context, d *DeliveryAttempt) error {
	var attempted string
	if err := s.db.QueryRowContext(ctx,
		`INSERT INTO delivery_attempts (alert_id, channel_id, status, response_snippet)
		 VALUES (?, ?, ?, ?) RETURNING id, attempted_at`,
		d.AlertID, d.ChannelID, d.Status, d.ResponseSnippet,
	).Scan(&d.ID, &attempted); err != nil {
		return mapSQLiteErr(err)
	}
	t, err := parseSQLiteTime(attempted)
	if err != nil {
		return err
	}
	d.AttemptedAt = t
	return nil
}

func (s *SQLite) ListDeliveryAttempts(ctx context.Context, alertID int64, status string) ([]DeliveryAttempt, error) {
	q := `SELECT id, alert_id, channel_id, status, response_snippet, attempted_at
		 FROM delivery_attempts WHERE alert_id = ?`
	args := []any{alertID}
	if status != "" {
		// Applied in SQL so a busy alert does not ship every attempt just so
		// the client can throw most of them away.
		q += ` AND status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY id`
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
		`SELECT id, monitor_id, rule_id, event_id, payload, created_at, ledger, retracted_at
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

func (s *SQLite) RecordLedgerHashes(ctx context.Context, hashes []LedgerHash) error {
	if len(hashes) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // rollback after commit is a no-op
	for _, h := range hashes {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ledger_hashes (ledger, hash) VALUES (?, ?)
			 ON CONFLICT (ledger) DO UPDATE SET hash = excluded.hash,
			                                   observed_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
			 WHERE ledger_hashes.hash IS NOT excluded.hash`,
			int64(h.Ledger), h.Hash); err != nil {
			return mapSQLiteErr(err)
		}
	}
	return tx.Commit()
}

func (s *SQLite) LedgerHashes(ctx context.Context, from, to uint32) ([]LedgerHash, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ledger, hash FROM ledger_hashes WHERE ledger >= ? AND ledger <= ? ORDER BY ledger`,
		int64(from), int64(to))
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

func (s *SQLite) PruneLedgerHashes(ctx context.Context, before uint32) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM ledger_hashes WHERE ledger < ?`, int64(before))
	return err
}

func (s *SQLite) RetractAlertsFromLedger(ctx context.Context, ledger uint32, at time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE alerts SET retracted_at = ? WHERE ledger >= ? AND retracted_at IS NULL`,
		sqliteTimeString(at), int64(ledger))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- ingest state ---

func (s *SQLite) GetIngestState(ctx context.Context) (IngestState, error) {
	var st IngestState
	var lastLedger int64
	var updated string
	if err := s.db.QueryRowContext(ctx,
		`SELECT last_ledger, last_cursor, updated_at FROM ingest_state WHERE id = 1`,
	).Scan(&lastLedger, &st.LastCursor, &updated); err != nil {
		return st, mapSQLiteErr(err)
	}
	st.LastLedger = uint32(lastLedger)
	var err error
	if st.UpdatedAt, err = parseSQLiteTime(updated); err != nil {
		return st, err
	}
	return st, nil
}

func (s *SQLite) SetIngestState(ctx context.Context, st IngestState) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE ingest_state SET last_ledger = ?, last_cursor = ?, updated_at = ? WHERE id = 1`,
		int64(st.LastLedger), st.LastCursor, sqliteTimeString(time.Now()))
	return err
}

// --- digest queue ---

func (s *SQLite) PushDigestAlert(ctx context.Context, channelID int64, payload json.RawMessage) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pending_digests (channel_id, payload) VALUES (?, ?)`, channelID, string(payload))
	return mapSQLiteErr(err)
}

func (s *SQLite) ListDigestAlerts(ctx context.Context, channelID int64) ([]DigestAlert, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, channel_id, payload, created_at FROM pending_digests WHERE channel_id = ? ORDER BY id`, channelID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DigestAlert
	for rows.Next() {
		var d DigestAlert
		var payload, created string
		if err := rows.Scan(&d.ID, &d.ChannelID, &payload, &created); err != nil {
			return nil, err
		}
		d.Payload = json.RawMessage(payload)
		if d.CreatedAt, err = parseSQLiteTime(created); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *SQLite) DeleteDigestAlerts(ctx context.Context, channelID int64, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+1)
	args = append(args, channelID)
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM pending_digests WHERE channel_id = ? AND id IN (`+placeholders+`)`, args...)
	return mapSQLiteErr(err)
}

// --- audit log ---

func (s *SQLite) CreateAuditEntry(ctx context.Context, e *AuditEntry) error {
	diff := e.Diff
	if len(diff) == 0 {
		diff = json.RawMessage(`{}`)
	}
	var created string
	if err := s.db.QueryRowContext(ctx,
		`INSERT INTO audit_log (actor, action, target_type, target_id, diff) VALUES (?, ?, ?, ?, ?) RETURNING id, created_at`,
		e.Actor, e.Action, e.TargetType, e.TargetID, string(diff)).Scan(&e.ID, &created); err != nil {
		return mapSQLiteErr(err)
	}
	var err error
	e.CreatedAt, err = parseSQLiteTime(created)
	return err
}

func (s *SQLite) ListAuditEntries(ctx context.Context, f AuditFilter) ([]AuditEntry, error) {
	where := []string{"1 = 1"}
	args := []any{}
	if f.TargetType != "" {
		where = append(where, "target_type = ?")
		args = append(args, f.TargetType)
	}
	if f.TargetID != 0 {
		where = append(where, "target_id = ?")
		args = append(args, f.TargetID)
	}
	if !f.From.IsZero() {
		where = append(where, "created_at >= ?")
		args = append(args, sqliteTimeString(f.From))
	}
	if !f.To.IsZero() {
		where = append(where, "created_at <= ?")
		args = append(args, sqliteTimeString(f.To))
	}
	args = append(args, clampAuditLimit(f.Limit))
	q := `SELECT id, actor, action, target_type, target_id, diff, created_at FROM audit_log WHERE ` +
		strings.Join(where, " AND ") + ` ORDER BY id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var diff, created string
		if err := rows.Scan(&e.ID, &e.Actor, &e.Action, &e.TargetType, &e.TargetID, &diff, &created); err != nil {
			return nil, err
		}
		e.Diff = json.RawMessage(diff)
		if e.CreatedAt, err = parseSQLiteTime(created); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- backfills ---

func (s *SQLite) GetBackfill(ctx context.Context, monitorID int64) (Backfill, error) {
	var b Backfill
	var fromLedger, toLedger, nextLedger int64
	var updated string
	err := s.db.QueryRowContext(ctx,
		`SELECT monitor_id, from_ledger, to_ledger, next_ledger, cursor, deliver, complete, updated_at
		   FROM backfills WHERE monitor_id = ?`, monitorID,
	).Scan(&b.MonitorID, &fromLedger, &toLedger, &nextLedger, &b.Cursor, &b.Deliver, &b.Complete, &updated)
	if err != nil {
		return b, mapSQLiteErr(err)
	}
	b.FromLedger = uint32(fromLedger)
	b.ToLedger = uint32(toLedger)
	b.NextLedger = uint32(nextLedger)
	if b.UpdatedAt, err = parseSQLiteTime(updated); err != nil {
		return b, err
	}
	return b, nil
}

// UpsertBackfill writes the run's resume point, replacing any previous row for
// the monitor. One row per monitor is what makes "resume where it stopped"
// unambiguous.
func (s *SQLite) UpsertBackfill(ctx context.Context, b *Backfill) error {
	var updated string
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO backfills (monitor_id, from_ledger, to_ledger, next_ledger, cursor, deliver, complete, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (monitor_id) DO UPDATE SET
		     from_ledger = excluded.from_ledger,
		     to_ledger   = excluded.to_ledger,
		     next_ledger = excluded.next_ledger,
		     cursor      = excluded.cursor,
		     deliver     = excluded.deliver,
		     complete    = excluded.complete,
		     updated_at  = excluded.updated_at
		 RETURNING updated_at`,
		b.MonitorID, int64(b.FromLedger), int64(b.ToLedger), int64(b.NextLedger),
		b.Cursor, boolToInt(b.Deliver), boolToInt(b.Complete), sqliteTimeString(time.Now()),
	).Scan(&updated)
	if err != nil {
		return mapSQLiteErr(err)
	}
	if b.UpdatedAt, err = parseSQLiteTime(updated); err != nil {
		return err
	}
	return nil
}

// --- stats ---

func (s *SQLite) GetStats(ctx context.Context) (Stats, error) {
	var st Stats
	var updated string
	// The 24h window is computed in Go and compared as text; the fixed-width
	// timestamp format makes that a valid chronological comparison.
	cutoff := sqliteTimeString(time.Now().UTC().Add(-24 * time.Hour))
	err := s.db.QueryRowContext(ctx, `
		SELECT
			(SELECT count(*) FROM monitors),
			(SELECT count(*) FROM rules),
			(SELECT count(*) FROM channels),
			(SELECT count(*) FROM alerts),
			(SELECT count(*) FROM alerts WHERE created_at > ?),
			(SELECT last_ledger FROM ingest_state WHERE id = 1),
			(SELECT updated_at FROM ingest_state WHERE id = 1)`, cutoff).
		Scan(&st.Monitors, &st.Rules, &st.Channels, &st.Alerts, &st.AlertsLast24, &st.LastLedger, &updated)
	if err != nil {
		return st, err
	}
	if st.LastPollAt, err = parseSQLiteTime(updated); err != nil {
		return st, err
	}
	return st, nil
}

// AlertCountsByDay returns `days` consecutive UTC calendar days ending today,
// zero-filled. Bucketing runs in SQL: substr(created_at, 1, 10) is the UTC day
// of the fixed-format timestamp, and a recursive CTE generates the window so a
// quiet day is an explicit 0 rather than a missing bar. SQLite has no
// generate_series, hence the CTE.
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
		GROUP BY window.day
		ORDER BY window.day`, fmt.Sprintf("-%d days", days-1))
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
	if ss.IsDefault {
		if _, err := s.db.ExecContext(ctx, `UPDATE saved_searches SET is_default = 0 WHERE is_default = 1`); err != nil {
			return err
		}
	}
	var created string
	if err := s.db.QueryRowContext(ctx,
		`INSERT INTO saved_searches (name, filter, is_default) VALUES (?, ?, ?) RETURNING id, created_at`,
		ss.Name, string(filter), boolToInt(ss.IsDefault)).Scan(&ss.ID, &created); err != nil {
		return mapSQLiteErr(err)
	}
	ss.CreatedAt, err = parseSQLiteTime(created)
	return err
}

func (s *SQLite) ListSavedSearches(ctx context.Context) ([]SavedSearch, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, filter, is_default, created_at FROM saved_searches ORDER BY name`)
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
		`SELECT id, name, filter, is_default, created_at FROM saved_searches WHERE id = ?`, id))
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

	if _, err := tx.ExecContext(ctx, `UPDATE saved_searches SET is_default = 0 WHERE is_default = 1`); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE saved_searches SET is_default = 1 WHERE id = ?`, id)
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
	res, err := s.db.ExecContext(ctx, `UPDATE saved_searches SET is_default = 0 WHERE id = ?`, id)
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
		`INSERT INTO monitor_templates (name, description, rules, channel_ids, parameters) VALUES (?, ?, ?, ?, ?) RETURNING id, created_at`,
		t.Name, t.Description, string(rulesJSON), string(channelJSON), string(paramsJSON)).Scan(&t.ID, &created); err != nil {
		return mapSQLiteErr(err)
	}
	var err error
	t.CreatedAt, err = parseSQLiteTime(created)
	return err
}

func (s *SQLite) GetMonitorTemplate(ctx context.Context, id int64) (*MonitorTemplate, error) {
	t, err := scanSQLiteTemplate(s.db.QueryRowContext(ctx,
		`SELECT id, name, description, rules, channel_ids, parameters, created_at FROM monitor_templates WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *SQLite) ListMonitorTemplates(ctx context.Context) ([]MonitorTemplate, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, description, rules, channel_ids, parameters, created_at FROM monitor_templates ORDER BY name`)
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
		`UPDATE monitor_templates SET name = ?, description = ?, rules = ?, channel_ids = ?, parameters = ? WHERE id = ?`,
		t.Name, t.Description, string(rulesJSON), string(channelJSON), string(paramsJSON), t.ID)
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
