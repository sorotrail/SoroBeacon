package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/sorotrail/sorobeacon/internal/broadcast"
	"github.com/sorotrail/sorobeacon/internal/reqid"
)

// defaultStreamHeartbeat is how often an idle SSE connection emits a comment.
// Fifteen seconds is comfortably inside the default idle timeout of common
// proxies (nginx 60s, many load balancers 30s), so a connection that is merely
// quiet is not reaped mid-watch.
const defaultStreamHeartbeat = 15 * time.Second

// streamRetryHint asks EventSource to wait 3s before reconnecting; the default
// of 1-2s hammers a restarting server.
const streamRetryHint = 3000

// WithBroadcaster shares the process-wide alert fan-out with the SSE endpoint.
// main passes the same Broadcaster it gave the poller, so /alerts/stream
// serves exactly the alerts that were just created. A nil argument is ignored.
func (s *Server) WithBroadcaster(b *broadcast.Broadcaster) *Server {
	if b != nil {
		s.broadcaster = b
	}
	return s
}

// WithStreamHeartbeat overrides the SSE keep-alive interval. Non-positive
// values are ignored so a miswired caller cannot leave idle connections to be
// reaped by a proxy.
func (s *Server) WithStreamHeartbeat(d time.Duration) *Server {
	if d > 0 {
		s.streamHeartbeat = d
	}
	return s
}

// streamAlerts serves GET /alerts/stream: a Server-Sent Events feed of alerts
// as they are created. An optional monitor_id filters server-side, so a client
// watching one monitor is not woken for every other monitor's alerts.
//
// SSE rather than WebSockets: the traffic is one-directional, and SSE survives
// proxies (and reconnects on its own) with far less configuration. The handler
// is a plain loop over the subscriber's buffered channel, so a slow or dead
// client costs at most a dropped event — never a blocked alert creation.
func (s *Server) streamAlerts(w http.ResponseWriter, r *http.Request) {
	var monitorID int64
	if v := r.URL.Query().Get("monitor_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeErr(w, r, http.StatusBadRequest, "invalid monitor_id")
			return
		}
		monitorID = id
	}

	// Flushing is what makes this a stream rather than one buffered response.
	// Every http.ResponseWriter in net/http implements Flusher; a test double
	// or an unusual middleware might not.
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, r, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	sub := s.broadcaster.Subscribe(monitorID)
	// This defer is what keeps SubscriberCount at zero: every exit from the
	// loop below — client gone, write error, shutdown — unregisters the
	// subscriber, so no goroutine or buffered channel is left behind.
	defer sub.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	// A stream is live data; a cache that stored it would replay stale
	// alerts onto the next client.
	w.Header().Set("Cache-Control", "no-store")
	// Ask proxies (nginx and friends) not to buffer the body, which would
	// otherwise hold events until the buffer fills. Unknown to others.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// A comment opens the stream immediately, letting the client fire onopen
	// without waiting for the first alert or heartbeat.
	if _, err := io.WriteString(w, ": connected\n\n"); err != nil {
		return
	}
	flusher.Flush()
	if _, err := fmt.Fprintf(w, "retry: %d\n\n", streamRetryHint); err != nil {
		return
	}
	flusher.Flush()

	beat := time.NewTicker(s.streamHeartbeat)
	defer beat.Stop()

	for {
		select {
		// The request context is cancelled when the client disconnects or
		// the server shuts down; returning runs the deferred Close.
		case <-r.Context().Done():
			return
		case <-beat.C:
			// A comment line is a valid SSE event with no data: it keeps
			// the connection warm without reaching the client's handler.
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case a := <-sub.C:
			data, err := json.Marshal(a)
			if err != nil {
				// A payload that will not marshal is a bug in what was
				// stored, not a reason to drop the whole stream.
				s.log.Error("marshal stream alert",
					"request_id", reqid.From(r), "alert_id", a.ID, "err", err)
				continue
			}
			// One line per field: a raw newline in data would truncate the
			// event. json.Marshal never emits one.
			if _, err := fmt.Fprintf(w, "event: alert\ndata: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
