package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// healthStore serves the channels page from one channel and records the writes
// the page's buttons make.
type healthStore struct {
	emptyStore
	channel store.Channel
	updated []store.Channel
	health  []store.ChannelHealthUpdate
}

func (s *healthStore) ListChannelsPage(context.Context, store.ListFilter) ([]store.Channel, error) {
	return []store.Channel{s.channel}, nil
}

func (s *healthStore) GetChannel(context.Context, int64) (*store.Channel, error) {
	ch := s.channel
	return &ch, nil
}

func (s *healthStore) UpdateChannel(_ context.Context, c *store.Channel) error {
	s.updated = append(s.updated, *c)
	return nil
}

func (s *healthStore) RecordChannelHealth(_ context.Context, _ int64, u store.ChannelHealthUpdate) error {
	s.health = append(s.health, u)
	return nil
}

// scriptedNotifier returns the outcome a test wants from a "Send test" click.
type scriptedNotifier struct{ err error }

func (n scriptedNotifier) Send(context.Context, notify.Alert) error { return n.err }

func healthWebServer(t *testing.T, st store.Store, notifier notify.Notifier) *httptest.Server {
	t.Helper()
	f := notify.DefaultFactory()
	f.Register("mock", func(json.RawMessage) (notify.Notifier, error) { return notifier, nil })
	s, err := New(st, rules.NewRegistry(), f, slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return httptest.NewServer(s.Routes())
}

func getPage(t *testing.T, url string) string {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", url, res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// An auto-disabled channel is the one row that means alerts have stopped
// arriving, so it has to be unmistakable and it has to say why.
func TestChannelsPageShowsAutoDisabledReason(t *testing.T) {
	at := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	srv := healthWebServer(t, &healthStore{channel: store.Channel{
		ID: 3, Name: "ops-slack", Type: "slack", Enabled: false, CreatedAt: at,
		ConsecutiveFailures:          3,
		ConsecutivePermanentFailures: 3,
		LastError:                    "status 401: Unauthorized",
		LastErrorAt:                  &at,
		DisabledAt:                   &at,
	}}, scriptedNotifier{})
	defer srv.Close()

	html := getPage(t, srv.URL+"/channels")

	for _, want := range []string{
		`class="attention"`, // the row is visually distinct
		"auto-disabled",
		"3 consecutive failures",
		"status 401: Unauthorized",
		"Re-enable it to start delivering again",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("auto-disabled channel page is missing %q:\n%s", want, html)
		}
	}
}

// A channel that is still on but failing is worth warning about, without the
// alarm that auto-disable deserves.
func TestChannelsPageShowsFailingChannel(t *testing.T) {
	srv := healthWebServer(t, &healthStore{channel: store.Channel{
		ID: 3, Name: "ops-discord", Type: "discord", Enabled: true,
		ConsecutiveFailures: 2,
		LastError:           "status 503: Service Unavailable",
	}}, scriptedNotifier{})
	defer srv.Close()

	html := getPage(t, srv.URL+"/channels")

	if strings.Contains(html, `class="attention"`) {
		t.Fatalf("a channel that is still enabled must not look auto-disabled:\n%s", html)
	}
	for _, want := range []string{"failing (2)", "status 503: Service Unavailable"} {
		if !strings.Contains(html, want) {
			t.Fatalf("failing channel page is missing %q:\n%s", want, html)
		}
	}
}

func TestChannelsPageOffersEnableToggle(t *testing.T) {
	t.Run("a disabled channel offers Enable", func(t *testing.T) {
		srv := healthWebServer(t, &healthStore{channel: store.Channel{ID: 3, Name: "ops", Type: "webhook", Enabled: false}}, scriptedNotifier{})
		defer srv.Close()

		html := getPage(t, srv.URL+"/channels")
		if !strings.Contains(html, `action="/channels/3/toggle"`) {
			t.Fatalf("no toggle form for the channel:\n%s", html)
		}
		if !strings.Contains(html, ">Enable<") {
			t.Fatalf("a disabled channel must offer Enable:\n%s", html)
		}
	})

	t.Run("an enabled channel offers Disable", func(t *testing.T) {
		srv := healthWebServer(t, &healthStore{channel: store.Channel{ID: 3, Name: "ops", Type: "webhook", Enabled: true}}, scriptedNotifier{})
		defer srv.Close()

		html := getPage(t, srv.URL+"/channels")
		if !strings.Contains(html, ">Disable<") {
			t.Fatalf("an enabled channel must offer Disable:\n%s", html)
		}
	})
}

// Re-enabling is explicit, and it goes through the store write that clears the
// counters — otherwise the channel would re-disable on its very next failure.
func TestToggleChannelReenablesThroughUpdate(t *testing.T) {
	st := &healthStore{channel: store.Channel{
		ID: 3, Name: "ops", Type: "webhook", Enabled: false,
		ConsecutiveFailures: 3, ConsecutivePermanentFailures: 3,
	}}
	srv := healthWebServer(t, st, scriptedNotifier{})
	defer srv.Close()

	res, err := http.Post(srv.URL+"/channels/3/toggle", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if len(st.updated) != 1 {
		t.Fatalf("got %d channel updates, want 1", len(st.updated))
	}
	if !st.updated[0].Enabled {
		t.Fatal("toggling a disabled channel must turn it on")
	}
}

// The dashboard's own test button has to move the health state, or a channel an
// operator has just fixed keeps reading as broken.
func TestWebTestChannelRecordsHealth(t *testing.T) {
	t.Run("success clears it", func(t *testing.T) {
		st := &healthStore{channel: store.Channel{ID: 3, Name: "ops", Type: "mock", Enabled: true}}
		srv := healthWebServer(t, st, scriptedNotifier{})
		defer srv.Close()

		res, err := http.Post(srv.URL+"/channels/3/test", "application/x-www-form-urlencoded", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		if len(st.health) != 1 {
			t.Fatalf("got %d health updates, want 1", len(st.health))
		}
		if !st.health[0].Success {
			t.Fatalf("a delivered test must clear the channel's failure state: %+v", st.health[0])
		}
	})

	t.Run("failure records it, without a disable threshold", func(t *testing.T) {
		st := &healthStore{channel: store.Channel{ID: 3, Name: "ops", Type: "mock", Enabled: true}}
		srv := healthWebServer(t, st, scriptedNotifier{err: &notify.HTTPStatusError{StatusCode: 404, Body: "Not Found"}})
		defer srv.Close()

		res, err := http.Post(srv.URL+"/channels/3/test", "application/x-www-form-urlencoded", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		if len(st.health) != 1 {
			t.Fatalf("got %d health updates, want 1", len(st.health))
		}
		got := st.health[0]
		if got.Success || !got.Permanent {
			t.Fatalf("a 404 test must be recorded as a permanent failure: %+v", got)
		}
		if got.DisableAfter != 0 {
			t.Fatalf("a diagnostic click must not carry a disable threshold, got %d", got.DisableAfter)
		}
	})
}
