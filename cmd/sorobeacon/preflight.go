package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sorotrail/sorobeacon/internal/config"
	"github.com/sorotrail/sorobeacon/internal/horizon"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/secrets"
	"github.com/sorotrail/sorobeacon/internal/sorotrail"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

const (
	defaultPreflightTimeout = 15 * time.Second
	exitPreflightConfig     = 4
	exitPreflightDependency = 5
	exitPreflightIncomplete = 6
)

const preflightUsage = `sorobeacon preflight — validate a deployment configuration.

Usage:
  sorobeacon preflight [--json] [--timeout DURATION] [--only CHECKS] [--send-tests]

Checks: config, database, source, channels, partitions. Dependency checks run
concurrently. --send-tests explicitly delivers a synthetic alert through
enabled channels; without it, preflight never sends a message.
`

type preflightCheck struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Detail      string `json:"detail"`
	Remediation string `json:"remediation,omitempty"`
}

type preflightReport struct {
	Status string           `json:"status"`
	Checks []preflightCheck `json:"checks"`
}

type preflightOptions struct {
	timeout   time.Duration
	only      map[string]bool
	sendTests bool
	json      bool
}

type preflightExitError struct {
	code int
	err  error
}

func (e *preflightExitError) Error() string { return e.err.Error() }
func (e *preflightExitError) Unwrap() error { return e.err }

func runPreflightCLI(ctx context.Context, args []string, out io.Writer) error {
	fs := newFlagSet("preflight")
	jsonOutput := fs.Bool("json", false, "write a JSON report")
	timeout := fs.Duration("timeout", defaultPreflightTimeout, "shared deadline for dependency checks")
	only := fs.String("only", "", "comma-separated checks: config,database,source,channels")
	sendTests := fs.Bool("send-tests", false, "deliver a synthetic alert through enabled channels")
	if err := parseFlags(fs, args, preflightUsage, out); err != nil {
		if errors.Is(err, errHelp) {
			return nil
		}
		return err
	}
	if err := noExtraArgs(fs); err != nil {
		return err
	}
	if *timeout <= 0 || *timeout > 10*time.Minute {
		return &usageError{err: errors.New("--timeout must be greater than 0 and no more than 10m")}
	}

	selected, err := parsePreflightSelection(*only)
	if err != nil {
		return &usageError{err: err}
	}
	opts := preflightOptions{timeout: *timeout, only: selected, sendTests: *sendTests, json: *jsonOutput}
	cfg, err := config.Load()
	if err != nil {
		report := preflightReport{Status: "failed", Checks: []preflightCheck{{
			Name:        "config",
			Status:      "fail",
			Detail:      "configuration could not be loaded; details are suppressed to protect configured credentials",
			Remediation: "check the required variables and formats in docs/configuration.md",
		}}}
		_ = writePreflightReport(out, report, opts.json)
		return &preflightExitError{code: exitPreflightConfig, err: errors.New("invalid configuration")}
	}

	report := runPreflight(ctx, cfg, opts)
	if err := writePreflightReport(out, report, opts.json); err != nil {
		return err
	}
	for _, result := range report.Checks {
		if result.Status == "fail" {
			return &preflightExitError{code: exitPreflightDependency, err: errors.New("preflight dependency checks failed")}
		}
		if result.Status == "warning" {
			return &preflightExitError{code: exitPreflightIncomplete, err: errors.New("preflight could not verify every configured dependency")}
		}
	}
	return nil
}

func parsePreflightSelection(raw string) (map[string]bool, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	selected := map[string]bool{"config": true}
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		switch name {
		case "config", "database", "source", "channels", "partitions":
			selected[name] = true
		default:
			return nil, fmt.Errorf("unknown preflight check %q (want config,database,source,channels,partitions)", name)
		}
	}
	return selected, nil
}

func runPreflight(ctx context.Context, cfg config.Config, opts preflightOptions) preflightReport {
	checks := []preflightCheck{{Name: "config", Status: "pass", Detail: "configuration loaded and validated"}}
	type resultGroup struct {
		name    string
		results []preflightCheck
	}
	work := []struct {
		name string
		run  func(context.Context) []preflightCheck
	}{
		{name: "database", run: func(ctx context.Context) []preflightCheck { return checkPreflightDatabase(ctx, cfg) }},
		{name: "source", run: func(ctx context.Context) []preflightCheck { return checkPreflightSource(ctx, cfg) }},
		{name: "channels", run: func(ctx context.Context) []preflightCheck { return checkPreflightChannels(ctx, cfg, opts.sendTests) }},
		{name: "partitions", run: func(ctx context.Context) []preflightCheck { return checkPreflightPartitions(ctx, cfg) }},
	}
	sharedCtx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()
	results := make(chan resultGroup, len(work))
	var wg sync.WaitGroup
	for _, check := range work {
		if opts.only != nil && !opts.only[check.name] {
			continue
		}
		wg.Add(1)
		go func(name string, run func(context.Context) []preflightCheck) {
			defer wg.Done()
			results <- resultGroup{name: name, results: run(sharedCtx)}
		}(check.name, check.run)
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	for group := range results {
		checks = append(checks, group.results...)
	}
	sort.SliceStable(checks, func(i, j int) bool { return checks[i].Name < checks[j].Name })
	status := "pass"
	for _, check := range checks {
		if check.Status == "fail" {
			status = "failed"
			break
		}
		if check.Status == "warning" {
			status = "incomplete"
		}
	}
	return preflightReport{Status: status, Checks: checks}
}

func checkPreflightDatabase(ctx context.Context, cfg config.Config) []preflightCheck {
	current, latest, dirty, err := store.MigrationState(ctx, cfg.DatabaseURL)
	if err != nil {
		return []preflightCheck{{Name: "database", Status: "fail", Detail: redactPreflightError(err, cfg.DatabaseURL), Remediation: "make DATABASE_URL reachable and ensure the database is initialized"}}
	}
	if dirty || current != latest {
		return []preflightCheck{{Name: "database", Status: "fail", Detail: fmt.Sprintf("migration state is current=%d latest=%d dirty=%t", current, latest, dirty), Remediation: "apply the SoroBeacon migrations before deploying this version"}}
	}
	return []preflightCheck{{Name: "database", Status: "pass", Detail: fmt.Sprintf("reachable; migrations are current at version %d", current)}}
}

func checkPreflightSource(ctx context.Context, cfg config.Config) []preflightCheck {
	_, health, err := buildSource(ctx, slog.New(slog.DiscardHandler), cfg)
	if err != nil {
		return []preflightCheck{{Name: "source", Status: "fail", Detail: redactPreflightError(err, cfg.RPCURLs...), Remediation: "verify SOURCE_MODE, its endpoint, and the configured Stellar network"}}
	}
	result, err := health.GetHealth(ctx)
	if err != nil {
		return []preflightCheck{{Name: "source", Status: "fail", Detail: redactPreflightError(err, append(cfg.RPCURLs, cfg.SoroTrailURL, cfg.HorizonURL)...), Remediation: "make the configured event source reachable and verify its network"}}
	}
	var passphrase string
	switch cfg.SourceMode {
	case "sorotrail":
		passphrase, err = sorotrail.NewClient(cfg.SoroTrailURL, nil).NetworkPassphrase(ctx)
	case "horizon":
		passphrase, err = horizon.NewClient(cfg.HorizonURL, nil).NetworkPassphrase(ctx)
	default:
		passphrase = cfg.Network.Passphrase
	}
	endpoints := append(append([]string{}, cfg.RPCURLs...), cfg.SoroTrailURL, cfg.HorizonURL)
	if err != nil {
		return []preflightCheck{{Name: "source", Status: "fail", Detail: redactPreflightError(err, endpoints...), Remediation: "make the configured event source reachable and verify its network"}}
	}
	if passphrase == "" {
		return []preflightCheck{{Name: "source", Status: "warning", Detail: fmt.Sprintf("reachable; latest ledger %d; the upstream does not report network identity", result.LatestLedger), Remediation: "verify the upstream deployment is indexing the configured NETWORK"}}
	}
	if err := config.VerifyPassphrase(cfg.Network.Passphrase, passphrase); err != nil {
		return []preflightCheck{{Name: "source", Status: "fail", Detail: redactPreflightError(err, endpoints...), Remediation: "point the event source at the configured Stellar network"}}
	}
	return []preflightCheck{{Name: "source", Status: "pass", Detail: fmt.Sprintf("reachable; network %s; latest ledger %d", cfg.Network.Name, result.LatestLedger)}}
}

func checkPreflightChannels(ctx context.Context, cfg config.Config, sendTests bool) []preflightCheck {
	st, err := openPreflightStore(ctx, cfg)
	if err != nil {
		return []preflightCheck{{Name: "channels", Status: "fail", Detail: redactPreflightError(err, cfg.DatabaseURL), Remediation: "make the migrated database available to read saved channel configurations"}}
	}
	defer st.Close()
	channels, err := st.ListChannels(workspace.WithSystem(ctx), false)
	if err != nil {
		return []preflightCheck{{Name: "channels", Status: "fail", Detail: redactPreflightError(err, cfg.DatabaseURL), Remediation: "verify the database migration state and CONFIG_ENCRYPTION_KEY"}}
	}
	if len(channels) == 0 {
		return []preflightCheck{{Name: "channels", Status: "pass", Detail: "no saved channels to validate"}}
	}
	factory := preflightFactory(cfg)
	results := make([]preflightCheck, len(channels))
	var wg sync.WaitGroup
	for i := range channels {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			channel := channels[i]
			name := fmt.Sprintf("channel/%d", channel.ID)
			notifier, err := validateChannelConfiguration(ctx, factory, channel, cfg.SecretsProvider)
			if err != nil {
				results[i] = preflightCheck{Name: name, Status: "fail", Detail: redactPreflightError(err, channelConfigValues(channel.Config)...), Remediation: "correct this channel's type, configuration, and secret references"}
				return
			}
			if sendTests {
				if !channel.Enabled {
					results[i] = preflightCheck{Name: name, Status: "warning", Detail: "configuration validated; disabled channel was not sent a test alert", Remediation: "enable the channel before using --send-tests"}
					return
				}
				err := notifier.Send(ctx, notify.Alert{MonitorName: "SoroBeacon preflight", RuleType: "preflight", EventName: "sorobeacon_preflight", EventID: fmt.Sprintf("preflight-%d", channel.ID), CreatedAt: time.Now().UTC()})
				if err != nil {
					results[i] = preflightCheck{Name: name, Status: "fail", Detail: redactPreflightError(err, channelConfigValues(channel.Config)...), Remediation: "check the channel credential and destination; this explicit test may have reached the real endpoint"}
					return
				}
				results[i] = preflightCheck{Name: name, Status: "pass", Detail: "synthetic test alert delivered by explicit request"}
				return
			}
			checker, ok := notifier.(notify.PreflightChecker)
			if !ok {
				results[i] = preflightCheck{Name: name, Status: "warning", Detail: "configuration and secret references validated; no non-delivering remote probe is available", Remediation: "use --send-tests only when a real synthetic delivery is acceptable"}
				return
			}
			if err := checker.Check(ctx); err != nil {
				results[i] = preflightCheck{Name: name, Status: "fail", Detail: redactPreflightError(err, channelConfigValues(channel.Config)...), Remediation: "check the channel credential and destination"}
				return
			}
			results[i] = preflightCheck{Name: name, Status: "pass", Detail: "configuration, secret references, and non-delivering remote probe passed"}
		}(i)
	}
	wg.Wait()
	return results
}

func checkPreflightPartitions(ctx context.Context, cfg config.Config) []preflightCheck {
	st, err := openPreflightStore(ctx, cfg)
	if err != nil {
		return []preflightCheck{{Name: "partitions", Status: "fail", Detail: redactPreflightError(err, cfg.DatabaseURL), Remediation: "make the migrated database available for partition inspection"}}
	}
	defer st.Close()
	checker, ok := st.(store.PartitionRunwayChecker)
	if !ok {
		return []preflightCheck{{Name: "partitions", Status: "skip", Detail: "not applicable to this database backend"}}
	}
	if err := checker.CheckAlertPartitionRunway(ctx, time.Now().UTC(), 3); err != nil {
		return []preflightCheck{{Name: "partitions", Status: "fail", Detail: redactPreflightError(err, cfg.DatabaseURL), Remediation: "restart the current SoroBeacon version to create the normal three-month partition runway"}}
	}
	return []preflightCheck{{Name: "partitions", Status: "pass", Detail: "the next three monthly alert partitions exist"}}
}

func validateChannelConfiguration(ctx context.Context, factory *notify.Factory, channel store.Channel, secretsProvider string) (notify.Notifier, error) {
	if secretsProvider == "" {
		for _, value := range channelConfigValues(channel.Config) {
			if _, ok := secrets.ParseReference(value); ok {
				return nil, errors.New("channel config contains a secret reference but SECRETS_PROVIDER is not configured")
			}
		}
	}
	return factory.NewContext(ctx, channel.Type, channel.Config)
}

func openPreflightStore(ctx context.Context, cfg config.Config) (store.Store, error) {
	var cipher store.ConfigCipher
	if len(cfg.ConfigEncryptionKey) > 0 {
		var err error
		cipher, err = store.NewAESGCMCipher(cfg.ConfigEncryptionKey)
		if err != nil {
			return nil, err
		}
	}
	return store.NewReadOnly(ctx, cfg.DatabaseURL, store.PoolSettings{
		MaxConns:        cfg.DatabaseMaxConns,
		MinConns:        cfg.DatabaseMinConns,
		MaxConnLifetime: cfg.DatabaseMaxConnLifetime,
		MaxConnIdleTime: cfg.DatabaseMaxConnIdleTime,
	}, cipher)
}

func preflightFactory(cfg config.Config) *notify.Factory {
	factory := notify.DefaultFactory()
	var provider secrets.Provider
	switch cfg.SecretsProvider {
	case "env":
		provider = secrets.NewEnvProvider()
	case "vault":
		provider = secrets.NewVaultProvider(cfg.VaultAddr, cfg.VaultToken, cfg.VaultNamespace)
	}
	if provider != nil {
		factory.WithSecrets(secrets.NewResolver(provider).WithTTL(0))
	}
	return factory
}

func channelConfigValues(raw json.RawMessage) []string {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	var values []string
	var walk func(any)
	walk = func(v any) {
		switch typed := v.(type) {
		case string:
			if typed != "" {
				values = append(values, typed)
			}
		case []any:
			for _, item := range typed {
				walk(item)
			}
		case map[string]any:
			for _, item := range typed {
				walk(item)
			}
		}
	}
	walk(value)
	return values
}

func redactPreflightError(err error, values ...string) string {
	message := err.Error()
	for _, value := range values {
		if value != "" {
			message = strings.ReplaceAll(message, value, "[redacted]")
		}
	}
	return message
}

func writePreflightReport(out io.Writer, report preflightReport, jsonOutput bool) error {
	if jsonOutput {
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	for _, check := range report.Checks {
		if _, err := fmt.Fprintf(out, "%-7s %-14s %s\n", strings.ToUpper(check.Status), check.Name, check.Detail); err != nil {
			return err
		}
		if check.Remediation != "" {
			if _, err := fmt.Fprintf(out, "        action: %s\n", check.Remediation); err != nil {
				return err
			}
		}
	}
	return nil
}
