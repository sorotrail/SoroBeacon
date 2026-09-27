package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/reqid"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// statsStore stubs GetStats so handler tests can drive empty, populated,
// and error responses without a real database.
type statsStore struct {
	store.Store
	stats store.Stats
	err   error
}

func (s *statsStore) GetStats(context.Context) (store.Stats, error) {
	return s.stats, s.err
}

func statsServer(st store.Store) *httptest.Server {
	s := New(st, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger())
	// Production mounts Routes() under /api/v1 behind reqid.Middleware;
	// wrapping here so the 500 envelope's request_id matches the header.
	return httptest.NewServer(reqid.Middleware(s.Routes()))
}

// documentedStatsKeys is the JSON contract GET /stats must keep. A rename
// here is a dashboard break, so the empty-instance case is the one that
// would hide a dropped zero field.
var documentedStatsKeys = []string{
	"monitors",
	"rules",
	"channels",
	"alerts",
	"alerts_last_24h",
	"last_ledger",
	"last_poll_at",
}

func getStats(t *testing.T, st store.Store) (status int, ct string, raw map[string]any, body []byte, reqID string) {
	t.Helper()
	srv := statsServer(st)
	defer srv.Close()

	res, err := http.Get(srv.URL + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err = io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	raw = map[string]any{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("decode: %v body=%s", err, body)
		}
	}
	return res.StatusCode, res.Header.Get("Content-Type"), raw, body, res.Header.Get(reqid.Header)
}

func TestStatsEndpointShape(t *testing.T) {
	pollAt := time.Date(2026, 9, 21, 8, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		stats store.Stats
	}{
		{
			name:  "empty",
			stats: store.Stats{},
		},
		{
			name: "populated",
			stats: store.Stats{
				Monitors:     2,
				Rules:        3,
				Channels:     1,
				Alerts:       10,
				AlertsLast24: 4,
				LastLedger:   12345,
				LastPollAt:   pollAt,
			},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			status, ct, raw, body, _ := getStats(t, &statsStore{stats: tt.stats})
			if status != http.StatusOK {
				t.Fatalf("GET /stats = %d, want 200; body=%s", status, body)
			}
			if ct != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", ct)
			}

			for _, key := range documentedStatsKeys {
				if _, ok := raw[key]; !ok {
					t.Fatalf("missing field %q in %s", key, body)
				}
			}
			if len(raw) != len(documentedStatsKeys) {
				t.Fatalf("unexpected extra fields: %v", raw)
			}

			for _, key := range []string{"monitors", "rules", "channels", "alerts", "alerts_last_24h", "last_ledger"} {
				if _, ok := raw[key].(float64); !ok {
					t.Fatalf("%s type = %T, want number", key, raw[key])
				}
			}
			if _, ok := raw["last_poll_at"].(string); !ok {
				t.Fatalf("last_poll_at type = %T, want string", raw["last_poll_at"])
			}

			var got store.Stats
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			if got.Monitors != tt.stats.Monitors || got.Rules != tt.stats.Rules ||
				got.Channels != tt.stats.Channels || got.Alerts != tt.stats.Alerts ||
				got.AlertsLast24 != tt.stats.AlertsLast24 || got.LastLedger != tt.stats.LastLedger {
				t.Fatalf("decoded stats = %+v, want %+v", got, tt.stats)
			}
			// encoding/json round-trips a zero time as UTC, not loc==nil.
			if !got.LastPollAt.Equal(tt.stats.LastPollAt) {
				t.Fatalf("last_poll_at = %v, want %v", got.LastPollAt, tt.stats.LastPollAt)
			}
		})
	}
}

func TestStatsEndpointStoreError(t *testing.T) {
	status, ct, raw, body, hdrID := getStats(t, &statsStore{err: errors.New("db down")})
	if status != http.StatusInternalServerError {
		t.Fatalf("GET /stats store error = %d, want 500; body=%s", status, body)
	}
	if ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if got, _ := raw["error"].(string); got != "internal error" {
		t.Fatalf("error = %q, want internal error", raw["error"])
	}
	if got, _ := raw["code"].(string); got != http.StatusText(http.StatusInternalServerError) {
		t.Fatalf("code = %q, want %q", raw["code"], http.StatusText(http.StatusInternalServerError))
	}
	reqID, _ := raw["request_id"].(string)
	if reqID == "" {
		t.Fatalf("request_id empty in envelope: %s", body)
	}
	if hdrID != "" && reqID != hdrID {
		t.Fatalf("request_id %q != X-Request-ID %q", reqID, hdrID)
	}
	if _, ok := raw["monitors"]; ok {
		t.Fatalf("error envelope must not leak a partial stats body: %s", body)
	}
}
