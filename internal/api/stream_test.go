package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/broadcast"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
)

func streamTestServer(t *testing.T, b *broadcast.Broadcaster, heartbeat time.Duration) *httptest.Server {
	t.Helper()
	s := New(&fakeStore{}, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger()).
		WithBroadcaster(b).
		WithStreamHeartbeat(heartbeat)
	return httptest.NewServer(s.Routes())
}

// streamReader pumps the SSE body's lines into a channel, so a test can wait
// for one with a deadline instead of blocking forever on a regression.
func streamReader(res *http.Response) <-chan string {
	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	return lines
}

func awaitLine(t *testing.T, lines <-chan string, timeout time.Duration, match func(string) bool) string {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("stream closed before the expected line arrived")
			}
			if match(line) {
				return line
			}
		case <-deadline:
			t.Fatal("timed out waiting for an SSE line")
		}
	}
}

func waitForSubscribers(t *testing.T, b *broadcast.Broadcaster, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := b.SubscriberCount(); got == want {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("SubscriberCount = %d, want %d", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func openStream(t *testing.T, srv *httptest.Server, query string) *http.Response {
	t.Helper()
	res, err := http.Get(srv.URL + "/alerts/stream" + query)
	if err != nil {
		t.Fatalf("GET /alerts/stream%s: %v", query, err)
	}
	return res
}

// The contract the issue calls out: text/event-stream, and an alert published
// after the client connected reaches it as an `alert` event.
func TestAlertStream_DeliversPublishedAlert(t *testing.T) {
	b := broadcast.New(4)
	srv := streamTestServer(t, b, time.Minute)
	defer srv.Close()

	res := openStream(t, srv, "")
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	lines := streamReader(res)
	// The stream opens with a comment so the client knows it is connected.
	awaitLine(t, lines, time.Second, func(l string) bool { return strings.HasPrefix(l, ":") })
	// Wait for the handler to actually register before publishing, or the
	// alert could be fanned out to nobody.
	waitForSubscribers(t, b, 1)

	b.Publish(broadcast.Alert{
		ID: 7, MonitorID: 1, MonitorName: "ops", RuleID: 3, EventID: "ev-7",
		Payload:   json.RawMessage(`{"event_name":"transfer","contract_id":"C"}`),
		CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
	})

	awaitLine(t, lines, 2*time.Second, func(l string) bool { return l == "event: alert" })
	// `retry: 3000` is skipped over by the matcher above; the data line is
	// the next thing the client parses.
	dataLine := awaitLine(t, lines, 2*time.Second, func(l string) bool { return strings.HasPrefix(l, "data: ") })

	var got broadcast.Alert
	if err := json.Unmarshal([]byte(strings.TrimPrefix(dataLine, "data: ")), &got); err != nil {
		t.Fatalf("data line does not parse: %v (%s)", err, dataLine)
	}
	if got.ID != 7 || got.MonitorID != 1 || got.MonitorName != "ops" || got.RuleID != 3 || got.EventID != "ev-7" {
		t.Fatalf("streamed alert = %+v", got)
	}
	if got.CreatedAt.Unix() != 1_700_000_000 {
		t.Fatalf("created_at = %v, want the published time", got.CreatedAt)
	}
	var payload map[string]any
	if err := json.Unmarshal(got.Payload, &payload); err != nil || payload["event_name"] != "transfer" {
		t.Fatalf("payload = %s err=%v", got.Payload, err)
	}
}

// monitor_id filters server-side: a subscriber on monitor 2 must never be
// woken by monitor 1's alerts, so the first alert it sees is monitor 2's.
func TestAlertStream_FiltersByMonitorServerSide(t *testing.T) {
	b := broadcast.New(4)
	srv := streamTestServer(t, b, time.Minute)
	defer srv.Close()

	res := openStream(t, srv, "?monitor_id=2")
	defer res.Body.Close()

	lines := streamReader(res)
	awaitLine(t, lines, time.Second, func(l string) bool { return strings.HasPrefix(l, ":") })
	waitForSubscribers(t, b, 1)

	b.Publish(broadcast.Alert{ID: 1, MonitorID: 1, EventID: "filtered-out"})
	b.Publish(broadcast.Alert{ID: 2, MonitorID: 2, EventID: "wanted"})

	dataLine := awaitLine(t, lines, 2*time.Second, func(l string) bool { return strings.HasPrefix(l, "data: ") })
	var got broadcast.Alert
	if err := json.Unmarshal([]byte(strings.TrimPrefix(dataLine, "data: ")), &got); err != nil {
		t.Fatalf("data line does not parse: %v (%s)", err, dataLine)
	}
	if got.MonitorID != 2 || got.EventID != "wanted" {
		t.Fatalf("first streamed alert = %+v, want the monitor_id=2 one", got)
	}
}

func TestAlertStream_InvalidMonitorID(t *testing.T) {
	srv := streamTestServer(t, broadcast.New(4), time.Minute)
	defer srv.Close()

	res := openStream(t, srv, "?monitor_id=abc")
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	var env map[string]any
	if err := json.NewDecoder(res.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env["error"] == nil || env["code"] == nil {
		t.Fatalf("400 envelope missing error/code: %v", env)
	}
}

// Idle connections must not be silently reaped: a keep-alive comment arrives
// on the heartbeat tick even when no alert is published.
func TestAlertStream_SendsHeartbeatComments(t *testing.T) {
	srv := streamTestServer(t, broadcast.New(4), 20*time.Millisecond)
	defer srv.Close()

	res := openStream(t, srv, "")
	defer res.Body.Close()

	lines := streamReader(res)
	line := awaitLine(t, lines, 2*time.Second, func(l string) bool { return strings.Contains(l, "keep-alive") })
	if !strings.HasPrefix(line, ":") {
		t.Fatalf("keep-alive line = %q, want an SSE comment", line)
	}
}

// The leak assertion: connect, disconnect, and the subscriber count must
// return to zero. A count that stays at 1 is a goroutine (and its buffered
// channel) retained for the life of the process.
func TestAlertStream_DisconnectRemovesSubscriber(t *testing.T) {
	b := broadcast.New(4)
	srv := streamTestServer(t, b, time.Minute)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/alerts/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /alerts/stream: %v", err)
	}
	lines := streamReader(res)
	awaitLine(t, lines, time.Second, func(l string) bool { return strings.HasPrefix(l, ":") })
	waitForSubscribers(t, b, 1)

	// Disconnecting cancels the request context; the handler must notice and
	// unregister the subscriber.
	cancel()
	res.Body.Close()
	waitForSubscribers(t, b, 0)

	// Publishing after the client is gone must be a no-op, not a panic on a
	// dead subscriber.
	b.Publish(broadcast.Alert{ID: 1, MonitorID: 1})
}
