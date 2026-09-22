package config

import (
	"log/slog"
	"strconv"
	"strings"
)

// Reloadable is the explicit list of settings SIGHUP may change at
// runtime. Everything else stays at the values from process start;
// Reload reports those as skipped instead of applying them silently.
var Reloadable = []string{"log_level", "poll_interval"}

// Change is one applied reload: a setting that moved from From to To.
type Change struct {
	Name string
	From string
	To   string
}

// Skip is a setting that differed after re-reading the environment but
// is not reloadable. From/To are log-safe (secrets redacted).
type Skip struct {
	Name   string
	Reason string
	From   string
	To     string
}

// ReloadResult is what a successful Reload did. Applied is empty when
// nothing reloadable changed; Skipped lists non-reloadable diffs so an
// operator who edited DATABASE_URL sees that the process ignored it.
type ReloadResult struct {
	Applied []Change
	Skipped []Skip
}

const skipNotReloadable = "not reloadable at runtime; restart to apply"

// Reload re-reads the environment and, if the new configuration is
// valid, copies log level and poll interval onto current. An invalid
// environment is rejected wholesale: current is returned unchanged so a
// bad SIGHUP cannot leave the process half-configured.
func Reload(current Config) (Config, ReloadResult, error) {
	next, err := Load()
	if err != nil {
		return current, ReloadResult{}, err
	}
	result := diffReload(current, next)
	current.LogLevel = next.LogLevel
	current.PollInterval = next.PollInterval
	return current, result, nil
}

func diffReload(prev, next Config) ReloadResult {
	var r ReloadResult
	if prev.LogLevel != next.LogLevel {
		r.Applied = append(r.Applied, Change{
			Name: "log_level",
			From: formatLevel(prev.LogLevel),
			To:   formatLevel(next.LogLevel),
		})
	}
	if prev.PollInterval != next.PollInterval {
		r.Applied = append(r.Applied, Change{
			Name: "poll_interval",
			From: prev.PollInterval.String(),
			To:   next.PollInterval.String(),
		})
	}

	addSkip := func(name, from, to string) {
		if from == to {
			return
		}
		r.Skipped = append(r.Skipped, Skip{
			Name:   name,
			Reason: skipNotReloadable,
			From:   from,
			To:     to,
		})
	}
	addSkip("database_url", redactDatabaseURL(prev.DatabaseURL), redactDatabaseURL(next.DatabaseURL))
	addSkip("http_addr", prev.HTTPAddr, next.HTTPAddr)
	addSkip("source_mode", prev.SourceMode, next.SourceMode)
	addSkip("sorotrail_url", prev.SoroTrailURL, next.SoroTrailURL)
	addSkip("rpc_url", prev.RPCURL, next.RPCURL)
	addSkip("network", prev.Network.Name, next.Network.Name)
	addSkip("http_max_body_bytes", strconv.FormatInt(prev.HTTPMaxBodyBytes, 10), strconv.FormatInt(next.HTTPMaxBodyBytes, 10))
	addSkip("readyz_lag_threshold", strconv.FormatUint(uint64(prev.ReadyzLagThreshold), 10), strconv.FormatUint(uint64(next.ReadyzLagThreshold), 10))
	addSkip("cors_allowed_origins", strings.Join(prev.CORSAllowedOrigins, ","), strings.Join(next.CORSAllowedOrigins, ","))
	addSkip("alert_retention", prev.AlertRetention.String(), next.AlertRetention.String())
	return r
}

func formatLevel(l slog.Level) string {
	return strings.ToLower(l.String())
}
