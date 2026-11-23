package config

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Reloader is the live half of the configuration: the settings a running
// process may change without a restart, and the code path that changes them.
//
// The reloadable list is deliberately tiny and explicit. Only settings whose
// consumers can read a new value while the process runs are here — LOG_LEVEL
// (the slog handler checks its LevelVar on every record) and POLL_INTERVAL
// (the poll loop reads its interval fresh after every cycle). Everything
// else — DATABASE_URL, HTTP_ADDR, GRPC_ADDR, SOURCE_MODE, and with them the
// whole rest of the configuration — is wired once at startup into stores,
// listeners and sources that cannot be swapped underneath the goroutines
// holding them. Restart for those.
type Reloader struct {
	mu      sync.Mutex
	current Config
}

// NewReloader wraps the configuration a process booted with, so a reload can
// diff against it and keep the settings that are not reloadable pinned to
// their startup values.
func NewReloader(cfg Config) *Reloader {
	return &Reloader{current: cfg}
}

// Current returns the configuration in effect, safe to call from any
// goroutine while a reload is running.
func (r *Reloader) Current() Config {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current
}

// Change is one setting whose effective value differs: the environment
// variable name and the old and new values, lower_snake_case so a log line
// can carry it as-is. Values here are never raw secrets — the reloadable
// settings are durations and a level, and a skipped DATABASE_URL is redacted
// the same way LogAttrs redacts it at startup.
type Change struct {
	Setting string
	Old     string
	New     string
}

// Reloaded reports what one reload did. Applied is every reloadable setting
// that changed; Skipped is every watched non-reloadable setting whose newly
// read value differs from the one in effect, named so an operator who edits
// SOURCE_MODE and sends SIGHUP is told a restart is required instead of
// silently keeping the old value.
type Reloaded struct {
	Applied []Change
	Skipped []Change
}

// Reload re-reads the configuration from the environment and CONFIG_FILE and,
// if the whole of it validates, applies the reloadable settings. Validation
// runs before anything is applied and any problem rejects the reload
// wholesale: a half-applied configuration is exactly the state a live reload
// must never leave behind, and the process keeps running on the configuration
// it booted with.
//
// The returned Reloaded lists what changed, old value to new value, so the
// caller can log it. Only the reloadable settings ever appear under Applied,
// so nothing secret can reach a log line through this path; Skipped values
// are redacted for DATABASE_URL for the same reason LogAttrs redacts it.
func (r *Reloader) Reload() (Reloaded, error) {
	fresh, problems := LoadProblems()
	if len(problems) > 0 {
		// Every problem, not just the first: the operator is standing at a
		// live process and one SIGHUP should tell them everything that is
		// wrong with the new configuration.
		errs := make([]error, len(problems))
		for i, p := range problems {
			errs[i] = fmt.Errorf("%s: %s", p.Field, p.Reason)
		}
		return Reloaded{}, fmt.Errorf("configuration reload rejected; keeping the configuration in effect: %w", errors.Join(errs...))
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	var out Reloaded
	if fresh.LogLevel != r.current.LogLevel {
		out.Applied = append(out.Applied, Change{
			Setting: "log_level",
			Old:     strings.ToLower(r.current.LogLevel.String()),
			New:     strings.ToLower(fresh.LogLevel.String()),
		})
		r.current.LogLevel = fresh.LogLevel
	}
	if fresh.PollInterval != r.current.PollInterval {
		out.Applied = append(out.Applied, Change{
			Setting: "poll_interval",
			Old:     r.current.PollInterval.String(),
			New:     fresh.PollInterval.String(),
		})
		r.current.PollInterval = fresh.PollInterval
	}
	out.Skipped = skippedChanges(r.current, fresh)
	return out, nil
}

// skippedChanges reports the watched non-reloadable settings whose freshly
// read value differs from the one in effect. The list is explicit rather
// than "anything else": a new Config field is wired once at startup like all
// the others, and becomes reloadable (or joins this list) by being named
// here, never by accident. The database URL is compared raw but reported
// redacted — the file an operator needs to check is the same either way,
// and the URL itself carries credentials.
func skippedChanges(inEffect, fresh Config) []Change {
	var out []Change
	if inEffect.DatabaseURL != fresh.DatabaseURL {
		out = append(out, Change{
			Setting: "database_url",
			Old:     redactDatabaseURL(inEffect.DatabaseURL),
			New:     redactDatabaseURL(fresh.DatabaseURL),
		})
	}
	if inEffect.HTTPAddr != fresh.HTTPAddr {
		out = append(out, Change{Setting: "http_addr", Old: inEffect.HTTPAddr, New: fresh.HTTPAddr})
	}
	if inEffect.GRPCAddr != fresh.GRPCAddr {
		out = append(out, Change{Setting: "grpc_addr", Old: inEffect.GRPCAddr, New: fresh.GRPCAddr})
	}
	if inEffect.SourceMode != fresh.SourceMode {
		out = append(out, Change{Setting: "source_mode", Old: inEffect.SourceMode, New: fresh.SourceMode})
	}
	return out
}
