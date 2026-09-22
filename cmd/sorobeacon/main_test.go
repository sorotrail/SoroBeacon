package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/notify"
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

func TestGracefulShutdownBoundsExit(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	d := notify.NewDispatcher(nil, notify.DefaultFactory(), slog.New(slog.DiscardHandler))
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	start := time.Now()
	if err := gracefulShutdown(time.Second, log, d, srv); err != nil {
		t.Fatalf("gracefulShutdown: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("shutdown took %s, want bounded by grace period", elapsed)
	}
	if !strings.Contains(buf.String(), "shutdown drain") {
		t.Fatalf("expected shutdown drain log, got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "abandoned_deliveries=0") {
		t.Fatalf("expected abandoned_deliveries count, got: %s", buf.String())
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
