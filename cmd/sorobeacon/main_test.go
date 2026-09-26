package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// fakeHealthChecker lets tests control what GetHealth returns without a
// real RPC or SoroTrail endpoint.
type fakeHealthChecker struct {
	health *stellar.Health
	err    error
}

func (f fakeHealthChecker) GetHealth(context.Context) (*stellar.Health, error) {
	return f.health, f.err
}

func TestLogStartupHealth_Healthy(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	logStartupHealth(context.Background(), log, fakeHealthChecker{
		health: &stellar.Health{Status: "healthy", LatestLedger: 12345},
	})

	out := buf.String()
	if !strings.Contains(out, "event source healthy") {
		t.Fatalf("expected a healthy log line, got: %s", out)
	}
	if !strings.Contains(out, "12345") {
		t.Fatalf("expected the latest ledger in the log line, got: %s", out)
	}
	if strings.Contains(out, "level=WARN") {
		t.Fatalf("healthy source must not log a warning, got: %s", out)
	}
}

// networkStub is a minimal RPC endpoint that answers every call with a fixed
// getNetwork passphrase, so the startup sweep can be exercised without a
// Stellar node behind it.
func networkStub(t *testing.T, passphrase string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"passphrase":%q}}`, passphrase)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestVerifyNetworkEndpoints_AllOnTheConfiguredNetwork(t *testing.T) {
	first := networkStub(t, stellar.PassphraseTestnet)
	second := networkStub(t, stellar.PassphraseTestnet)
	client := stellar.NewFailoverClient([]string{first.URL, second.URL}, nil, slog.New(slog.DiscardHandler))

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	if err := verifyNetworkEndpoints(context.Background(), log, client, stellar.PassphraseTestnet); err != nil {
		t.Fatalf("endpoints on the configured network must verify, got %v", err)
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("reachable matching endpoints must not warn, got: %s", buf.String())
	}
}

// One endpoint on the wrong chain is the whole reason the check exists:
// failover spreads calls over the list, so a mixed set would intermittently
// feed another network's events into the alert stream.
func TestVerifyNetworkEndpoints_RejectsMixedNetworks(t *testing.T) {
	testnet := networkStub(t, stellar.PassphraseTestnet)
	mainnet := networkStub(t, stellar.PassphraseMainnet)
	client := stellar.NewFailoverClient([]string{testnet.URL, mainnet.URL}, nil, slog.New(slog.DiscardHandler))

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	err := verifyNetworkEndpoints(context.Background(), log, client, stellar.PassphraseTestnet)

	if err == nil {
		t.Fatal("a mainnet endpoint in a testnet set must fail startup")
	}
	for _, want := range []string{"network mismatch", "mainnet", "testnet", mainnet.URL} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should name %q", err.Error(), want)
		}
	}
}

// An endpoint that is down at boot is exactly what failover is for, so it is
// reported and skipped rather than blocking startup.
func TestVerifyNetworkEndpoints_SkipsUnreachableEndpoint(t *testing.T) {
	unreachable := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := unreachable.URL
	unreachable.Close() // every request now fails at the transport layer

	healthy := networkStub(t, stellar.PassphraseTestnet)
	client := stellar.NewFailoverClient([]string{url, healthy.URL}, nil, slog.New(slog.DiscardHandler))

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	if err := verifyNetworkEndpoints(context.Background(), log, client, stellar.PassphraseTestnet); err != nil {
		t.Fatalf("an unreachable endpoint must not block startup, got %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "could not verify network passphrase") {
		t.Fatalf("expected a warning for the unreachable endpoint, got: %s", out)
	}
	if !strings.Contains(out, url) {
		t.Fatalf("the warning should name the endpoint, got: %s", out)
	}
}

func TestWarnIfChannelConfigUnencrypted(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	warnIfChannelConfigUnencrypted(log, nil)
	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("expected a warning log line, got: %s", out)
	}
	if !strings.Contains(out, "CONFIG_ENCRYPTION_KEY") {
		t.Fatalf("expected the warning to name CONFIG_ENCRYPTION_KEY, got: %s", out)
	}

	buf.Reset()
	warnIfChannelConfigUnencrypted(log, []byte("0123456789abcdef0123456789abcdef"))
	if buf.Len() != 0 {
		t.Fatalf("a configured key must not warn, got: %s", buf.String())
	}
}

func TestLogStartupHealth_Unreachable(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	logStartupHealth(context.Background(), log, fakeHealthChecker{
		err: errors.New("connection refused"),
	})

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("expected a warning log line, got: %s", out)
	}
	if !strings.Contains(out, "connection refused") {
		t.Fatalf("expected the underlying error in the log line, got: %s", out)
	}
}
