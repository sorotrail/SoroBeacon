package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/config"
)

// clearValidateEnv makes a test's environment deterministic: the config file
// is switched off and every variable the command reads is reset, so a
// variable set in the developer's shell cannot change the outcome.
func clearValidateEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CONFIG_FILE", "")
	t.Setenv("DATABASE_URL", "sqlite:///tmp/sorobeacon-validate-test.db")
	t.Setenv("HTTP_ADDR", ":8080")
	t.Setenv("POLL_INTERVAL", "5s")
	t.Setenv("SOURCE_MODE", "rpc")
	t.Setenv("API_TOKEN", "")
	t.Setenv("WORKSPACE_TOKENS", "")
	t.Setenv("REPLICA_DATABASE_URL", "")
	t.Setenv("CONFIG_ENCRYPTION_KEY", "")
	t.Setenv("RATE_LIMIT_RPS", "")
	t.Setenv("RATE_LIMIT_BURST", "")
	t.Setenv("OTLP_ENDPOINT", "")
	t.Setenv("VAULT_TOKEN", "")
	t.Setenv("VAULT_ADDR", "")
	t.Setenv("SECRETS_PROVIDER", "")
}

// TestValidateReportsEveryProblemInOneRun pins the behaviour the command
// exists for: an operator fixing four typos should not need four runs.
func TestValidateReportsEveryProblemInOneRun(t *testing.T) {
	clearValidateEnv(t)
	t.Setenv("DATABASE_URL", "not-a-url")
	t.Setenv("HTTP_ADDR", "no-port-here")
	t.Setenv("POLL_INTERVAL", "0s")

	var out bytes.Buffer
	code := runValidate(context.Background(), []string{}, &out)

	if code != exitValidateInvalid {
		t.Fatalf("exit code = %d, want %d (output: %s)", code, exitValidateInvalid, out.String())
	}
	for _, field := range []string{"DATABASE_URL", "HTTP_ADDR", "POLL_INTERVAL"} {
		if !strings.Contains(out.String(), field) {
			t.Errorf("output does not mention %s:\n%s", field, out.String())
		}
	}
}

// TestValidateValidConfigExitsZero covers the happy path: a configuration
// the service would start with is reported as valid.
func TestValidateValidConfigExitsZero(t *testing.T) {
	clearValidateEnv(t)

	var out bytes.Buffer
	code := runValidate(context.Background(), []string{}, &out)

	if code != exitValidateOK {
		t.Fatalf("exit code = %d, want %d (output: %s)", code, exitValidateOK, out.String())
	}
	if !strings.Contains(out.String(), "configuration is valid") {
		t.Errorf("output does not report the configuration as valid:\n%s", out.String())
	}
}

// TestValidateJSONOutput checks the machine-readable form: a script must be
// able to read the problems without parsing the text.
func TestValidateJSONOutput(t *testing.T) {
	clearValidateEnv(t)
	t.Setenv("DATABASE_URL", "not-a-url")
	t.Setenv("HTTP_ADDR", "no-port-here")

	var out bytes.Buffer
	code := runValidate(context.Background(), []string{"--json"}, &out)
	if code != exitValidateInvalid {
		t.Fatalf("exit code = %d, want %d", code, exitValidateInvalid)
	}

	var report validateReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v (output: %s)", err, out.String())
	}
	if report.Valid {
		t.Error("report says the configuration is valid, but it is not")
	}
	if len(report.Problems) != 2 {
		t.Fatalf("report has %d problems, want 2: %+v", len(report.Problems), report.Problems)
	}
	if report.Problems[0].Field != "DATABASE_URL" {
		t.Errorf("first problem is %q, want DATABASE_URL", report.Problems[0].Field)
	}
}

// TestValidateUsesTheStartupCodePath is the shared test: the validator and
// startup must agree about what a valid configuration is, so the first
// problem the validator reports is the error startup would have failed
// with.
func TestValidateUsesTheStartupCodePath(t *testing.T) {
	clearValidateEnv(t)
	t.Setenv("DATABASE_URL", "not-a-url")
	t.Setenv("HTTP_ADDR", "no-port-here")
	t.Setenv("POLL_INTERVAL", "0s")

	_, err := config.Load()
	if err == nil {
		t.Fatal("startup accepted a configuration the validator rejects")
	}
	cfg, problems := config.LoadProblems()
	report := validateConfig(cfg, problems)
	if len(report.Problems) == 0 {
		t.Fatal("validator reported no problems")
	}
	if report.Problems[0].Reason != err.Error() {
		t.Errorf("validator's first problem is %q, startup failed with %q", report.Problems[0].Reason, err.Error())
	}
}

// TestValidateNeverPrintsASecret covers both output modes. The secrets are
// in the environment exactly as an operator would set them, and the point is
// that neither the text nor the JSON form can carry one out.
func TestValidateNeverPrintsASecret(t *testing.T) {
	clearValidateEnv(t)
	const token = "s3cr3t-api-token-value"
	const password = "s3cr3t-db-password"
	t.Setenv("API_TOKEN", token)
	t.Setenv("DATABASE_URL", "postgres://user:"+password+"@127.0.0.1:1/sorobeacon?sslmode=disable&connect_timeout=1")
	t.Setenv("VAULT_TOKEN", "s3cr3t-vault-token")

	for _, args := range [][]string{{}, {"--json"}} {
		var out bytes.Buffer
		runValidate(context.Background(), args, &out)
		for _, secret := range []string{token, password, "s3cr3t-vault-token"} {
			if strings.Contains(out.String(), secret) {
				t.Errorf("output with args %v contains a secret:\n%s", args, out.String())
			}
		}
	}
}

// TestValidateUnreachableDependencyHasItsOwnExitCode covers the distinction
// a pipeline branches on: the configuration is fine, the environment is not.
func TestValidateUnreachableDependencyHasItsOwnExitCode(t *testing.T) {
	clearValidateEnv(t)
	// Port 1 on the loopback interface refuses the connection at once, so
	// the check fails without waiting for a timeout.
	t.Setenv("DATABASE_URL", "postgres://user:pass@127.0.0.1:1/sorobeacon?sslmode=disable&connect_timeout=1")

	var out bytes.Buffer
	code := runValidate(context.Background(), []string{"--connectivity", "--timeout", "5s"}, &out)

	if code != exitValidateUnreachable {
		t.Fatalf("exit code = %d, want %d (output: %s)", code, exitValidateUnreachable, out.String())
	}
	if !strings.Contains(out.String(), "database") {
		t.Errorf("output does not mention the database check:\n%s", out.String())
	}
}

// TestValidateConnectivityFailureIsNotAConfigurationProblem guards the other
// half of the exit-code contract: an unreachable database must not be
// reported as an invalid configuration.
func TestValidateConnectivityFailureIsNotAConfigurationProblem(t *testing.T) {
	clearValidateEnv(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@127.0.0.1:1/sorobeacon?sslmode=disable&connect_timeout=1")

	var out bytes.Buffer
	runValidate(context.Background(), []string{"--connectivity", "--timeout", "5s"}, &out)

	if strings.Contains(out.String(), "configuration is invalid") {
		t.Errorf("an unreachable database was reported as an invalid configuration:\n%s", out.String())
	}
}

// TestValidateRequestedFindsTheCommandAnywhere checks the dispatch: the
// global flags may come before or after the command.
func TestValidateRequestedFindsTheCommandAnywhere(t *testing.T) {
	for _, args := range [][]string{
		{"validate"},
		{"--json", "validate"},
		{"validate", "--json"},
	} {
		found, rest := validateRequested(args)
		if !found {
			t.Errorf("validate not found in %v", args)
			continue
		}
		for _, arg := range rest {
			if arg == "validate" {
				t.Errorf("the command word was left in the arguments %v", rest)
			}
		}
	}
}
