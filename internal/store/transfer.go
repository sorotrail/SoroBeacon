// This file implements the row copy behind `sorobeacon store migrate`: moving
// every table both backends share from one database to another with the IDs
// preserved, so every foreign key, saved filter and template channel list
// stays valid without remapping.
//
// The design keeps the command backend-agnostic. The table plan, the batching
// loop and the restart logic here talk to a narrow per-backend seam
// (transferNode) rather than raw SQL against both databases; each backend
// knows its own placeholder syntax, its own column types and its own way to
// advance an autoincrement counter. Two details are inherently backend
// specific and live behind small hooks:
//
//   - Postgres range-partitions alerts by created_at. Before alerts are
//     copied, monthly partitions are ensured for the copied range so rows do
//     not pile up in the DEFAULT partition.
//   - Postgres delivery_attempts carries alert_created_at (a NOT NULL column
//     supporting the composite foreign key to the partitioned alerts table)
//     which SQLite's schema does not have. The Postgres side drops the
//     constraint, copies, backfills the column from the copied alerts and
//     restores the constraint — the same dance the 0011 migration performs.
//
// Every insert runs with ON CONFLICT DO NOTHING, so a run interrupted
// mid-table can be restarted with --force: already-copied rows are skipped
// and the copy continues where it stopped. Reads are keyset-paginated in the
// table's key order, so restarts and large tables never degrade into OFFSET
// scans.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// transferTable names one shared table and the columns its primary key is
// made of. The key columns drive keyset pagination and keep the copy stable
// across restarts.
type transferTable struct {
	name string
	key  []string
}

// transferTables lists every table both backends share, in dependency order:
// each table appears after every table it references by foreign key, so rows
// can be inserted in this order without deferring constraints.
//
// Deliberately absent:
//   - schema_migrations: the target runs its own migrations and must record
//     its own version.
//   - The Postgres alerts partitions (alerts_YYYY_MM, alerts_default):
//     derived containers, not data; EnsureAlertPartitions creates the ones
//     the copied range needs.
//   - SQLite internals (sqlite_*): never part of a copy.
//
// TestTransferTablesMatchSchema fails when a migration adds a table without
// adding it here, so this list cannot silently fall behind the schema.
var transferTables = []transferTable{
	{name: "workspaces", key: []string{"id"}},
	{name: "monitors", key: []string{"id"}},
	{name: "channels", key: []string{"id"}},
	{name: "monitor_channels", key: []string{"monitor_id", "channel_id"}},
	{name: "rules", key: []string{"id"}},
	{name: "saved_searches", key: []string{"id"}},
	{name: "monitor_templates", key: []string{"id"}},
	{name: "ledger_hashes", key: []string{"ledger"}},
	{name: "ingest_state", key: []string{"id"}},
	{name: "network_ingest_state", key: []string{"network"}},
	{name: "network_ledger_hashes", key: []string{"network", "ledger"}},
	{name: "contract_ingest_cursors", key: []string{"network", "contract_id"}},
	{name: "backfills", key: []string{"monitor_id"}},
	{name: "maintenance_windows", key: []string{"id"}},
	{name: "rule_absence_state", key: []string{"rule_id", "event_name"}},
	{name: "alert_inhibitions", key: []string{"source_rule_id", "target_rule_id"}},
	{name: "escalation_policies", key: []string{"id"}},
	{name: "escalation_steps", key: []string{"id"}},
	{name: "escalation_step_channels", key: []string{"step_id", "channel_id"}},
	{name: "alerts", key: []string{"id"}},
	{name: "alert_dedup", key: []string{"rule_id", "event_id"}}, {name: "delivery_attempts", key: []string{"id"}},
	{name: "alert_escalations", key: []string{"alert_id"}},
	{name: "dead_letters", key: []string{"id"}},
	{name: "pending_digests", key: []string{"id"}},
	{name: "audit_log", key: []string{"id"}},
	{name: "api_tokens", key: []string{"id"}},
}

// transferDerived marks plan tables that only one backend physically has,
// because the same guard exists in both through different means. alert_dedup
// is Postgres-only: SQLite enforces the identical rule with a unique index
// on alerts (rule_id, event_id). A copy in either direction therefore either
// rebuilds the table on the target from the copied alerts (sqlite ->
// postgres) or skips it (postgres -> sqlite), and never treats its absence as
// a schema error.
var transferDerived = map[string]bool{
	"alert_dedup": true,
}

// TransferOptions tunes a copy. The zero value copies in batches of 500 rows
// without progress reporting.
type TransferOptions struct {
	// BatchSize is the maximum number of rows read and written per
	// statement. It is clamped per backend to the driver's parameter limit.
	BatchSize int
	// Progress, when non-nil, is called after every written batch with the
	// table, the rows inserted so far for this run and the source's total
	// row count. An interrupted run's Progress can cancel the context to
	// stop the copy; rows already written stay written.
	Progress func(table string, copied, total int64)
}

// TableTransferReport is one table's outcome in a TransferReport. Inserted
// counts only this run's writes: on a restarted run the rows that were
// already present are skipped and show up in Skipped.
type TableTransferReport struct {
	Table    string
	Source   int64
	Inserted int64
	Skipped  int64
}

// TransferReport is the per-table outcome of TransferData, in plan order.
type TransferReport []TableTransferReport

// TotalInserted sums the rows this run wrote.
func (r TransferReport) TotalInserted() int64 {
	var n int64
	for _, t := range r {
		n += t.Inserted
	}
	return n
}

// TableCount is one table's row count as reported by TransferCounts.
type TableCount struct {
	Table string
	Rows  int64
}

// TableCounts is a per-table row-count report, in plan order.
type TableCounts []TableCount

// Total sums the counted rows.
func (c TableCounts) Total() int64 {
	var n int64
	for _, t := range c {
		n += t.Rows
	}
	return n
}

// transferColumn describes one destination column as the destination backend
// sees it: its name, its type text (used by the Postgres side to coerce
// values; SQLite coerces without types) and whether inserting a row that
// omits the column is possible at all.
type transferColumn struct {
	name       string
	typ        string
	notNull    bool
	hasDefault bool
}

// transferNode is the narrow per-backend seam the copy runs through. Both
// *SQLite and *Postgres implement it; nothing else in the store package has
// to change when the plan grows.
type transferNode interface {
	// transferColumns lists a table's columns in destination order. A nil
	// slice with no error means the table does not exist on this backend.
	transferColumns(ctx context.Context, table string) ([]transferColumn, error)
	// transferCount returns a table's row count.
	transferCount(ctx context.Context, table string) (int64, error)
	// transferRead returns up to limit rows whose key tuple sorts after
	// after (exclusive), in ascending key order. A nil after starts at the
	// first row. Returned values are the backend's native driver values.
	transferRead(ctx context.Context, table string, columns, key []string, after []any, limit int) ([][]any, error)
	// transferInsert writes one batch with ON CONFLICT DO NOTHING and
	// returns how many rows it actually inserted.
	transferInsert(ctx context.Context, table string, columns []transferColumn, rows [][]any) (int64, error)
	// transferMaxParams is the driver's safe parameter budget, used to
	// clamp the batch size to one statement.
	transferMaxParams() int
	// transferAdvanceSequences pushes every autoincrement/serial counter
	// past the copied IDs, so the next row created on the target cannot
	// collide with a copied one.
	transferAdvanceSequences(ctx context.Context, tables []string) error
	// transferAlertTimeRange reports the copied alerts' created_at span so
	// partitions can be ensured. ok is false when there are no alerts.
	transferAlertTimeRange(ctx context.Context) (from, to time.Time, ok bool, err error)
	// transferDeriveTable rebuilds a transferDerived table on this backend
	// from data that has already been copied. It runs only for tables this
	// backend has and the source does not.
	transferDeriveTable(ctx context.Context, table string) error
}

// transferColumnGapFiller is implemented by a backend whose schema has a NOT
// NULL column without a default that the other backend's schema lacks. The
// only such column today is delivery_attempts.alert_created_at on Postgres;
// SQLite never needs a filler.
type transferColumnGapFiller interface {
	// transferFillColumnGapPre runs before the table is copied.
	transferFillColumnGapPre(ctx context.Context, table string, gaps []transferColumn) error
	// transferFillColumnGapPost runs after the table is copied and must
	// leave the column's constraints in place again.
	transferFillColumnGapPost(ctx context.Context, table string, gaps []transferColumn) error
}

// transferNodeOf narrows a Store to the copy seam. Both shipped backends
// satisfy it; an embedder with a custom Store cannot be copied and gets an
// explicit error rather than a partial migration.
func transferNodeOf(st Store) (transferNode, error) {
	switch t := st.(type) {
	case *SQLite:
		return t, nil
	case *Postgres:
		return t, nil
	default:
		return nil, fmt.Errorf("store of type %T cannot be copied by store migrate (only the sqlite and postgres backends can)", st)
	}
}

// TransferCounts returns the source plan tables' row counts, in plan order.
// The CLI uses it both for the dry run's report and for the non-empty-target
// guard, so the two always agree on what "contains data" means.
func TransferCounts(ctx context.Context, st Store) (TableCounts, error) {
	node, err := transferNodeOf(st)
	if err != nil {
		return nil, err
	}
	out := make(TableCounts, 0, len(transferTables))
	for _, t := range transferTables {
		cols, err := node.transferColumns(ctx, t.name)
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", t.name, err)
		}
		if cols == nil {
			if !transferDerived[t.name] {
				return nil, fmt.Errorf("table %s does not exist; run migrations first", t.name)
			}
			out = append(out, TableCount{Table: t.name})
			continue
		}
		n, err := node.transferCount(ctx, t.name)
		if err != nil {
			return nil, fmt.Errorf("count %s: %w", t.name, err)
		}
		out = append(out, TableCount{Table: t.name, Rows: n})
	}
	return out, nil
}

// TransferData copies every shared table from src to dst in dependency
// order, preserving IDs. It does not check whether dst is empty — that guard
// (and --force) belongs to the CLI, which reports it as a usage error — and
// it does not move schema: both databases must already be migrated to the
// same version, which the CLI verifies before calling.
func TransferData(ctx context.Context, src, dst Store, opts TransferOptions) (TransferReport, error) {
	srcNode, err := transferNodeOf(src)
	if err != nil {
		return nil, err
	}
	dstNode, err := transferNodeOf(dst)
	if err != nil {
		return nil, err
	}

	batch := opts.BatchSize
	if batch <= 0 {
		batch = 500
	}

	// Postgres keeps alerts in monthly ranges; make sure the copied range
	// has partitions before the first alert row arrives, so none of them
	// lands in the DEFAULT partition.
	if err := ensureTransferAlertPartitions(ctx, srcNode, dst); err != nil {
		return nil, err
	}

	report := make(TransferReport, 0, len(transferTables))
	for _, t := range transferTables {
		rep, err := transferOneTable(ctx, srcNode, dstNode, t, batch, opts.Progress)
		if err != nil {
			return report, fmt.Errorf("copy %s: %w", t.name, err)
		}
		report = append(report, rep)
	}

	if err := dstNode.transferAdvanceSequences(ctx, transferTableNames()); err != nil {
		return report, fmt.Errorf("advance target sequences: %w", err)
	}
	return report, nil
}

func transferTableNames() []string {
	names := make([]string, len(transferTables))
	for i, t := range transferTables {
		names[i] = t.name
	}
	return names
}

// ensureTransferAlertPartitions creates the monthly Postgres partitions that
// cover the source's alerts. A target without partitioning (SQLite) and an
// empty source are both no-ops.
func ensureTransferAlertPartitions(ctx context.Context, src transferNode, dst Store) error {
	ensurer, ok := dst.(PartitionEnsurer)
	if !ok {
		return nil
	}
	from, to, ok, err := src.transferAlertTimeRange(ctx)
	if !ok || err != nil {
		return err
	}
	months := (to.Year()-from.Year())*12 + int(to.Month()-from.Month()) + 1
	return ensurer.EnsureAlertPartitions(ctx, monthStart(from), months)
}

// transferOneTable copies a single table and reports what happened.
func transferOneTable(ctx context.Context, src, dst transferNode, t transferTable, batch int, progress func(string, int64, int64)) (rep TableTransferReport, err error) {
	rep = TableTransferReport{Table: t.name}

	dstCols, err := dst.transferColumns(ctx, t.name)
	if err != nil {
		return rep, err
	}
	srcCols, err := src.transferColumns(ctx, t.name)
	if err != nil {
		return rep, fmt.Errorf("source schema: %w", err)
	}

	// One-sided tables are the transferDerived contract; anything else one
	// side is missing is a schema problem the operator must fix first.
	if srcCols == nil || dstCols == nil {
		switch {
		case srcCols == nil && dstCols == nil:
			if transferDerived[t.name] {
				return rep, nil
			}
			return rep, fmt.Errorf("neither database has table %s; this binary's schema and the copy plan disagree, run migrations on both sides", t.name)
		case srcCols == nil:
			if !transferDerived[t.name] {
				return rep, fmt.Errorf("source database has no table %s; it is older than this binary, run the matching sorobeacon version against it once to upgrade", t.name)
			}
			if err := dst.transferDeriveTable(ctx, t.name); err != nil {
				return rep, fmt.Errorf("derive %s on the target: %w", t.name, err)
			}
			n, err := dst.transferCount(ctx, t.name)
			if err != nil {
				return rep, err
			}
			rep.Source, rep.Inserted = n, n
			return rep, nil
		default: // dstCols == nil
			if !transferDerived[t.name] {
				return rep, fmt.Errorf("target database has no table %s; run migrations first", t.name)
			}
			// The source's copy of a derived table is recoverable from the
			// rows that were copied, and the target enforces the same guard
			// natively (SQLite: a unique index on alerts).
			return rep, nil
		}
	}

	srcNames := make(map[string]bool, len(srcCols))
	for _, c := range srcCols {
		srcNames[c.name] = true
	}
	for _, k := range t.key {
		if !srcNames[k] {
			return rep, fmt.Errorf("source table %s has no key column %s; the source schema is older than this binary, run the matching sorobeacon version against it once to upgrade", t.name, k)
		}
	}

	// Columns the source lacks are filled by their destination default when
	// one exists; anything else needs the backend's gap filler (see
	// transferColumnGapFiller).
	var gaps []transferColumn
	cols := make([]transferColumn, 0, len(dstCols))
	for _, c := range dstCols {
		if !srcNames[c.name] {
			if c.notNull && !c.hasDefault {
				gaps = append(gaps, c)
			}
			continue
		}
		cols = append(cols, c)
	}
	var filler transferColumnGapFiller
	if len(gaps) > 0 {
		f, ok := dst.(transferColumnGapFiller)
		if !ok {
			names := make([]string, len(gaps))
			for i, g := range gaps {
				names[i] = g.name
			}
			return rep, fmt.Errorf("target requires column %s.%s, which the source schema does not have and this target cannot fill", t.name, strings.Join(names, ", "))
		}
		filler = f
		if err := filler.transferFillColumnGapPre(ctx, t.name, gaps); err != nil {
			return rep, fmt.Errorf("prepare %s: %w", t.name, err)
		}
		defer func() {
			if postErr := filler.transferFillColumnGapPost(ctx, t.name, gaps); postErr != nil && err == nil {
				err = fmt.Errorf("finish %s: %w", t.name, postErr)
			}
		}()
	}

	if rep.Source, err = src.transferCount(ctx, t.name); err != nil {
		return rep, err
	}
	if rep.Source == 0 {
		return rep, nil
	}

	keyIdx := make([]int, len(t.key))
	for i, k := range t.key {
		keyIdx[i] = indexOfColumn(cols, k)
	}

	rowsPerBatch := batch
	if maxParams := dst.transferMaxParams() / len(cols); maxParams < rowsPerBatch {
		rowsPerBatch = maxParams
	}
	if rowsPerBatch < 1 {
		rowsPerBatch = 1
	}

	var after []any
	for {
		rows, err := src.transferRead(ctx, t.name, columnNames(cols), t.key, after, rowsPerBatch)
		if err != nil {
			return rep, err
		}
		if len(rows) == 0 {
			break
		}
		inserted, err := dst.transferInsert(ctx, t.name, cols, rows)
		if err != nil {
			return rep, err
		}
		rep.Inserted += inserted
		last := rows[len(rows)-1]
		after = make([]any, len(keyIdx))
		for i, idx := range keyIdx {
			after[i] = last[idx]
		}
		if progress != nil {
			progress(t.name, rep.Inserted, rep.Source)
		}
		if len(rows) < rowsPerBatch {
			break
		}
	}
	rep.Skipped = rep.Source - rep.Inserted
	return rep, nil
}

func columnNames(cols []transferColumn) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.name
	}
	return out
}

func indexOfColumn(cols []transferColumn, name string) int {
	for i, c := range cols {
		if c.name == name {
			return i
		}
	}
	return -1
}

// transferKeysetWhere builds the keyset predicate and its ORDER BY for the
// first page (no lower bound) or a later page. Both backends support
// row-value comparison, which keeps composite keys to one clause.
func transferKeysetWhere(style, table string, columns, key []string, hasAfter bool, limitArg string) string {
	var sb strings.Builder
	sb.WriteString("SELECT ")
	sb.WriteString(strings.Join(transferQuoteAll(columns), ", "))
	sb.WriteString(" FROM ")
	sb.WriteString(transferQuoteIdent(table))
	if hasAfter {
		sb.WriteString(" WHERE (")
		sb.WriteString(strings.Join(transferQuoteAll(key), ", "))
		sb.WriteString(") > (")
		sb.WriteString(transferPlaceholders(style, len(key), 1, 0))
		sb.WriteByte(')')
	}
	sb.WriteString(" ORDER BY ")
	sb.WriteString(transferOrderList(key))
	sb.WriteString(" LIMIT ")
	sb.WriteString(limitArg)
	return sb.String()
}

func transferOrderList(key []string) string {
	return strings.Join(transferQuoteAll(key), ", ") + " ASC"
}

func transferQuoteAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = transferQuoteIdent(n)
	}
	return out
}

func transferQuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// transferPlaceholders renders a values tuple list for a backend: "?, ?, ?"
// for SQLite, "$1, $2, $3" for Postgres. offset advances across batches of
// tuples so one statement can hold many rows.
func transferPlaceholders(style string, count, rows, offset int) string {
	var sb strings.Builder
	for r := 0; r < rows; r++ {
		if r > 0 {
			sb.WriteByte(',')
		}
		sb.WriteByte('(')
		for i := 0; i < count; i++ {
			if i > 0 {
				sb.WriteByte(',')
			}
			switch style {
			case "pg":
				sb.WriteByte('$')
				sb.WriteString(strconv.Itoa(offset + r*count + i + 1))
			default:
				sb.WriteByte('?')
			}
		}
		sb.WriteByte(')')
	}
	return sb.String()
}

// transferTimeLayouts are the timestamp formats a source may hand over: the
// SQLite on-disk layout, RFC3339 with and without fractional seconds, and the
// classic Postgres text form.
var transferTimeLayouts = []string{
	time.RFC3339Nano,
	sqliteTimeLayout,
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05",
}

func parseTransferTime(v []byte) (time.Time, error) {
	s := string(v)
	for _, layout := range transferTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse %q as a timestamp", s)
}

// ---------------------------------------------------------------------------
// SQLite implementation.
// ---------------------------------------------------------------------------

// transferColumns implements transferNode for SQLite via PRAGMA table_info.
func (s *SQLite) transferColumns(ctx context.Context, table string) ([]transferColumn, error) {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info("+transferQuoteIdent(table)+")")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []transferColumn
	for rows.Next() {
		var (
			cid       int64
			c         transferColumn
			dfltValue sql.NullString
		)
		if err := rows.Scan(&cid, &c.name, &c.typ, &c.notNull, &dfltValue, new(any)); err != nil {
			return nil, err
		}
		c.hasDefault = dfltValue.Valid
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// PRAGMA table_info on a table that does not exist returns no rows,
	// not an error: nil means "no such table".
	return out, nil
}

func (s *SQLite) transferCount(ctx context.Context, table string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+transferQuoteIdent(table)).Scan(&n)
	return n, err
}

func (s *SQLite) transferRead(ctx context.Context, table string, columns, key []string, after []any, limit int) ([][]any, error) {
	query := transferKeysetWhere("sqlite", table, columns, key, len(after) > 0, "?")
	args := append(append([]any{}, after...), limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out [][]any
	for rows.Next() {
		vals := make([]any, len(columns))
		ptrs := make([]any, len(columns))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		out = append(out, vals)
	}
	return out, rows.Err()
}

func (s *SQLite) transferInsert(ctx context.Context, table string, columns []transferColumn, rows [][]any) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	var sb strings.Builder
	sb.WriteString("INSERT INTO ")
	sb.WriteString(transferQuoteIdent(table))
	sb.WriteString(" (")
	sb.WriteString(strings.Join(transferQuoteAll(columnNames(columns)), ", "))
	sb.WriteString(") VALUES ")
	sb.WriteString(transferPlaceholders("sqlite", len(columns), len(rows), 0))
	// DO NOTHING is what makes an interrupted run restartable: rerunning
	// with --force skips the rows that already landed.
	sb.WriteString(" ON CONFLICT DO NOTHING")

	args := make([]any, 0, len(rows)*len(columns))
	for _, row := range rows {
		for _, v := range row {
			coerced, err := coerceForSQLite(v)
			if err != nil {
				return 0, fmt.Errorf("copy %s: %w", table, err)
			}
			args = append(args, coerced)
		}
	}
	res, err := s.db.ExecContext(ctx, sb.String(), args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (s *SQLite) transferMaxParams() int { return 20000 }

func (s *SQLite) transferAdvanceSequences(ctx context.Context, tables []string) error {
	// AUTOINCREMENT tables keep their high-water mark in sqlite_sequence,
	// and SQLite already bumps it for explicit-id inserts. The update is a
	// belt-and-braces pass for rows skipped by ON CONFLICT and for the
	// reverse direction (Postgres -> SQLite), where the source sequence
	// state does not travel with the rows.
	var exists int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sqlite_sequence'`).Scan(&exists)
	if err != nil {
		return err
	}
	if exists == 0 {
		return nil
	}
	present, err := s.transferPresentTables(ctx)
	if err != nil {
		return err
	}
	// Only AUTOINCREMENT tables have a sqlite_sequence row; the join tables
	// and single-row checkpoints have no serial id to advance.
	seqRows, err := s.db.QueryContext(ctx, `SELECT name FROM sqlite_sequence`)
	if err != nil {
		return err
	}
	autoIncrement := make(map[string]bool)
	for seqRows.Next() {
		var name string
		if err := seqRows.Scan(&name); err != nil {
			_ = seqRows.Close()
			return err
		}
		autoIncrement[name] = true
	}
	if err := seqRows.Close(); err != nil {
		return err
	}
	for _, table := range tables {
		if !present[table] || !autoIncrement[table] {
			continue
		}
		_, err := s.db.ExecContext(ctx,
			"UPDATE sqlite_sequence SET seq = MAX(seq, COALESCE((SELECT MAX(id) FROM "+transferQuoteIdent(table)+"), 0)) WHERE name = ?",
			table)
		if err != nil {
			return fmt.Errorf("advance %s: %w", table, err)
		}
	}
	return nil
}

// transferPresentTables lists SQLite tables that physically exist.
func (s *SQLite) transferPresentTables(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// transferDeriveTable exists for interface symmetry; SQLite has no derived
// table — its dedup guard is a unique index on alerts itself.
func (s *SQLite) transferDeriveTable(ctx context.Context, table string) error {
	return fmt.Errorf("sqlite cannot derive table %s", table)
}

func (s *SQLite) transferAlertTimeRange(ctx context.Context) (time.Time, time.Time, bool, error) {
	var lo, hi sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT MIN(created_at), MAX(created_at) FROM alerts`).Scan(&lo, &hi)
	if err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	if !lo.Valid || !hi.Valid {
		return time.Time{}, time.Time{}, false, nil
	}
	from, err := parseSQLiteTime(lo.String)
	if err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	to, err := parseSQLiteTime(hi.String)
	if err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	return from, to, true, nil
}

// coerceForSQLite renders a value in the form SQLite stores it: timestamps
// in the canonical on-disk layout, booleans as INTEGER 0/1, and byte slices
// (Postgres jsonb) as TEXT so they do not become BLOBs.
func coerceForSQLite(v any) (any, error) {
	switch t := v.(type) {
	case time.Time:
		return sqliteTimeString(t), nil
	case bool:
		return boolToInt(t), nil
	case []byte:
		return string(t), nil
	default:
		return v, nil
	}
}

// ---------------------------------------------------------------------------
// Postgres implementation.
// ---------------------------------------------------------------------------

// transferColumns implements transferNode for Postgres against
// information_schema.
func (p *Postgres) transferColumns(ctx context.Context, table string) ([]transferColumn, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT column_name, data_type, is_nullable = 'YES', column_default IS NOT NULL
		  FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = $1
		 ORDER BY ordinal_position`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []transferColumn
	for rows.Next() {
		var (
			name       string
			typ        string
			isNullable bool
			hasDefault bool
		)
		if err := rows.Scan(&name, &typ, &isNullable, &hasDefault); err != nil {
			return nil, err
		}
		out = append(out, transferColumn{name: name, typ: typ, notNull: !isNullable, hasDefault: hasDefault})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// information_schema returns no rows for an unknown table: nil means
	// "no such table".
	return out, nil
}

func (p *Postgres) transferCount(ctx context.Context, table string) (int64, error) {
	var n int64
	err := p.pool.QueryRow(ctx, "SELECT COUNT(*) FROM "+transferQuoteIdent(table)).Scan(&n)
	return n, err
}

func (p *Postgres) transferRead(ctx context.Context, table string, columns, key []string, after []any, limit int) ([][]any, error) {
	limitArg := "$1"
	if len(after) > 0 {
		limitArg = fmt.Sprintf("$%d", len(after)+1)
	}
	query := transferKeysetWhere("pg", table, columns, key, len(after) > 0, limitArg)
	args := append(append([]any{}, after...), limit)

	rows, err := p.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out [][]any
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		out = append(out, vals)
	}
	return out, rows.Err()
}

func (p *Postgres) transferInsert(ctx context.Context, table string, columns []transferColumn, rows [][]any) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	var sb strings.Builder
	sb.WriteString("INSERT INTO ")
	sb.WriteString(transferQuoteIdent(table))
	sb.WriteString(" (")
	sb.WriteString(strings.Join(transferQuoteAll(columnNames(columns)), ", "))
	sb.WriteString(") VALUES ")
	sb.WriteString(transferPlaceholders("pg", len(columns), len(rows), 0))
	sb.WriteString(" ON CONFLICT DO NOTHING")

	args := make([]any, 0, len(rows)*len(columns))
	for _, row := range rows {
		for i, v := range row {
			coerced, err := coerceForPostgres(columns[i].typ, v)
			if err != nil {
				return 0, fmt.Errorf("copy %s: %w", table, err)
			}
			args = append(args, coerced)
		}
	}
	tag, err := p.pool.Exec(ctx, sb.String(), args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (p *Postgres) transferMaxParams() int { return 60000 }

func (p *Postgres) transferAdvanceSequences(ctx context.Context, tables []string) error {
	// Copying rows with explicit IDs does not move Postgres sequences the
	// way it moves SQLite's sqlite_sequence, so every serial counter is
	// pushed past max(id) explicitly. Tables without a serial id (the
	// composite-key joins, ingest_state) yield a NULL sequence and are
	// skipped.
	for _, table := range tables {
		var seq *string
		if err := p.pool.QueryRow(ctx,
			`SELECT pg_get_serial_sequence($1, 'id')`, table).Scan(&seq); err != nil {
			return fmt.Errorf("find sequence for %s: %w", table, err)
		}
		if seq == nil {
			continue
		}
		if _, err := p.pool.Exec(ctx,
			"SELECT setval($1, GREATEST(COALESCE((SELECT MAX(id) FROM "+transferQuoteIdent(table)+"), 1), 1))",
			*seq); err != nil {
			return fmt.Errorf("advance %s: %w", table, err)
		}
	}
	return nil
}

func (p *Postgres) transferAlertTimeRange(ctx context.Context) (time.Time, time.Time, bool, error) {
	var lo, hi sql.NullTime
	if err := p.pool.QueryRow(ctx, `SELECT MIN(created_at), MAX(created_at) FROM alerts`).Scan(&lo, &hi); err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	if !lo.Valid || !hi.Valid {
		return time.Time{}, time.Time{}, false, nil
	}
	return lo.Time, hi.Time, true, nil
}

// transferDeriveTable rebuilds alert_dedup — the table Postgres needs for its
// dedup guard and SQLite does not have — from the alerts copied moments
// earlier (same IDs, so alert_id stays correct). Federated alerts have no
// local rule and are excluded by rule_id's NOT NULL primary key, exactly as
// in the 0011 migration that populates this table.
func (p *Postgres) transferDeriveTable(ctx context.Context, table string) error {
	if table != "alert_dedup" {
		return fmt.Errorf("postgres cannot derive table %s", table)
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO alert_dedup (rule_id, event_id, alert_id, alert_created_at)
		SELECT rule_id, event_id, id, created_at
		  FROM alerts
		 WHERE rule_id IS NOT NULL
		ON CONFLICT DO NOTHING`)
	return err
}

// transferFillColumnGapPre relaxes delivery_attempts.alert_created_at before
// a source without that column is copied into it. The constraint is restored
// — and the column backfilled from the copied alerts — by the Post method,
// so the finished target matches a freshly migrated one.
func (p *Postgres) transferFillColumnGapPre(ctx context.Context, table string, gaps []transferColumn) error {
	if !isDeliveryAttemptParentTimeGap(table, gaps) {
		return fmt.Errorf("target requires column %s.%s, which the source schema does not have", table, gaps[0].name)
	}
	_, err := p.pool.Exec(ctx,
		`ALTER TABLE delivery_attempts ALTER COLUMN alert_created_at DROP NOT NULL`)
	return err
}

// transferFillColumnGapPost backfills the copied rows' alert_created_at from
// the alerts copied moments earlier (same IDs, same timestamps), then puts
// the NOT NULL constraint back.
func (p *Postgres) transferFillColumnGapPost(ctx context.Context, table string, gaps []transferColumn) error {
	if !isDeliveryAttemptParentTimeGap(table, gaps) {
		return nil
	}
	if _, err := p.pool.Exec(ctx, `
		UPDATE delivery_attempts da
		   SET alert_created_at = a.created_at
		  FROM alerts a
		 WHERE a.id = da.alert_id AND da.alert_created_at IS NULL`); err != nil {
		return err
	}
	_, err := p.pool.Exec(ctx,
		`ALTER TABLE delivery_attempts ALTER COLUMN alert_created_at SET NOT NULL`)
	return err
}

func isDeliveryAttemptParentTimeGap(table string, gaps []transferColumn) bool {
	return table == "delivery_attempts" && len(gaps) == 1 && gaps[0].name == "alert_created_at"
}

// coerceForPostgres converts a source value to something the pgx encoder
// accepts for the destination column's type. SQLite hands booleans as 0/1
// and timestamps as text; Postgres columns are strict about both.
func coerceForPostgres(destType string, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch destType {
	case "boolean":
		switch t := v.(type) {
		case bool:
			return t, nil
		case int64:
			return t != 0, nil
		case string:
			b, err := strconv.ParseBool(t)
			if err != nil {
				return nil, fmt.Errorf("cannot read %q as a boolean", t)
			}
			return b, nil
		}
	case "timestamp with time zone", "timestamp without time zone", "date":
		switch t := v.(type) {
		case time.Time:
			return t.UTC(), nil
		case string:
			parsed, err := parseTransferTime([]byte(t))
			if err != nil {
				return nil, err
			}
			return parsed, nil
		case []byte:
			parsed, err := parseTransferTime(t)
			if err != nil {
				return nil, err
			}
			return parsed, nil
		}
	case "bigint", "integer", "smallint", "numeric", "double precision", "real":
		switch t := v.(type) {
		case int64:
			return t, nil
		case float64:
			return t, nil
		case bool:
			return boolToInt(t), nil
		case string:
			n, err := strconv.ParseInt(t, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("cannot read %q as an integer", t)
			}
			return n, nil
		}
	case "text", "character varying", "jsonb", "json":
		switch t := v.(type) {
		case []byte:
			return string(t), nil
		case time.Time:
			return sqliteTimeString(t), nil
		case bool:
			return strconv.FormatBool(t), nil
		}
	}
	// Everything else (int64 into bigint, string into text/jsonb, []byte
	// into jsonb, time.Time into timestamptz, nil) is already what pgx
	// wants.
	return v, nil
}
