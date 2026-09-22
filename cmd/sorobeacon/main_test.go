package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/config"
	"github.com/sorotrail/sorobeacon/internal/poller"
	"github.com/sorotrail/sorobeacon/internal/rules"
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

func TestApplySIGHUPReloadsLogLevelAndPollInterval(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("POLL_INTERVAL", "5s")
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_ADDR", ":8080")
	t.Setenv("SOURCE_MODE", "rpc")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	level := new(slog.LevelVar)
	level.Set(cfg.LogLevel)
	p := poller.New(nil, nil, rules.NewRegistry(), nil, cfg.PollInterval, slog.New(slog.DiscardHandler))
	live := &liveConfig{
		cfg:    cfg,
		level:  level,
		poller: p,
		log:    slog.New(slog.NewTextHandler(&buf, nil)),
	}

	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("POLL_INTERVAL", "12s")
	t.Setenv("HTTP_ADDR", ":9999")
	applySIGHUP(live)

	if live.cfg.LogLevel != slog.LevelDebug {
		t.Fatalf("log level = %v, want debug", live.cfg.LogLevel)
	}
	if live.level.Level() != slog.LevelDebug {
		t.Fatalf("slog LevelVar = %v, want debug", live.level.Level())
	}
	if p.Interval() != 12*time.Second {
		t.Fatalf("poll interval = %s, want 12s", p.Interval())
	}
	if live.cfg.HTTPAddr != ":8080" {
		t.Fatalf("HTTP_ADDR applied on reload: %s", live.cfg.HTTPAddr)
	}
	out := buf.String()
	if !strings.Contains(out, "configuration reloaded") || !strings.Contains(out, "log_level") {
		t.Fatalf("expected applied log_level line, got: %s", out)
	}
	if !strings.Contains(out, "configuration reload skipped") || !strings.Contains(out, "http_addr") {
		t.Fatalf("expected skipped http_addr line, got: %s", out)
	}
}

func TestApplySIGHUPRejectsInvalid(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("POLL_INTERVAL", "5s")
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("HTTP_ADDR", ":8080")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	level := new(slog.LevelVar)
	level.Set(cfg.LogLevel)
	p := poller.New(nil, nil, rules.NewRegistry(), nil, cfg.PollInterval, slog.New(slog.DiscardHandler))
	live := &liveConfig{
		cfg:    cfg,
		level:  level,
		poller: p,
		log:    slog.New(slog.NewTextHandler(&buf, nil)),
	}

	t.Setenv("LOG_LEVEL", "nope")
	applySIGHUP(live)

	if live.cfg.LogLevel != slog.LevelInfo {
		t.Fatalf("rejected reload mutated log level: %v", live.cfg.LogLevel)
	}
	if p.Interval() != 5*time.Second {
		t.Fatalf("rejected reload mutated poll interval: %s", p.Interval())
	}
	if !strings.Contains(buf.String(), "configuration reload rejected") {
		t.Fatalf("expected rejection log, got: %s", buf.String())
	}
}
