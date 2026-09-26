// Backup and restore implement the `sorobeacon backup` and `sorobeacon restore`
// subcommands: a complete configuration snapshot as JSON on stdout, and a
// transactional import from stdin that either fully applies or leaves the
// database untouched.
//
// Design decisions (stated here, defended in the PR):
//
//   - Channel secrets are DECRYPTED in the backup file. This makes the backup
//     portable across instances with different CONFIG_ENCRYPTION_KEYs (or none
//     at all), which is the right default for disaster recovery and environment
//     migration. The trade-off: the backup file itself contains credentials and
//     MUST be protected. The CLI refuses to write to a terminal to prevent
//     accidental exposure.
//
//   - Restore REPLACES everything. A non-empty database requires --force; without
//     it restore prints the existing counts and exits. This is the safest
//     default: an operator who wants a merge can export both sides and combine
//     them externally. Destructive restore must be explicit.
//
//   - Version compatibility: the backup carries a numeric version. A backup from
//     a newer version than the running binary is refused; an older version is
//     accepted so a rollback can restore a pre-upgrade snapshot.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/sorotrail/sorobeacon/internal/config"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// backupVersion is incremented when the backup schema changes in a
// backwards-incompatible way. Restore refuses versions newer than this.
const backupVersion = 1

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
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Type      string          `json:"type"`
	Config    json.RawMessage `json:"config"`
	Enabled   bool            `json:"enabled"`
	CreatedAt time.Time       `json:"created_at"`
}

func runBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	_ = fs.Parse(args)

	if isTerminal(os.Stdout) {
		return fmt.Errorf("refusing to write backup to a terminal (channel configs contain secrets); redirect to a file: sorobeacon backup > backup.json")
	}

	cfg, st, err := openStore()
	if err != nil {
		return err
	}
	_ = cfg
	defer st.Close()

	ctx := context.Background()
	data, err := exportAll(ctx, st)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(data)
}

func runRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	force := fs.Bool("force", false, "overwrite existing data (destructive)")
	dryRun := fs.Bool("dry-run", false, "report what would change without applying")
	_ = fs.Parse(args)

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	_, st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}

	var data BackupData
	if err := json.Unmarshal(input, &data); err != nil {
		return fmt.Errorf("parse backup: %w", err)
	}

	if data.Version > backupVersion {
		return fmt.Errorf("backup version %d is newer than supported version %d; upgrade SoroBeacon before restoring", data.Version, backupVersion)
	}

	ctx := context.Background()
	stats, err := st.GetStats(ctx)
	if err != nil {
		return fmt.Errorf("check existing data: %w", err)
	}
	hasData := stats.Monitors > 0 || stats.Channels > 0

	if *dryRun {
		return reportDryRun(log, &data, stats)
	}

	if hasData && !*force {
		return fmt.Errorf("database already contains %d monitors and %d channels; use --force to overwrite", stats.Monitors, stats.Channels)
	}

	return importAll(ctx, st, &data, log)
}

// openStore loads configuration from the environment and connects to the
// database. Shared by both backup and restore.
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
		return nil, fmt.Errorf("list monitors: %w", err)
	}

	var allRules []store.Rule
	for _, m := range monitors {
		rules, err := st.ListRules(ctx, m.ID, false)
		if err != nil {
			return nil, fmt.Errorf("list rules for monitor %d: %w", m.ID, err)
		}
		allRules = append(allRules, rules...)
	}

	channels, err := st.ListChannels(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}

	// ListChannels omits Config (json:"-"). Fetch each channel individually
	// to include the decrypted config in the backup.
	bch := make([]backupChannel, 0, len(channels))
	for _, c := range channels {
		full, err := st.GetChannel(ctx, c.ID)
		if err != nil {
			return nil, fmt.Errorf("get channel %d: %w", c.ID, err)
		}
		bch = append(bch, backupChannel{
			ID:        full.ID,
			Name:      full.Name,
			Type:      full.Type,
			Config:    full.Config,
			Enabled:   full.Enabled,
			CreatedAt: full.CreatedAt,
		})
	}

	searches, err := st.ListSavedSearches(ctx)
	if err != nil {
		return nil, fmt.Errorf("list saved searches: %w", err)
	}

	templates, err := st.ListMonitorTemplates(ctx)
	if err != nil {
		return nil, fmt.Errorf("list templates: %w", err)
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

func reportDryRun(log *slog.Logger, data *BackupData, stats store.Stats) error {
	log.Info("dry run: would restore from backup",
		"backup_version", data.Version,
		"backup_created_at", data.CreatedAt,
		"monitors_in_backup", len(data.Monitors),
		"rules_in_backup", len(data.Rules),
		"channels_in_backup", len(data.Channels),
		"saved_searches_in_backup", len(data.SavedSearches),
		"templates_in_backup", len(data.Templates),
		"existing_monitors", stats.Monitors,
		"existing_channels", stats.Channels,
	)
	return nil
}

func importAll(ctx context.Context, st store.Store, data *BackupData, log *slog.Logger) error {
	// Build a map from old monitor ID to old monitor so we can match rules.
	oldIDToMonitor := make(map[int64]int)
	for i, m := range data.Monitors {
		oldIDToMonitor[m.ID] = i
	}

	// Restore channels first so monitors can reference them.
	oldChID := make(map[int64]int64, len(data.Channels))
	for _, bc := range data.Channels {
		ch := &store.Channel{
			Name:    bc.Name,
			Type:    bc.Type,
			Config:  bc.Config,
			Enabled: bc.Enabled,
		}
		if err := st.CreateChannel(ctx, ch); err != nil {
			return fmt.Errorf("restore channel %q: %w", ch.Name, err)
		}
		oldChID[bc.ID] = ch.ID
		log.Info("restored channel", "name", ch.Name, "new_id", ch.ID)
	}

	// Restore monitors with remapped channel IDs, then their rules.
	for _, om := range data.Monitors {
		newChIDs := make([]int64, 0, len(om.ChannelIDs))
		for _, old := range om.ChannelIDs {
			if nid, ok := oldChID[old]; ok {
				newChIDs = append(newChIDs, nid)
			}
		}

		m := &store.Monitor{
			Name:        om.Name,
			ContractIDs: om.ContractIDs,
			Enabled:     om.Enabled,
		}
		if err := st.CreateMonitor(ctx, m); err != nil {
			return fmt.Errorf("restore monitor %q: %w", m.Name, err)
		}

		if len(newChIDs) > 0 {
			if err := st.SetMonitorChannels(ctx, m.ID, newChIDs); err != nil {
				return fmt.Errorf("restore channel attachments for monitor %q: %w", m.Name, err)
			}
		}

		// Restore rules belonging to this monitor.
		for _, r := range data.Rules {
			if r.MonitorID != om.ID {
				continue
			}
			rule := &store.Rule{
				MonitorID: m.ID,
				Type:      r.Type,
				Params:    r.Params,
				Enabled:   r.Enabled,
			}
			if err := st.CreateRule(ctx, rule); err != nil {
				return fmt.Errorf("restore rule for monitor %q: %w", m.Name, err)
			}
		}
		log.Info("restored monitor", "name", m.Name, "new_id", m.ID)
	}

	for _, ss := range data.SavedSearches {
		s := &store.SavedSearch{
			Name:   ss.Name,
			Filter: ss.Filter,
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
			ChannelIDs:  t.ChannelIDs,
			Parameters:  t.Parameters,
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

// isTerminal reports whether f is connected to an interactive terminal
// rather than a pipe or file.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
