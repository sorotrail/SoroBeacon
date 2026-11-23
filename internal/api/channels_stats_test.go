package api

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// channelStatsStore captures the arguments the stats handler passes to the
// store and answers with a scripted result, so every assertion below is about
// the handler: the store side has its own integration tests.
type channelStatsStore struct {
	store.Store
	stats    store.ChannelDeliveryStats
	err      error
	calls    int
	gotID    int64
	gotSince time.Time
}

func (s *channelStatsStore) GetChannelDeliveryStats(_ context.Context, id int64, since time.Time) (store.ChannelDeliveryStats, error) {
	s.calls++
	s.gotID, s.gotSince = id, since
	return s.stats, s.err
}

func (s *channelStatsStore) GetChannelAckReconciliation(_ context.Context, _ int64) (store.ChannelAckReconciliation, error) {
	return store.ChannelAckReconciliation{}, nil
}

func getChannelStats(t *testing.T, st *channelStatsStore, path string) (int, map[string]any, string) {
	t.Helper()
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if res.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return res.StatusCode, body, string(raw)
}

// A. Omitted window aggregates the last 24 hours and says so.
func TestChannelStatsDefaultWindowIs24h(t *testing.T) {
	st := &channelStatsStore{stats: store.ChannelDeliveryStats{ChannelID: 7}}
	before := time.Now()
	code, body, _ := getChannelStats(t, st, "/channels/7/stats")
	after := time.Now()

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if st.calls != 1 || st.gotID != 7 {
		t.Fatalf("store got (%d, calls=%d), want channel 7 once", st.gotID, st.calls)
	}
	wantSince := before.Add(-24 * time.Hour)
	if st.gotSince.Before(wantSince.Add(-5*time.Second)) || st.gotSince.After(after.Add(-24*time.Hour).Add(5*time.Second)) {
		t.Fatalf("since = %v, want ~24h before now", st.gotSince)
	}
	if st.gotSince.Location() != time.UTC {
		t.Fatalf("since location = %v, want UTC", st.gotSince.Location())
	}
	if body["window"] != "24h" {
		t.Fatalf("window = %v, want the normalized default \"24h\"", body["window"])
	}
}

// B. A valid custom window reaches the store as the correct duration.
func TestChannelStatsCustomWindowReachesStore(t *testing.T) {
	st := &channelStatsStore{stats: store.ChannelDeliveryStats{ChannelID: 7}}
	before := time.Now()
	code, body, _ := getChannelStats(t, st, "/channels/7/stats?window=90m")
	after := time.Now()

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	wantSince := before.Add(-90 * time.Minute)
	if st.gotSince.Before(wantSince.Add(-5*time.Second)) || st.gotSince.After(after.Add(-90*time.Minute).Add(5*time.Second)) {
		t.Fatalf("since = %v, want ~90m before now", st.gotSince)
	}
	if body["window"] != "90m" {
		t.Fatalf("window = %v, want the echoed \"90m\"", body["window"])
	}
}

// C. Malformed, zero, negative and excessive windows are rejected before the
// store is consulted, so a bad value never becomes a database scan.
func TestChannelStatsRejectsInvalidWindow(t *testing.T) {
	for _, q := range []string{"nonsense", "24", "0s", "-1h", "0", "5000h", "2161h"} {
		t.Run(q, func(t *testing.T) {
			st := &channelStatsStore{}
			code, _, _ := getChannelStats(t, st, "/channels/7/stats?window="+q)
			if code != http.StatusBadRequest {
				t.Fatalf("window=%q status = %d, want 400", q, code)
			}
			if st.calls != 0 {
				t.Fatalf("window=%q reached the store %d times, want 0", q, st.calls)
			}
		})
	}
}

// The cap is inclusive: exactly 90 days is the largest accepted window.
func TestChannelStatsAcceptsMaxWindow(t *testing.T) {
	st := &channelStatsStore{}
	code, _, _ := getChannelStats(t, st, "/channels/7/stats?window=2160h")
	if code != http.StatusOK {
		t.Fatalf("status = %d for the documented maximum window, want 200", code)
	}
}

// D. Counts, rate and timestamps serialise as documented.
func TestChannelStatsServesCountsAndTimestamps(t *testing.T) {
	okAt := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	failAt := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	rate := 0.75
	st := &channelStatsStore{stats: store.ChannelDeliveryStats{
		ChannelID: 7, Total: 40, Succeeded: 30, Failed: 10,
		SuccessRate: &rate, LastSuccessAt: &okAt, LastFailureAt: &failAt,
	}}
	code, body, _ := getChannelStats(t, st, "/channels/7/stats")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	for key, want := range map[string]any{
		"channel_id":      float64(7),
		"total":           float64(40),
		"succeeded":       float64(30),
		"failed":          float64(10),
		"success_rate":    0.75,
		"last_success_at": "2026-09-01T10:00:00Z",
		"last_failure_at": "2026-09-01T09:00:00Z",
		"window":          "24h",
	} {
		if got := body[key]; got != want {
			t.Errorf("body[%q] = %v, want %v", key, got, want)
		}
	}
}

// E. A channel with no attempts is a 200 with zeroes, null timestamps and no
// success rate at all — an undefined rate must not become 0%.
func TestChannelStatsNoAttemptsOmitsRate(t *testing.T) {
	st := &channelStatsStore{stats: store.ChannelDeliveryStats{ChannelID: 7}}
	code, body, raw := getChannelStats(t, st, "/channels/7/stats")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	for _, key := range []string{"total", "succeeded", "failed"} {
		if got := body[key]; got != float64(0) {
			t.Errorf("body[%q] = %v, want explicit 0", key, got)
		}
	}
	for _, key := range []string{"success_rate", "last_success_at", "last_failure_at"} {
		if _, ok := body[key]; ok {
			t.Errorf("body[%q] present for a channel with no attempts; must be omitted", key)
		}
	}
	if !json.Valid([]byte(raw)) {
		t.Fatalf("response is not valid JSON: %s", raw)
	}
}

// F. An unknown channel follows the API's not-found behaviour.
func TestChannelStatsUnknownChannelIs404(t *testing.T) {
	st := &channelStatsStore{err: store.ErrNotFound}
	code, _, _ := getChannelStats(t, st, "/channels/999/stats")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

// A malformed id never reaches the store.
func TestChannelStatsRejectsMalformedID(t *testing.T) {
	st := &channelStatsStore{}
	code, _, _ := getChannelStats(t, st, "/channels/not-a-number/stats")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if st.calls != 0 {
		t.Fatalf("store called %d times for a malformed id, want 0", st.calls)
	}
}

// G. The route carries the stats read scope, and the response body can never
// contain channel configuration: the handler serialises only the store's
// aggregate, whose fields are counts, a rate and timestamps.
func TestChannelStatsScopeAndNoConfigLeak(t *testing.T) {
	scopes, ok := requiredScopes(http.MethodGet, "/channels/7/stats")
	if !ok {
		t.Fatal("GET /channels/{id}/stats is not in the scope table")
	}
	if len(scopes) != 1 || scopes[0] != auth.ScopeStatsRead {
		t.Fatalf("scopes = %v, want [stats:read]", scopes)
	}

	rate := 0.5
	st := &channelStatsStore{stats: store.ChannelDeliveryStats{
		ChannelID: 7, Total: 2, Succeeded: 1, Failed: 1, SuccessRate: &rate,
	}}
	_, _, raw := getChannelStats(t, st, "/channels/7/stats")
	for _, forbidden := range []string{"config", "webhook_url", "token", "password"} {
		if strings.Contains(strings.ToLower(raw), forbidden) {
			t.Fatalf("response leaks %q: %s", forbidden, raw)
		}
	}
	// The rate survives rounding exactly as the store derived it.
	var parsed struct {
		SuccessRate float64 `json:"success_rate"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatal(err)
	}
	if math.Abs(parsed.SuccessRate-0.5) > 1e-9 {
		t.Fatalf("success_rate = %v, want 0.5", parsed.SuccessRate)
	}
}
