package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// statsHealthStore serves the channels page from one channel plus scripted
// delivery statistics, so each test below pins one state of the new delivery
// column without a database.
type statsHealthStore struct {
	healthStore
	stats map[int64]store.ChannelDeliveryStats
	acks  map[int64]store.ChannelAckReconciliation
	err   error
}

func (s *statsHealthStore) GetChannelDeliveryStats(_ context.Context, id int64, _ time.Time) (store.ChannelDeliveryStats, error) {
	if s.err != nil {
		return store.ChannelDeliveryStats{}, s.err
	}
	return s.stats[id], nil
}

func (s *statsHealthStore) ListChannelAckReconciliations(_ context.Context) (map[int64]store.ChannelAckReconciliation, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.acks, nil
}

// A healthy channel shows its rate as a percentage with an explicit "ok" word,
// so the state does not rest on colour alone.
func TestChannelsPageShowsHealthyDeliveryRate(t *testing.T) {
	rate := 1.0
	srv := healthWebServer(t, &statsHealthStore{
		healthStore: healthStore{channel: store.Channel{ID: 3, Name: "ops", Type: "webhook", Enabled: true}},
		stats: map[int64]store.ChannelDeliveryStats{
			3: {ChannelID: 3, Total: 2, Succeeded: 2, Failed: 0, SuccessRate: &rate},
		},
	}, scriptedNotifier{})
	defer srv.Close()

	html := getPage(t, srv.URL+"/channels")

	for _, want := range []string{"Delivery (last 24h)", "100% ok", "2 of 2 delivered"} {
		if !strings.Contains(html, want) {
			t.Fatalf("healthy channel page is missing %q:\n%s", want, html)
		}
	}
	if strings.Contains(html, "degraded") {
		t.Fatalf("a fully healthy channel must not read as degraded:\n%s", html)
	}
}

// A failing channel is easy to identify: the rate is marked degraded in words
// and the last failure inside the window is shown.
func TestChannelsPageShowsDegradedDeliveryRate(t *testing.T) {
	at := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	rate := 2.0 / 3.0
	srv := healthWebServer(t, &statsHealthStore{
		healthStore: healthStore{channel: store.Channel{ID: 3, Name: "ops", Type: "webhook", Enabled: true}},
		stats: map[int64]store.ChannelDeliveryStats{
			3: {ChannelID: 3, Total: 3, Succeeded: 2, Failed: 1, SuccessRate: &rate, LastFailureAt: &at},
		},
	}, scriptedNotifier{})
	defer srv.Close()

	html := getPage(t, srv.URL+"/channels")

	// Two thirds rounds to 66.7% — the same formatting the API's
	// SuccessPercent promises, so the page and the endpoint agree.
	for _, want := range []string{"66.7% degraded", "2 of 3 delivered", "Last failure"} {
		if !strings.Contains(html, want) {
			t.Fatalf("degraded channel page is missing %q:\n%s", want, html)
		}
	}
}

// A channel with no attempts in the window says so in words. It must never
// show 0%, which reads as "every delivery failed".
func TestChannelsPageShowsNoAttemptsNotZeroPercent(t *testing.T) {
	srv := healthWebServer(t, &statsHealthStore{
		healthStore: healthStore{channel: store.Channel{ID: 3, Name: "ops", Type: "webhook", Enabled: true}},
		stats:       map[int64]store.ChannelDeliveryStats{3: {ChannelID: 3}},
	}, scriptedNotifier{})
	defer srv.Close()

	html := getPage(t, srv.URL+"/channels")

	if !strings.Contains(html, "No attempts") {
		t.Fatalf("a channel with no attempts must say so:\n%s", html)
	}
	// The success-rate pill must be absent entirely: neither "0% degraded"
	// (which reads as "everything failed") nor any "N% ok" substitute.
	for _, unwanted := range []string{"degraded", "% ok", "delivered."} {
		if strings.Contains(html, unwanted) {
			t.Fatalf("no attempts must not be rendered as a rate pill (%q present):\n%s", unwanted, html)
		}
	}
}

// A statistics failure fails the page: a half-rendered health column that
// silently shows nothing would be mistaken for "no attempts".
func TestChannelsPageFailsWhenStatsStoreFails(t *testing.T) {
	srv := healthWebServer(t, &statsHealthStore{
		healthStore: healthStore{channel: store.Channel{ID: 3, Name: "ops", Type: "webhook", Enabled: true}},
		err:         errors.New("database exploded"),
	}, scriptedNotifier{})
	defer srv.Close()

	res, err := http.Get(srv.URL + "/channels")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("GET /channels with a failing stats store = %d, want 500", res.StatusCode)
	}
}

// A channel flagged as unacknowledged displays the 'unacknowledged' pill and details.
func TestChannelsPageShowsUnacknowledgedFlaggedBadge(t *testing.T) {
	rate := 1.0
	srv := healthWebServer(t, &statsHealthStore{
		healthStore: healthStore{channel: store.Channel{ID: 3, Name: "ops-silent", Type: "slack", Enabled: true}},
		stats: map[int64]store.ChannelDeliveryStats{
			3: {ChannelID: 3, Total: 5, Succeeded: 5, Failed: 0, SuccessRate: &rate},
		},
		acks: map[int64]store.ChannelAckReconciliation{
			3: {ChannelID: 3, DeliveredTotal: 5, AcknowledgedTotal: 0, UnacknowledgedTotal: 5, UnackRate: &rate, Flagged: true},
		},
	}, scriptedNotifier{})
	defer srv.Close()

	html := getPage(t, srv.URL+"/channels")

	for _, want := range []string{"unacknowledged", "Delivered 5 alert(s) without human acknowledgement", "HTTP acceptance only"} {
		if !strings.Contains(html, want) {
			t.Fatalf("unacknowledged channel page is missing %q:\n%s", want, html)
		}
	}
}

// Channel types display their receipt capability in the health column.
func TestChannelsPageShowsReceiptCapabilities(t *testing.T) {
	srv := healthWebServer(t, &statsHealthStore{
		healthStore: healthStore{channel: store.Channel{ID: 5, Name: "pager", Type: "pagerduty", Enabled: true}},
	}, scriptedNotifier{})
	defer srv.Close()

	html := getPage(t, srv.URL+"/channels")

	if !strings.Contains(html, "native paging acknowledgement") {
		t.Fatalf("pagerduty channel must display native paging acknowledgement:\n%s", html)
	}
}
