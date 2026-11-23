package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveHealthcheckURL(t *testing.T) {
	tests := []struct {
		name        string
		explicitURL string
		addr        string
		probePath   string
		tlsEnabled  bool
		want        string
		wantErr     bool
	}{
		{
			name:      "default colon port",
			addr:      ":8080",
			probePath: "/api/v1/health",
			want:      "http://127.0.0.1:8080/api/v1/health",
		},
		{
			name:       "wildcard ipv4 with tls",
			addr:       "0.0.0.0:8443",
			probePath:  "api/v1/livez",
			tlsEnabled: true,
			want:       "https://127.0.0.1:8443/api/v1/livez",
		},
		{
			name:      "wildcard ipv6",
			addr:      "[::]:9090",
			probePath: "/api/v1/health",
			want:      "http://127.0.0.1:9090/api/v1/health",
		},
		{
			name:        "explicit url overrides addr",
			explicitURL: "http://127.0.0.1:18080/api/v1/livez",
			addr:        ":8080",
			want:        "http://127.0.0.1:18080/api/v1/livez",
		},
		{
			name:        "invalid explicit url rejected",
			explicitURL: "://bad",
			wantErr:     true,
		},
		{
			name:    "invalid addr rejected",
			addr:    "no-port",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveHealthcheckURL(tc.explicitURL, tc.addr, tc.probePath, tc.tlsEnabled)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got url %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("url = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRunHealthcheck(t *testing.T) {
	t.Run("200 ok succeeds", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/health" {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		}))
		defer srv.Close()

		var out bytes.Buffer
		if err := runHealthcheck(context.Background(), []string{"--url", srv.URL + "/api/v1/health"}, &out); err != nil {
			t.Fatalf("runHealthcheck failed: %v", err)
		}
	})

	t.Run("non-2xx fails", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()

		var out bytes.Buffer
		err := runHealthcheck(context.Background(), []string{"--url", srv.URL + "/api/v1/health"}, &out)
		if err == nil || !strings.Contains(err.Error(), "503") {
			t.Fatalf("expected 503 error, got %v", err)
		}
	})

	t.Run("help prints usage", func(t *testing.T) {
		var out bytes.Buffer
		if err := runHealthcheck(context.Background(), []string{"--help"}, &out); err != nil {
			t.Fatalf("unexpected error on --help: %v", err)
		}
		if !strings.Contains(out.String(), "sorobeacon healthcheck") {
			t.Fatalf("unexpected help output: %s", out.String())
		}
	})
}

func TestRunMigrate(t *testing.T) {
	t.Run("sqlite migration succeeds end-to-end", func(t *testing.T) {
		clearValidateEnv(t)
		dbFile := filepath.Join(t.TempDir(), "migrate.db")
		t.Setenv("DATABASE_URL", "sqlite://"+filepath.ToSlash(dbFile))

		var out bytes.Buffer
		err := runMigrate(context.Background(), []string{"--attempts=1", "--probe-path=/api/v1/livez"}, &out)
		if err != nil {
			t.Fatalf("runMigrate failed: %v", err)
		}
		if !strings.Contains(out.String(), "sorobeacon: schema is up to date") {
			t.Fatalf("unexpected output: %s", out.String())
		}
	})

	t.Run("retries transient failure then succeeds", func(t *testing.T) {
		clearValidateEnv(t)
		t.Setenv("DATABASE_URL", "postgres://sorobeacon:supersecret@db.example:5432/sorobeacon")

		calls := 0
		deps := migrateDeps{
			migrate: func(string) error {
				calls++
				if calls < 2 {
					return errors.New("dial tcp: connection refused")
				}
				return nil
			},
			migrationState: func(context.Context, string) (uint, uint, bool, error) {
				return 18, 18, false, nil
			},
		}

		var out bytes.Buffer
		err := runMigrateWith(context.Background(), []string{"--attempts=3", "--retry-delay=1ms"}, &out, deps)
		if err != nil {
			t.Fatalf("expected retry to succeed, got: %v", err)
		}
		if calls != 2 {
			t.Fatalf("calls = %d, want 2", calls)
		}
	})

	t.Run("redacts database password on failure", func(t *testing.T) {
		clearValidateEnv(t)
		t.Setenv("DATABASE_URL", "postgres://sorobeacon:top-secret-pw@db.example:5432/sorobeacon")

		deps := migrateDeps{
			migrate: func(rawURL string) error {
				return errors.New("connect failed for " + rawURL)
			},
			migrationState: func(context.Context, string) (uint, uint, bool, error) {
				return 0, 0, false, nil
			},
		}

		var out bytes.Buffer
		err := runMigrateWith(context.Background(), []string{"--attempts=2", "--retry-delay=1ms", "--timeout", (5 * time.Second).String()}, &out, deps)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if strings.Contains(err.Error(), "top-secret-pw") {
			t.Fatalf("error leaked database password: %v", err)
		}
		if !strings.Contains(err.Error(), "[redacted]") {
			t.Fatalf("expected [redacted] in error: %v", err)
		}
	})
}
