// Backup and restore implement the `sorobeacon backup` and `sorobeacon restore`
// subcommands: a configuration snapshot as JSON on stdout, and an import from
// stdin.
//
// Design decisions (stated here, defended in the PR):
//
//   - Channel secrets are DECRYPTED in the backup file. This makes the backup
//     portable across instances with different CONFIG_ENCRYPTION_KEYs (or none
//     at all), which is the right default for disaster recovery and environment
//     migration. The trade-off: **the backup file is a credential file**. It
//     contains webhook URLs, bot tokens and SMTP passwords in plaintext even
//     when the database encrypts them at rest. The CLI refuses to write to a
//     terminal, but everything else — file permissions, encryption at rest,
//     deleting the file after use — is the operator's responsibility.
//
//   - Restore APPENDS. It never deletes or updates an existing row. An empty
//     database imports directly; a non-empty one requires --force, and even
//     then the backup's rows are added alongside the existing ones with new
//     IDs. References (monitor attachments, rule ownership, saved-search
//     filters, template channel lists) are remapped to the new IDs. The earlier
//     wording said "overwrite" and "REPLACES"; that was never true and is
//     corrected here rather than changing the behaviour in a test-only PR.
//
//   - The backup covers configuration, not history: monitors, rules, channels,
//     saved searches and templates. Alerts, delivery attempts, escalations,
//     maintenance windows, inhibitions, the audit log, tokens and the ingest
//     checkpoint are not exported. Use pg_dump for a full-fidelity copy.
//
//   - A backup that cannot be parsed or validated is refused before the
//     database is opened, so a corrupt file cannot create a schema or a row.
//     An import that fails against the backend part-way is not transactional:
//     the Store interface exposes no transaction handle, so rows written before
//     the failure remain. The next successful restore appends to them.
//
//   - Version compatibility: the backup carries a numeric version. A backup from
//     a newer version than the running binary is refused; an older version is
//     accepted so a rollback can restore a pre-upgrade snapshot.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/config"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// backupVersion is incremented when the backup schema changes in a
// backwards-incompatible way. Restore refuses versions newer than this.
const backupVersion = 1

// Process exit codes. Scripts need to tell a mistyped command from a corrupt
// backup file from a database that refused the write, because only the last is
// worth retrying unchanged.
const (
	exitFailure       = 1
	exitUsage         = 2
	exitInvalidBackup = 3
)

// usageError marks a failure the caller can fix by changing the command line:
// an unknown flag, an unexpected argument, or a restore that needs --force.
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

// invalidBackupError marks input that is malformed, truncated, internally
// inconsistent, or written by a newer schema. Nothing downstream has read it
// into a database.
type invalidBackupError struct{ err error }

func (e *invalidBackupError) Error() string { return e.err.Error() }
func (e *invalidBackupError) Unwrap() error { return e.err }

// exitCodeFor maps an error to the process exit status the CLI returns.
func exitCodeFor(err error) int {
	if err == nil {
		return 0
	}
	var invalid *invalidBackupError
	if errors.As(err, &invalid) {
		return exitInvalidBackup
	}
	var usage *usageError
	if errors.As(err, &usage) {
		return exitUsage
	}
	var preflight *preflightExitError
	if errors.As(err, &preflight) {
		return preflight.code
	}
	return exitFailure
}

// backupDeps carries the process edges of both commands so tests can drive
// them without a subprocess: restore input, backup output, diagnostics and the
// database opener.
type backupDeps struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	open   func() (config.Config, store.Store, error)
	log    *slog.Logger
}

// processBackupDeps is the production wiring.
func processBackupDeps() backupDeps {
	return backupDeps{
		stdin:  os.Stdin,
		stdout: os.Stdout,
		stderr: os.Stderr,
		open:   openStore,
		log:    slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
}

// BackupData is the top-level envelope written by `sorobeacon backup` and
// read by `sorobeacon restore`.
type BackupData struct {
	Version       int                     `json:"version"`
	CreatedAt     time.Time               `json:"created_at"`
	Monitors      []store.Monitor         `json:"monitors"`
	Rules         []store.Rule            `json:"rules"`
	Channels      []backupChannel         `json:"channels"`
	SavedSearches []store.SavedSearch     `json:"saved_searches"`
	Templates     []store.MonitorTemplate `json:"templates"`
}

// backupChannel carries the channel's config in the JSON output. The normal
// store.Channel tags Config with json:"-" to prevent accidental leaks through
// the API; the backup path needs the config and controls its own output.
type backupChannel struct {
	ID                  int64           `json:"id"`
	Name                string          `json:"name"`
	Type                string          `json:"type"`
	Config              json.RawMessage `json:"config"`
	Enabled             bool            `json:"enabled"`
	DigestMode          string          `json:"digest_mode,omitempty"`
	DigestWindowSeconds int64           `json:"digest_window_seconds,omitempty"`
	MinSeverity         store.Severity  `json:"min_severity,omitempty"`
	TimeoutSeconds      int64           `json:"timeout_seconds,omitempty"`
	CreatedAt           time.Time       `json:"created_at"`
}

func runBackup(args []string) error {
	return runBackupWith(processBackupDeps(), args)
}

func runBackupWith(deps backupDeps, args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	if err := parseBackupFlags(fs, deps.stderr, args); err != nil {
		return err
	}

	if f, ok := deps.stdout.(*os.File); ok && isTerminal(f) {
		return &usageError{fmt.Errorf("refusing to write backup to a terminal (channel configs contain secrets); redirect to a file: sorobeacon backup > backup.json")}
	}

	_, st, err := deps.open()
	if err != nil {
		return err
	}
	defer st.Close()

	data, err := exportAll(context.Background(), st)
	if err != nil {
		return err
	}

	enc := json.NewEncoder(deps.stdout)
	// Without this, encoding/json rewrites <, > and & inside the channel
	// configs as \u003c, \u003e and \u0026. The JSON stays equivalent, but a
	// diff between two backups then looks like a credential changed when the
	// only change was escaping. Key order is already preserved.
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(data); err != nil {
		return fmt.Errorf("write backup: %w", err)
	}
	return nil
}

func runRestore(args []string) error {
	return runRestoreWith(processBackupDeps(), args)
}

func runRestoreWith(deps backupDeps, args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	force := fs.Bool("force", false, "append to a non-empty database (existing rows are kept)")
	dryRun := fs.Bool("dry-run", false, "validate and report what a restore would do without writing")
	if err := parseBackupFlags(fs, deps.stderr, args); err != nil {
		return err
	}

	// Parse and validate before opening the store. openStore runs migrations,
	// so a corrupt file would otherwise create a database (and, on a fresh
	// path, a file) before being refused.
	data, err := readBackup(deps.stdin)
	if err != nil {
		return err
	}

	_, st, err := deps.open()
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	existing, err := countExisting(ctx, st)
	if err != nil {
		return fmt.Errorf("check existing data: %w", err)
	}

	// This guard runs before the dry run so --dry-run predicts the same
	// refusal the real invocation would produce. A dry run still opens the
	// store to count what is there, but writes no rows.
	if existing.total() > 0 && !*force {
		return &usageError{fmt.Errorf("database already contains %s; rerun with --force to append the backup (existing rows are kept)", existing.summary())}
	}

	if *dryRun {
		return reportDryRun(deps.log, data, existing)
	}

	return importAll(ctx, st, data, deps.log)
}

// parseFlags turns flag-package failures into usage errors rather than letting
// flag.ExitOnError terminate the test process, and rejects positional
// arguments the subcommands do not take.
func parseBackupFlags(fs *flag.FlagSet, stderr io.Writer, args []string) error {
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return &usageError{err}
	}
	if fs.NArg() != 0 {
		return &usageError{fmt.Errorf("unexpected argument %q", fs.Arg(0))}
	}
	return nil
}

// readBackup consumes and validates the entire input before anything touches
// the database. The returned error is always an invalidBackupError.
func readBackup(r io.Reader) (*BackupData, error) {
	input, err := io.ReadAll(r)
	if err != nil {
		return nil, &invalidBackupError{fmt.Errorf("read backup: %w", err)}
	}

	var data BackupData
	if err := json.Unmarshal(input, &data); err != nil {
		return nil, &invalidBackupError{fmt.Errorf("parse backup: %w", err)}
	}
	if err := validateBackup(&data); err != nil {
		return nil, &invalidBackupError{err}
	}
	return &data, nil
}

// validJSONObject reports whether raw is a JSON object. Every config column
// this backup carries is an object; null, arrays and scalars are corruption.
func validJSONObject(raw json.RawMessage) bool {
	var obj map[string]json.RawMessage
	return len(raw) > 0 && json.Unmarshal(raw, &obj) == nil && obj != nil
}

// validateBackup refuses structurally valid JSON that could not have come from
// exportAll: wrong version, missing timestamps, duplicate IDs, dangling
// references, or invalid config. This is the line between "truncated file" and
// "file that imports half a database".
func validateBackup(data *BackupData) error {
	switch {
	case data.Version > backupVersion:
		return fmt.Errorf("backup version %d is newer than supported version %d; upgrade SoroBeacon before restoring", data.Version, backupVersion)
	case data.Version < 1:
		return fmt.Errorf("backup has no usable version (got %d)", data.Version)
	}
	if data.CreatedAt.IsZero() {
		return fmt.Errorf("backup has no created_at timestamp")
	}

	monitors := make(map[int64]struct{}, len(data.Monitors))
	for _, m := range data.Monitors {
		if m.ID <= 0 {
			return fmt.Errorf("monitor has invalid id %d", m.ID)
		}
		if _, dup := monitors[m.ID]; dup {
			return fmt.Errorf("duplicate monitor id %d", m.ID)
		}
		monitors[m.ID] = struct{}{}
		if strings.TrimSpace(m.Name) == "" {
			return fmt.Errorf("monitor %d has an empty name", m.ID)
		}
		if m.CreatedAt.IsZero() {
			return fmt.Errorf("monitor %d has no created_at timestamp", m.ID)
		}
		if _, ok := store.ParsePriority(string(m.Priority)); !ok {
			return fmt.Errorf("monitor %d has unknown priority %q", m.ID, m.Priority)
		}
		for _, contractID := range m.ContractIDs {
			if strings.TrimSpace(contractID) == "" {
				return fmt.Errorf("monitor %d has an empty contract id", m.ID)
			}
		}
		if _, err := store.ValidateRunbookURL(m.RunbookURL); err != nil {
			return fmt.Errorf("monitor %d has invalid runbook_url: %w", m.ID, err)
		}
		if _, err := store.ValidateResponseGuidance(m.ResponseGuidance); err != nil {
			return fmt.Errorf("monitor %d has invalid response_guidance: %w", m.ID, err)
		}
	}

	channels := make(map[int64]struct{}, len(data.Channels))
	for _, c := range data.Channels {
		if c.ID <= 0 {
			return fmt.Errorf("channel has invalid id %d", c.ID)
		}
		if _, dup := channels[c.ID]; dup {
			return fmt.Errorf("duplicate channel id %d", c.ID)
		}
		channels[c.ID] = struct{}{}
		if strings.TrimSpace(c.Name) == "" {
			return fmt.Errorf("channel %d has an empty name", c.ID)
		}
		if strings.TrimSpace(c.Type) == "" {
			return fmt.Errorf("channel %d has an empty type", c.ID)
		}
		if c.CreatedAt.IsZero() {
			return fmt.Errorf("channel %d has no created_at timestamp", c.ID)
		}
		if !validJSONObject(c.Config) {
			return fmt.Errorf("channel %d has an invalid config", c.ID)
		}
		switch c.DigestMode {
		case store.DigestModeOff:
			if c.DigestWindowSeconds < 0 {
				return fmt.Errorf("channel %d has a negative digest window", c.ID)
			}
		case store.DigestModeWindow:
			if c.DigestWindowSeconds <= 0 {
				return fmt.Errorf("channel %d has digest mode %q but no positive window", c.ID, c.DigestMode)
			}
		default:
			return fmt.Errorf("channel %d has unknown digest mode %q", c.ID, c.DigestMode)
		}
		if c.MinSeverity != "" {
			if _, ok := store.ParseSeverity(string(c.MinSeverity)); !ok {
				return fmt.Errorf("channel %d has unknown minimum severity %q", c.ID, c.MinSeverity)
			}
		}
		if c.TimeoutSeconds != 0 {
			timeout := time.Duration(c.TimeoutSeconds) * time.Second
			if timeout < store.MinChannelTimeout || timeout > store.MaxChannelTimeout {
				return fmt.Errorf("channel %d has timeout %ds outside %d-%ds", c.ID, c.TimeoutSeconds, int(store.MinChannelTimeout/time.Second), int(store.MaxChannelTimeout/time.Second))
			}
		}
	}

	for _, m := range data.Monitors {
		for _, channelID := range m.ChannelIDs {
			if _, ok := channels[channelID]; !ok {
				return fmt.Errorf("monitor %d references channel %d, which is not in the backup", m.ID, channelID)
			}
		}
	}

	rules := make(map[int64]struct{}, len(data.Rules))
	for _, r := range data.Rules {
		if r.ID <= 0 {
			return fmt.Errorf("rule has invalid id %d", r.ID)
		}
		if _, dup := rules[r.ID]; dup {
			return fmt.Errorf("duplicate rule id %d", r.ID)
		}
		rules[r.ID] = struct{}{}
		if _, ok := monitors[r.MonitorID]; !ok {
			return fmt.Errorf("rule %d references monitor %d, which is not in the backup", r.ID, r.MonitorID)
		}
		if strings.TrimSpace(r.Type) == "" {
			return fmt.Errorf("rule %d has an empty type", r.ID)
		}
		if !validJSONObject(r.Params) {
			return fmt.Errorf("rule %d has invalid params", r.ID)
		}
		if r.Severity != "" {
			if _, ok := store.ParseSeverity(string(r.Severity)); !ok {
				return fmt.Errorf("rule %d has unknown severity %q", r.ID, r.Severity)
			}
		}
		if _, err := store.ValidateRunbookURL(r.RunbookURL); err != nil {
			return fmt.Errorf("rule %d has invalid runbook_url: %w", r.ID, err)
		}
		if _, err := store.ValidateResponseGuidance(r.ResponseGuidance); err != nil {
			return fmt.Errorf("rule %d has invalid response_guidance: %w", r.ID, err)
		}
	}

	searches := make(map[int64]struct{}, len(data.SavedSearches))
	defaults := 0
	for _, s := range data.SavedSearches {
		if s.ID <= 0 {
			return fmt.Errorf("saved search has invalid id %d", s.ID)
		}
		if _, dup := searches[s.ID]; dup {
			return fmt.Errorf("duplicate saved search id %d", s.ID)
		}
		searches[s.ID] = struct{}{}
		if strings.TrimSpace(s.Name) == "" {
			return fmt.Errorf("saved search %d has an empty name", s.ID)
		}
		if s.CreatedAt.IsZero() {
			return fmt.Errorf("saved search %d has no created_at timestamp", s.ID)
		}
		if s.Filter.MonitorID < 0 || s.Filter.RuleID < 0 {
			return fmt.Errorf("saved search %d has a negative filter reference", s.ID)
		}
		if s.IsDefault {
			defaults++
			if defaults > 1 {
				return fmt.Errorf("backup contains more than one default saved search")
			}
		}
	}

	templates := make(map[int64]struct{}, len(data.Templates))
	for _, t := range data.Templates {
		if t.ID <= 0 {
			return fmt.Errorf("template has invalid id %d", t.ID)
		}
		if _, dup := templates[t.ID]; dup {
			return fmt.Errorf("duplicate template id %d", t.ID)
		}
		templates[t.ID] = struct{}{}
		if strings.TrimSpace(t.Name) == "" {
			return fmt.Errorf("template %d has an empty name", t.ID)
		}
		if t.CreatedAt.IsZero() {
			return fmt.Errorf("template %d has no created_at timestamp", t.ID)
		}
		for _, channelID := range t.ChannelIDs {
			if channelID <= 0 {
				return fmt.Errorf("template %d has invalid channel id %d", t.ID, channelID)
			}
		}
		for _, rule := range t.Rules {
			if strings.TrimSpace(rule.Type) == "" {
				return fmt.Errorf("template %d has a rule with an empty type", t.ID)
			}
			if !validJSONObject(rule.Params) {
				return fmt.Errorf("template %d has a rule with invalid params", t.ID)
			}
		}
	}

	return nil
}

func openStore() (config.Config, store.Store, error) {
	cfg, err := config.Load()
	if err != nil {
		return cfg, nil, err
	}

	var cipher store.ConfigCipher
	if len(cfg.ConfigEncryptionKey) > 0 {
		cipher, err = store.NewAESGCMCipher(cfg.ConfigEncryptionKey)
		if err != nil {
			return cfg, nil, err
		}
	}

	ctx := context.Background()
	if err := store.Migrate(cfg.DatabaseURL); err != nil {
		return cfg, nil, err
	}
	st, err := store.New(ctx, cfg.DatabaseURL, store.PoolSettings{
		MaxConns:        cfg.DatabaseMaxConns,
		MinConns:        cfg.DatabaseMinConns,
		MaxConnLifetime: cfg.DatabaseMaxConnLifetime,
		MaxConnIdleTime: cfg.DatabaseMaxConnIdleTime,
	}, cipher)
	if err != nil {
		return cfg, nil, err
	}
	return cfg, st, nil
}

func exportAll(ctx context.Context, st store.Store) (*BackupData, error) {
	monitors, err := st.ListMonitors(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("export monitors: %w", err)
	}

	var allRules []store.Rule
	for i := range monitors {
		// ListMonitors deliberately skips the per-monitor attachment query,
		// so a monitor from that list has no ChannelIDs. Fetch it in full or
		// every backup silently drops which channels alert it.
		full, err := st.GetMonitor(ctx, monitors[i].ID)
		if err != nil {
			return nil, fmt.Errorf("export monitor %d: %w", monitors[i].ID, err)
		}
		monitors[i] = *full

		rules, err := st.ListRules(ctx, full.ID, false)
		if err != nil {
			return nil, fmt.Errorf("export rules for monitor %d: %w", full.ID, err)
		}
		allRules = append(allRules, rules...)
	}

	channels, err := st.ListChannels(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("export channels: %w", err)
	}

	// ListChannels omits Config (json:"-"). Fetch each channel individually
	// to include the decrypted config in the backup.
	bch := make([]backupChannel, 0, len(channels))
	for _, c := range channels {
		full, err := st.GetChannel(ctx, c.ID)
		if err != nil {
			return nil, fmt.Errorf("export channel %d: %w", c.ID, err)
		}
		bch = append(bch, backupChannel{
			ID:                  full.ID,
			Name:                full.Name,
			Type:                full.Type,
			Config:              full.Config,
			Enabled:             full.Enabled,
			DigestMode:          full.DigestMode,
			DigestWindowSeconds: full.DigestWindowSeconds,
			MinSeverity:         full.MinSeverity,
			TimeoutSeconds:      int64(full.TimeoutSeconds()),
			CreatedAt:           full.CreatedAt,
		})
	}

	searches, err := st.ListSavedSearches(ctx)
	if err != nil {
		return nil, fmt.Errorf("export saved searches: %w", err)
	}

	templates, err := st.ListMonitorTemplates(ctx)
	if err != nil {
		return nil, fmt.Errorf("export templates: %w", err)
	}

	return &BackupData{
		Version:       backupVersion,
		CreatedAt:     time.Now().UTC(),
		Monitors:      monitors,
		Rules:         allRules,
		Channels:      bch,
		SavedSearches: searches,
		Templates:     templates,
	}, nil
}

// existingData is what the target already holds. Alerts are reported but do
// not force --force: history does not collide with configuration.
type existingData struct {
	Monitors      int64
	Rules         int64
	Channels      int64
	SavedSearches int64
	Templates     int64
	Alerts        int64
}

func (e existingData) total() int64 {
	return e.Monitors + e.Rules + e.Channels + e.SavedSearches + e.Templates
}

func (e existingData) summary() string {
	return fmt.Sprintf("%d monitors, %d rules, %d channels, %d saved searches and %d templates",
		e.Monitors, e.Rules, e.Channels, e.SavedSearches, e.Templates)
}

func countExisting(ctx context.Context, st store.Store) (existingData, error) {
	stats, err := st.GetStats(ctx)
	if err != nil {
		return existingData{}, err
	}
	searches, err := st.ListSavedSearches(ctx)
	if err != nil {
		return existingData{}, err
	}
	templates, err := st.ListMonitorTemplates(ctx)
	if err != nil {
		return existingData{}, err
	}
	return existingData{
		Monitors:      stats.Monitors,
		Rules:         stats.Rules,
		Channels:      stats.Channels,
		SavedSearches: int64(len(searches)),
		Templates:     int64(len(templates)),
		Alerts:        stats.Alerts,
	}, nil
}

func reportDryRun(log *slog.Logger, data *BackupData, existing existingData) error {
	mode := "append"
	if existing.total() == 0 {
		mode = "into-empty-database"
	}
	log.Info("dry run: no rows written",
		"mode", mode,
		"backup_version", data.Version,
		"backup_created_at", data.CreatedAt,
		"monitors_in_backup", len(data.Monitors),
		"rules_in_backup", len(data.Rules),
		"channels_in_backup", len(data.Channels),
		"saved_searches_in_backup", len(data.SavedSearches),
		"templates_in_backup", len(data.Templates),
		"existing_monitors", existing.Monitors,
		"existing_rules", existing.Rules,
		"existing_channels", existing.Channels,
		"existing_saved_searches", existing.SavedSearches,
		"existing_templates", existing.Templates,
		"existing_alerts", existing.Alerts,
	)
	return nil
}

// importAll writes the backup's rows into st and remaps every cross-row
// reference. Validation has already proved that monitors and rules resolve;
// template channel lists may legitimately name a channel deleted after the
// template was created, so unknown template references are dropped with a
// warning rather than corrupting the restored template.
func importAll(ctx context.Context, st store.Store, data *BackupData, log *slog.Logger) error {
	channelIDs := make(map[int64]int64, len(data.Channels))
	for _, bc := range data.Channels {
		ch := &store.Channel{
			Name:                bc.Name,
			Type:                bc.Type,
			Config:              bc.Config,
			Enabled:             bc.Enabled,
			DigestMode:          bc.DigestMode,
			DigestWindowSeconds: bc.DigestWindowSeconds,
			MinSeverity:         bc.MinSeverity,
			Timeout:             time.Duration(bc.TimeoutSeconds) * time.Second,
			CreatedAt:           bc.CreatedAt,
		}
		if err := st.CreateChannel(ctx, ch); err != nil {
			return fmt.Errorf("restore channel %q: %w", ch.Name, err)
		}
		channelIDs[bc.ID] = ch.ID
		log.Info("restored channel", "name", ch.Name, "new_id", ch.ID)
	}

	monitorIDs := make(map[int64]int64, len(data.Monitors))
	for _, om := range data.Monitors {
		m := &store.Monitor{
			Name:             om.Name,
			ContractIDs:      om.ContractIDs,
			Enabled:          om.Enabled,
			Priority:         om.Priority,
			Network:          om.Network,
			RunbookURL:       om.RunbookURL,
			ResponseGuidance: om.ResponseGuidance,
			CreatedAt:        om.CreatedAt,
		}
		if err := st.CreateMonitor(ctx, m); err != nil {
			return fmt.Errorf("restore monitor %q: %w", m.Name, err)
		}
		monitorIDs[om.ID] = m.ID

		attached := remapChannelIDs(om.ChannelIDs, channelIDs, log, "monitor", om.ID)
		if len(attached) > 0 {
			if err := st.SetMonitorChannels(ctx, m.ID, attached); err != nil {
				return fmt.Errorf("restore channel attachments for monitor %q: %w", m.Name, err)
			}
		}
		log.Info("restored monitor", "name", m.Name, "new_id", m.ID)
	}

	ruleIDs := make(map[int64]int64, len(data.Rules))
	for _, r := range data.Rules {
		rule := &store.Rule{
			MonitorID:        monitorIDs[r.MonitorID],
			Type:             r.Type,
			Params:           r.Params,
			Enabled:          r.Enabled,
			Severity:         r.Severity,
			RunbookURL:       r.RunbookURL,
			ResponseGuidance: r.ResponseGuidance,
		}
		if err := st.CreateRule(ctx, rule); err != nil {
			return fmt.Errorf("restore rule %d for monitor %d: %w", r.ID, r.MonitorID, err)
		}
		ruleIDs[r.ID] = rule.ID
	}

	for _, ss := range data.SavedSearches {
		filter := ss.Filter
		if filter.MonitorID != 0 {
			if id, ok := monitorIDs[filter.MonitorID]; ok {
				filter.MonitorID = id
			} else {
				// A filter can outlive the monitor it names; the API treats a
				// stale reference as no filter, so clear it rather than
				// pointing at an unrelated monitor in the target.
				log.Warn("saved search references a monitor missing from the backup; clearing that filter",
					"saved_search", ss.Name, "old_monitor_id", filter.MonitorID)
				filter.MonitorID = 0
			}
		}
		if filter.RuleID != 0 {
			if id, ok := ruleIDs[filter.RuleID]; ok {
				filter.RuleID = id
			} else {
				log.Warn("saved search references a rule missing from the backup; clearing that filter",
					"saved_search", ss.Name, "old_rule_id", filter.RuleID)
				filter.RuleID = 0
			}
		}

		s := &store.SavedSearch{
			Name:      ss.Name,
			Filter:    filter,
			IsDefault: ss.IsDefault,
			CreatedAt: ss.CreatedAt,
		}
		if err := st.CreateSavedSearch(ctx, s); err != nil {
			return fmt.Errorf("restore saved search %q: %w", s.Name, err)
		}
	}

	for _, t := range data.Templates {
		tmpl := &store.MonitorTemplate{
			Name:        t.Name,
			Description: t.Description,
			Rules:       t.Rules,
			ChannelIDs:  remapChannelIDs(t.ChannelIDs, channelIDs, log, "template", t.ID),
			Parameters:  t.Parameters,
			CreatedAt:   t.CreatedAt,
		}
		if err := st.CreateMonitorTemplate(ctx, tmpl); err != nil {
			return fmt.Errorf("restore template %q: %w", tmpl.Name, err)
		}
	}

	log.Info("restore complete",
		"monitors", len(data.Monitors),
		"rules", len(data.Rules),
		"channels", len(data.Channels),
		"saved_searches", len(data.SavedSearches),
		"templates", len(data.Templates),
	)
	return nil
}

// remapChannelIDs converts old channel ids to the ids created in the target.
// Unknown ids are dropped and logged: keeping the old number would attach the
// restored row to a different channel that happens to have that id.
func remapChannelIDs(ids []int64, mapping map[int64]int64, log *slog.Logger, ownerKind string, ownerID int64) []int64 {
	out := make([]int64, 0, len(ids))
	for _, oldID := range ids {
		if newID, ok := mapping[oldID]; ok {
			out = append(out, newID)
			continue
		}
		log.Warn("backup row references a channel missing from the backup; dropping the reference",
			"owner_kind", ownerKind, "owner_id", ownerID, "old_channel_id", oldID)
	}
	return out
}

// isTerminal reports whether f is connected to an interactive terminal
// rather than a pipe or file.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
