package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// channelsStore records the enabledOnly flag ListChannels was called with,
// and returns a fixed set of channels regardless of it — the filtering
// itself is the store's job (tested against Postgres); the handler's job
// is wiring the query param through correctly.
type channelsStore struct {
	store.Store
	gotEnabledOnly *bool
	gotType        string
}

func (c *channelsStore) ListChannelsPage(ctx context.Context, f store.ListFilter) ([]store.Channel, error) {
	c.gotEnabledOnly = &f.EnabledOnly
	c.gotType = f.Type
	return []store.Channel{{ID: 1, Name: "ops", Type: "webhook", Enabled: true}}, nil
}

func TestListChannels_EnabledParamWiring(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  bool
	}{
		{"no param", "", false},
		{"enabled=true", "?enabled=true", true},
		{"enabled=false leaves default behavior unchanged", "?enabled=false", false},
		{"enabled=garbage leaves default behavior unchanged", "?enabled=garbage", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := &channelsStore{}
			srv := httptest.NewServer(newProbeServer(cs, &fakeRPC{}))
			defer srv.Close()

			res, err := http.Get(srv.URL + "/channels" + tt.query)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("GET /channels%s = %d, want 200", tt.query, res.StatusCode)
			}
			if cs.gotEnabledOnly == nil {
				t.Fatal("ListChannels was not called")
			}
			if *cs.gotEnabledOnly != tt.want {
				t.Fatalf("enabledOnly = %v, want %v", *cs.gotEnabledOnly, tt.want)
			}

			var body map[string]any
			if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			list, _ := body["channels"].([]any)
			if len(list) != 1 {
				t.Fatalf("got %d channels, want 1 (body=%v)", len(list), body)
			}
		})
	}
}

// TestListChannels_TypeFilter covers the ?type= filter. The filter itself is
// applied in SQL; what the handler owns is validating the value and passing
// it through, so an unknown type is a 400 rather than an empty list that
// reads like "no channels configured".
func TestListChannels_TypeFilter(t *testing.T) {
	t.Run("passes a known type through", func(t *testing.T) {
		cs := &channelsStore{}
		code, _ := getJSON(t, cs, "/channels?type=webhook")
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		if cs.gotType != "webhook" {
			t.Fatalf("type = %q, want %q", cs.gotType, "webhook")
		}
	})

	t.Run("rejects an unknown type", func(t *testing.T) {
		cs := &channelsStore{}
		code, _ := getJSON(t, cs, "/channels?type=carrier-pigeon")
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", code)
		}
	})

	t.Run("no type means no filter", func(t *testing.T) {
		cs := &channelsStore{}
		code, _ := getJSON(t, cs, "/channels")
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		if cs.gotType != "" {
			t.Fatalf("type = %q, want empty", cs.gotType)
		}
	})
}

type channelStatsStore struct {
	store.Store
	stats    store.ChannelStats
	statsErr error
	gotID    int64
	gotSince time.Time
	called   bool
}

func (s *channelStatsStore) GetChannel(_ context.Context, id int64) (*store.Channel, error) {
	if id != 7 {
		return nil, store.ErrNotFound
	}
	return &store.Channel{ID: 7, Name: "ops", Type: "webhook", Enabled: true}, nil
}

func (s *channelStatsStore) ChannelStats(_ context.Context, id int64, since time.Time) (store.ChannelStats, error) {
	s.called = true
	s.gotID = id
	s.gotSince = since
	if s.statsErr != nil {
		return store.ChannelStats{}, s.statsErr
	}
	out := s.stats
	out.ChannelID = id
	return out, nil
}

func TestChannelStats_EmptyWindowUsesDefaultAndOmitsRate(t *testing.T) {
	st := &channelStatsStore{stats: store.ChannelStats{TotalAttempts: 0, Successes: 0, Failures: 0}}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	before := time.Now()
	res, err := http.Get(srv.URL + "/channels/7/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	after := time.Now()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d, want 200 body=%s", res.StatusCode, body)
	}
	if !st.called || st.gotID != 7 {
		t.Fatalf("ChannelStats called=%v id=%d", st.called, st.gotID)
	}
	wantSince := after.Add(-store.DefaultChannelStatsWindow)
	if st.gotSince.After(after.Add(-store.DefaultChannelStatsWindow+time.Second)) || st.gotSince.Before(before.Add(-store.DefaultChannelStatsWindow-time.Second)) {
		t.Fatalf("since = %v, want ~%v", st.gotSince, wantSince)
	}
	raw, _ := io.ReadAll(res.Body)
	if strings.Contains(string(raw), "success_rate") {
		t.Fatalf("success_rate must be omitted when there are no attempts: %s", raw)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body["window"] != "24h" {
		t.Fatalf("window = %v, want 24h", body["window"])
	}
	if body["total_attempts"] != float64(0) {
		t.Fatalf("total_attempts = %v, want 0", body["total_attempts"])
	}
	if body["last_success"] != nil || body["last_failure"] != nil {
		t.Fatalf("timestamps must be null with no attempts: %s", raw)
	}
}

func TestChannelStats_WindowQueryAndRate(t *testing.T) {
	rate := 0.5
	now := time.Now().UTC().Truncate(time.Second)
	st := &channelStatsStore{stats: store.ChannelStats{
		TotalAttempts: 4, Successes: 2, Failures: 2, SuccessRate: &rate,
		LastSuccess: &now, LastFailure: &now,
	}}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/channels/7/stats?window=1h")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d body=%s", res.StatusCode, body)
	}
	if time.Until(st.gotSince.Add(time.Hour)) > 2*time.Second {
		t.Fatalf("window 1h since = %v", st.gotSince)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["success_rate"] != 0.5 {
		t.Fatalf("success_rate = %v, want 0.5", body["success_rate"])
	}
	if body["window"] != "1h" {
		t.Fatalf("window = %v, want 1h", body["window"])
	}
}

func TestChannelStats_InvalidWindow(t *testing.T) {
	st := &channelStatsStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	for _, q := range []string{"?window=nope", "?window=0s", "?window=-1h"} {
		res, err := http.Get(srv.URL + "/channels/7/stats" + q)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", q, res.StatusCode)
		}
		res.Body.Close()
		if st.called {
			t.Fatalf("%s must not hit the store", q)
		}
	}
}

func TestChannelStats_UnknownChannel(t *testing.T) {
	st := &channelStatsStore{statsErr: store.ErrNotFound}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/channels/99/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
}

func TestChannelStats_InvalidID(t *testing.T) {
	st := &channelStatsStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/channels/nope/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if st.called {
		t.Fatal("store must not be called for invalid id")
	}
}
