package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
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
