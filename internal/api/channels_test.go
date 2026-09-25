package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
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

// channelHealthCall records one RecordChannelHealth call, so a test can assert
// what a handler told the store about a channel's health.
type channelHealthCall struct {
	channelID int64
	update    store.ChannelHealthUpdate
}

// healthChannelStore serves one channel and records the health writes and
// channel updates handlers make against it.
type healthChannelStore struct {
	store.Store
	channel store.Channel
	health  []channelHealthCall
	updated []store.Channel
}

func (s *healthChannelStore) ListChannelsPage(context.Context, store.ListFilter) ([]store.Channel, error) {
	return []store.Channel{s.channel}, nil
}

func (s *healthChannelStore) GetChannel(context.Context, int64) (*store.Channel, error) {
	ch := s.channel
	return &ch, nil
}

func (s *healthChannelStore) UpdateChannel(_ context.Context, c *store.Channel) error {
	s.updated = append(s.updated, *c)
	return nil
}

func (s *healthChannelStore) RecordChannelHealth(_ context.Context, channelID int64, u store.ChannelHealthUpdate) error {
	s.health = append(s.health, channelHealthCall{channelID: channelID, update: u})
	return nil
}

// healthServer wires an API server whose "mock" channel type is backed by n.
func healthServer(t *testing.T, st store.Store, n notify.Notifier) *httptest.Server {
	t.Helper()
	f := notify.DefaultFactory()
	f.Register("mock", func(json.RawMessage) (notify.Notifier, error) { return n, nil })
	return httptest.NewServer(New(st, rules.NewRegistry(), f, &fakeRPC{}, discardLogger()).Routes())
}

// A channel that has stopped working has to say so in the listing: reading
// delivery attempts one alert at a time was the only way to find out before.
func TestListChannels_SurfacesHealth(t *testing.T) {
	failedAt := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	st := &healthChannelStore{channel: store.Channel{
		ID: 1, Name: "ops", Type: "webhook", Enabled: false,
		ConsecutiveFailures:          7,
		ConsecutivePermanentFailures: 7,
		LastError:                    "status 401: Unauthorized",
		LastErrorAt:                  &failedAt,
		DisabledAt:                   &failedAt,
	}}

	code, body := getJSON(t, st, "/channels")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	list, _ := body["channels"].([]any)
	if len(list) != 1 {
		t.Fatalf("got %d channels, want 1 (body=%v)", len(list), body)
	}
	ch, _ := list[0].(map[string]any)
	if ch["consecutive_failures"] != float64(7) {
		t.Fatalf("consecutive_failures = %v, want 7", ch["consecutive_failures"])
	}
	if ch["last_error"] != "status 401: Unauthorized" {
		t.Fatalf("last_error = %v", ch["last_error"])
	}
	if ch["disabled_at"] == nil {
		t.Fatalf("an auto-disabled channel must report disabled_at, got %v", ch)
	}
}

// The "send test" button is how an operator checks a channel they have just
// fixed, so its outcome has to reach the health counters — including on the
// way back up, which is what stops a repaired channel being reported as
// broken for the rest of the day.
func TestTestChannel_RecordsHealth(t *testing.T) {
	t.Run("a successful send clears the failure state", func(t *testing.T) {
		st := &healthChannelStore{channel: store.Channel{ID: 4, Name: "ops", Type: "mock", Enabled: true}}
		srv := healthServer(t, st, &scriptedNotifier{})
		defer srv.Close()

		res, err := http.Post(srv.URL+"/channels/4/test", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", res.StatusCode)
		}

		if len(st.health) != 1 {
			t.Fatalf("got %d health updates, want 1", len(st.health))
		}
		got := st.health[0]
		if got.channelID != 4 {
			t.Fatalf("channel_id = %d, want 4", got.channelID)
		}
		if !got.update.Success {
			t.Fatalf("a delivered test must be recorded as a success: %+v", got.update)
		}
	})

	t.Run("a failing send records why, without a disable threshold", func(t *testing.T) {
		st := &healthChannelStore{channel: store.Channel{ID: 4, Name: "ops", Type: "mock", Enabled: true}}
		srv := healthServer(t, st, &scriptedNotifier{err: &notify.HTTPStatusError{StatusCode: 401, Body: "Unauthorized"}})
		defer srv.Close()

		res, err := http.Post(srv.URL+"/channels/4/test", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", res.StatusCode)
		}

		if len(st.health) != 1 {
			t.Fatalf("got %d health updates, want 1", len(st.health))
		}
		got := st.health[0].update
		if got.Success {
			t.Fatal("a failed send must not be recorded as a success")
		}
		if !got.Permanent {
			t.Fatal("a 401 should count as permanent")
		}
		if !strings.Contains(got.Error, "401") {
			t.Fatalf("error = %q, want it to name the status", got.Error)
		}
		if got.DisableAfter != 0 {
			t.Fatalf("a diagnostic click must never carry a disable threshold, got %d", got.DisableAfter)
		}
	})
}

// Re-enabling is the one way back from auto-disable: it goes through the same
// channel update the API already had, and the store clears the counters that
// parked the channel as part of that write.
func TestUpdateChannel_ReenableGoesThroughUpdate(t *testing.T) {
	st := &healthChannelStore{channel: store.Channel{
		ID: 6, Name: "ops", Type: "mock", Config: json.RawMessage(`{}`), Enabled: false,
		ConsecutiveFailures: 3, ConsecutivePermanentFailures: 3,
	}}
	srv := healthServer(t, st, &scriptedNotifier{})
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPatch, srv.URL+"/channels/6", strings.NewReader(`{"enabled":true}`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if len(st.updated) != 1 {
		t.Fatalf("got %d channel updates, want 1", len(st.updated))
	}
	if !st.updated[0].Enabled {
		t.Fatal("PATCH enabled=true must reach the store with the channel on")
	}
}

// A rename must not quietly wipe the evidence of a channel that is still
// failing, so nothing but an explicit re-enable clears the counters.
func TestUpdateChannel_RenameLeavesHealthAlone(t *testing.T) {
	st := &healthChannelStore{channel: store.Channel{
		ID: 6, Name: "ops", Type: "mock", Config: json.RawMessage(`{}`), Enabled: true,
		ConsecutiveFailures: 3, ConsecutivePermanentFailures: 3,
	}}
	srv := healthServer(t, st, &scriptedNotifier{})
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPatch, srv.URL+"/channels/6", strings.NewReader(`{"name":"ops-renamed"}`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if len(st.health) != 0 {
		t.Fatalf("a rename wrote %d health updates, want 0", len(st.health))
	}
}

// The health update must never carry channel config: the notifiers redact
// URLs and tokens before building an error message.
func TestChannelHealthNeverRecordsSecrets(t *testing.T) {
	st := &healthChannelStore{channel: store.Channel{ID: 4, Name: "ops", Type: "mock", Enabled: true}}
	srv := healthServer(t, st, &scriptedNotifier{err: errors.New("status 403: forbidden")})
	defer srv.Close()

	res, err := http.Post(srv.URL+"/channels/4/test", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if len(st.health) != 1 {
		t.Fatalf("got %d health updates, want 1", len(st.health))
	}
	if got := st.health[0].update.Error; strings.Contains(got, "webhook") || strings.Contains(got, "token") {
		t.Fatalf("health recorded something that reads like config: %q", got)
	}
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
