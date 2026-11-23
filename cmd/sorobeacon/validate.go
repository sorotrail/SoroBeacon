// The validate subcommand checks the configuration and exits. It is the
// pre-deployment check: a pipeline runs it before it stops the old version,
// so a broken configuration is found while the service is still up.
//
// The configuration itself is read through config.LoadProblems, the same
// code path startup uses, so the two cannot disagree about what a valid
// configuration is. What this command adds is the reporting: every problem
// at once instead of the first one, and an exit code a pipeline can branch
// on.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/config"
	"github.com/sorotrail/sorobeacon/internal/horizon"
	"github.com/sorotrail/sorobeacon/internal/sorotrail"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// Exit codes. A pipeline branches on them, so they are part of the
// command's contract: 1 means the configuration is wrong, 2 means the
// configuration is fine but a dependency could not be reached.
const (
	// exitValidateOK means the configuration is valid (and every requested
	// check passed).
	exitValidateOK = 0
	// exitValidateInvalid means the configuration itself is wrong. The
	// process would refuse to start with it.
	exitValidateInvalid = 1
	// exitValidateUnreachable means the configuration parsed, but a
	// dependency it names could not be reached. The distinction matters:
	// the first is fixed by editing a file, the second by waiting for a
	// database or by fixing the network.
	exitValidateUnreachable = 2
)

const validateUsage = `sorobeacon validate — check the configuration without starting the service.

Usage:
  sorobeacon validate [--connectivity] [--json] [--timeout DURATION]

The configuration is read from the environment (and CONFIG_FILE, if set)
exactly as startup reads it, so this command cannot disagree with the
service about what is valid. Every problem is reported in one run.

Flags:
  --connectivity   also check that the database and the event source are
                   reachable, and that the database's schema is migrated
  --json           machine-readable JSON instead of the default text
  --timeout DUR    per-dependency timeout for the connectivity checks
                   (default 10s)

Exit codes:
  0  the configuration is valid
  1  the configuration is invalid
  2  a dependency named by the configuration could not be reached

The connectivity checks are opt-in because they make the command slow and
network-dependent: a configuration that names a database on another host
cannot be checked from a machine that cannot route to it.
`

// validateRequested reports whether the arguments ask for the validate
// command and returns the arguments left over once the command word itself
// is removed.
//
// The command is dispatched before the CLI client, so it has to find its
// own name among the arguments: a script that always passes the global
// flags first (`sorobeacon --json validate`) means the same thing as one
// that does not.
func validateRequested(args []string) (bool, []string) {
	found := false
	rest := make([]string, 0, len(args))
	for _, arg := range args {
		if !found && arg == "validate" {
			found = true
			continue
		}
		rest = append(rest, arg)
	}
	return found, rest
}

// validateOptions are the flags the validate command accepts.
type validateOptions struct {
	connectivity bool
	json         bool
	timeout      time.Duration
}

// validateProblem is one configuration problem, as reported.
type validateProblem struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// validateCheck is one dependency check, as reported.
type validateCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// validateReport is the result of a validation run. The human-readable
// output is rendered from it, so the two forms can never disagree.
type validateReport struct {
	// Valid is true when no configuration problem was found and every
	// requested check passed.
	Valid bool `json:"valid"`
	// Problems lists every configuration problem, in the order the checks
	// ran.
	Problems []validateProblem `json:"problems"`
	// Checks lists the dependency checks that ran. Empty when the
	// connectivity checks were not requested.
	Checks []validateCheck `json:"checks"`
}

// runValidate implements `sorobeacon validate`. It returns the exit code
// rather than an error, because the exit code is the point of the command:
// a pipeline branches on it.
func runValidate(ctx context.Context, args []string, out io.Writer) int {
	fs := newFlagSet("validate")
	var opts validateOptions
	fs.BoolVar(&opts.connectivity, "connectivity", false, "also check that the database and the event source are reachable")
	fs.BoolVar(&opts.json, "json", false, "machine-readable JSON instead of the default text")
	fs.DurationVar(&opts.timeout, "timeout", 10*time.Second, "per-dependency timeout for the connectivity checks")
	// The global flags are accepted and ignored. A script that always
	// passes --url and --token should not fail on a command that talks to
	// no instance and needs no credential.
	fs.String("url", "", "ignored: validate reads the environment, not an instance")
	fs.String("token", "", "ignored: validate reads the environment, not an instance")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(out, validateUsage)
			return exitValidateOK
		}
		fmt.Fprintf(out, "sorobeacon: %s\n", err)
		return exitValidateInvalid
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(out, "sorobeacon: unexpected argument %q\n", fs.Arg(0))
		return exitValidateInvalid
	}

	cfg, problems := config.LoadProblems()
	report := validateConfig(cfg, problems)
	if opts.connectivity {
		report.Checks = validateConnectivity(ctx, cfg, report, opts.timeout)
	}
	report.Valid = len(report.Problems) == 0 && allChecksOK(report.Checks)

	if opts.json {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(report); err != nil {
			fmt.Fprintf(out, "sorobeacon: %s\n", err)
			return exitValidateInvalid
		}
	} else {
		if err := printValidateReport(out, report); err != nil {
			return exitValidateInvalid
		}
	}

	switch {
	case len(report.Problems) > 0:
		return exitValidateInvalid
	case !allChecksOK(report.Checks):
		return exitValidateUnreachable
	default:
		return exitValidateOK
	}
}

// validateConfig turns the problems the configuration loader found into a
// report. The problems are the ones startup would refuse to start with,
// reported together rather than one run at a time.
func validateConfig(cfg config.Config, problems []config.Problem) validateReport {
	report := validateReport{Valid: len(problems) == 0, Problems: []validateProblem{}, Checks: []validateCheck{}}
	for _, p := range problems {
		report.Problems = append(report.Problems, validateProblem{
			Field:  p.Field,
			Reason: redactSecrets(p.Reason, cfg),
		})
	}
	return report
}

// validateConnectivity runs the dependency checks: the database and the
// event source the configuration names. A dependency whose configuration is
// itself broken is skipped rather than reported as unreachable — the
// configuration problem is the real answer, and the two together would
// only be noise.
func validateConnectivity(ctx context.Context, cfg config.Config, report validateReport, timeout time.Duration) []validateCheck {
	checks := []validateCheck{}

	if hasProblem(report, "DATABASE_URL") {
		checks = append(checks, validateCheck{Name: "database", OK: false, Detail: "skipped: DATABASE_URL is invalid"})
	} else {
		checks = append(checks, checkDatabase(ctx, cfg, timeout))
	}

	switch cfg.SourceMode {
	case "sorotrail":
		if hasProblem(report, "SOROTRAIL_URL") {
			checks = append(checks, validateCheck{Name: "sorotrail", OK: false, Detail: "skipped: SOROTRAIL_URL is invalid"})
		} else {
			checks = append(checks, checkSoroTrail(ctx, cfg, timeout))
		}
	case "horizon":
		if hasProblem(report, "HORIZON_URL") {
			checks = append(checks, validateCheck{Name: "horizon", OK: false, Detail: "skipped: HORIZON_URL is invalid"})
		} else {
			checks = append(checks, checkHorizon(ctx, cfg, timeout))
		}
	default:
		if hasProblem(report, "RPC_URL") {
			checks = append(checks, validateCheck{Name: "rpc", OK: false, Detail: "skipped: RPC_URL is invalid"})
		} else {
			checks = append(checks, checkRPC(ctx, cfg, timeout))
		}
	}
	return checks
}

// checkDatabase verifies that the database is reachable and that its schema
// is at the version this binary expects.
func checkDatabase(ctx context.Context, cfg config.Config, timeout time.Duration) validateCheck {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	status, err := store.CheckSchema(ctx, cfg.DatabaseURL)
	if err != nil {
		return validateCheck{Name: "database", OK: false, Detail: redactSecrets(err.Error(), cfg)}
	}
	switch {
	case status.Dirty:
		return validateCheck{
			Name:   "database",
			OK:     false,
			Detail: fmt.Sprintf("reachable, but migration %d failed part-way; the schema is in an unknown state", status.Current),
		}
	case status.Migrated:
		return validateCheck{Name: "database", OK: true, Detail: fmt.Sprintf("reachable, schema version %d", status.Current)}
	default:
		return validateCheck{
			Name:   "database",
			OK:     false,
			Detail: fmt.Sprintf("reachable, but schema version is %d and this binary expects %d; run the migrations", status.Current, status.Expected),
		}
	}
}

// checkRPC verifies that at least one configured RPC endpoint answers and
// that it serves the configured network. An endpoint that is down is not
// fatal on its own — failover exists for exactly that — so the check passes
// when one endpoint is reachable, and says how many are.
func checkRPC(ctx context.Context, cfg config.Config, timeout time.Duration) validateCheck {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := stellar.NewFailoverClient(cfg.RPCURLs, nil, nil)
	endpoints := client.Endpoints()
	if len(endpoints) == 0 {
		return validateCheck{Name: "rpc", OK: false, Detail: "no RPC endpoint is configured"}
	}
	reachable := 0
	var lastErr error
	for _, ep := range endpoints {
		health, err := ep.Client.GetHealth(ctx)
		if err != nil {
			lastErr = err
			continue
		}
		reachable++
		// The network check is what catches a mainnet endpoint behind a
		// testnet configuration: every monitor would be evaluated against
		// the wrong chain, silently.
		network, err := ep.Client.GetNetwork(ctx)
		if err == nil {
			if err := config.VerifyPassphrase(cfg.Network.Passphrase, network.Passphrase); err != nil {
				return validateCheck{Name: "rpc", OK: false, Detail: redactSecrets(err.Error(), cfg)}
			}
		}
		detail := fmt.Sprintf("%d of %d endpoint(s) reachable, latest ledger %d", reachable, len(endpoints), health.LatestLedger)
		if reachable == len(endpoints) {
			detail = fmt.Sprintf("all %d endpoint(s) reachable, latest ledger %d", len(endpoints), health.LatestLedger)
		}
		return validateCheck{Name: "rpc", OK: true, Detail: detail}
	}
	detail := fmt.Sprintf("no RPC endpoint is reachable (%d configured)", len(endpoints))
	if lastErr != nil {
		detail += ": " + lastErr.Error()
	}
	return validateCheck{Name: "rpc", OK: false, Detail: redactSecrets(detail, cfg)}
}

// checkSoroTrail verifies that the upstream indexer answers and serves the
// configured Stellar network.
func checkSoroTrail(ctx context.Context, cfg config.Config, timeout time.Duration) validateCheck {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := sorotrail.NewClient(cfg.SoroTrailURL, nil)
	health, err := client.GetHealth(ctx)
	if err != nil {
		return validateCheck{Name: "sorotrail", OK: false, Detail: redactSecrets(err.Error(), cfg)}
	}
	if err := client.VerifyUpstream(ctx, cfg.Network.Passphrase); err != nil {
		return validateCheck{Name: "sorotrail", OK: false, Detail: redactSecrets(err.Error(), cfg)}
	}
	return validateCheck{Name: "sorotrail", OK: true, Detail: "reachable, status " + health.Status}
}

// checkHorizon verifies that the Horizon server answers.
func checkHorizon(ctx context.Context, cfg config.Config, timeout time.Duration) validateCheck {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := horizon.NewClient(cfg.HorizonURL, nil)
	health, err := client.GetHealth(ctx)
	if err != nil {
		return validateCheck{Name: "horizon", OK: false, Detail: redactSecrets(err.Error(), cfg)}
	}
	return validateCheck{Name: "horizon", OK: true, Detail: "reachable, status " + health.Status}
}

// hasProblem reports whether the configuration problem list mentions a
// field.
func hasProblem(report validateReport, field string) bool {
	for _, p := range report.Problems {
		if p.Field == field {
			return true
		}
	}
	return false
}

// allChecksOK reports whether every check passed. An empty list is
// vacuously fine: the checks were not requested.
func allChecksOK(checks []validateCheck) bool {
	for _, c := range checks {
		if !c.OK {
			return false
		}
	}
	return true
}

// printValidateReport writes the human-readable form: the problems first,
// then the checks, then a one-line verdict.
// It returns an error so the caller can satisfy errcheck on fmt.Fprintln.
func printValidateReport(out io.Writer, report validateReport) error {
	if len(report.Problems) > 0 {
		if _, err := fmt.Fprintf(out, "configuration is invalid: %d problem(s)\n", len(report.Problems)); err != nil {
			return err
		}
		for _, p := range report.Problems {
			if _, err := fmt.Fprintf(out, "  %s: %s\n", p.Field, p.Reason); err != nil {
				return err
			}
		}
	}
	for _, c := range report.Checks {
		state := "ok"
		if !c.OK {
			state = "FAILED"
		}
		if _, err := fmt.Fprintf(out, "  %s: %s%s\n", c.Name, state, detailSuffix(c.Detail)); err != nil {
			return err
		}
	}
	if len(report.Problems) == 0 && allChecksOK(report.Checks) {
		if _, err := fmt.Fprintln(out, "configuration is valid"); err != nil {
			return err
		}
	}
	return nil
}

// detailSuffix renders a check's detail as a suffix of its line.
func detailSuffix(detail string) string {
	if detail == "" {
		return ""
	}
	return " — " + detail
}

// redactSecrets removes every known secret from s.
//
// The configuration's own messages never echo a value, but a driver's can:
// a connection failure names the user and the database, and an error from
// a URL that carries a password in it may quote the URL. Redacting at the
// last moment, on the way to the output, is what makes the guarantee hold
// for every message whatever produced it.
func redactSecrets(s string, cfg config.Config) string {
	for _, secret := range secretValues(cfg) {
		if secret == "" {
			continue
		}
		s = strings.ReplaceAll(s, secret, "[redacted]")
	}
	return s
}

// secretValues collects the values that must never reach the output.
func secretValues(cfg config.Config) []string {
	values := make([]string, 0, len(cfg.APITokens)+len(cfg.WorkspaceTokens)+3)
	values = append(values, cfg.APITokens...)
	for _, wt := range cfg.WorkspaceTokens {
		values = append(values, wt.Token)
	}
	values = append(values, cfg.VaultToken)
	values = append(values, urlPassword(cfg.DatabaseURL), urlPassword(cfg.ReplicaDatabaseURL))
	return values
}

// urlPassword returns the password in a URL's userinfo, or an empty string
// when there is none.
func urlPassword(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return ""
	}
	password, _ := u.User.Password()
	return password
}
