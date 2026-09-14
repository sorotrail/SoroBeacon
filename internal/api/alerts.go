package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// listAlerts serves GET /alerts with query filters:
// monitor_id, from, to (RFC 3339), limit, cursor (last seen alert id).
func (s *Server) listAlerts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var f store.AlertFilter

	if v := q.Get("monitor_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeErr(w, r, http.StatusBadRequest, "invalid monitor_id")
			return
		}
		f.MonitorID = id
	}
	for name, dst := range map[string]*time.Time{"from": &f.From, "to": &f.To} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				writeErr(w, r, http.StatusBadRequest, "invalid "+name+" (want RFC 3339)")
				return
			}
			*dst = t
		}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeErr(w, r, http.StatusBadRequest, "invalid limit")
			return
		}
		f.Limit = n
	}
	if v := q.Get("cursor"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeErr(w, r, http.StatusBadRequest, "invalid cursor")
			return
		}
		f.AfterID = id
	}

	alerts, err := s.store.ListAlerts(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if alerts == nil {
		alerts = []store.Alert{}
	}
	// next_cursor is only meaningful when the page came back full: a short
	// page means there's nothing older to fetch, so setting it would just
	// cost the client one wasted round trip to learn that. "Full" has to
	// account for store.Postgres.ListAlerts' own default/max clamping
	// (f.Limit <= 0 or > 500 becomes 50), not the raw f.Limit the caller
	// passed — mirroring the heuristic internal/web/web.go's alerts page
	// already uses, where Limit is always pre-set to a sane value.
	effectiveLimit := f.Limit
	if effectiveLimit <= 0 || effectiveLimit > 500 {
		effectiveLimit = 50
	}
	next := ""
	if len(alerts) == effectiveLimit {
		next = strconv.FormatInt(alerts[len(alerts)-1].ID, 10)
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": alerts, "next_cursor": next})
}

// listDeliveries serves GET /alerts/{id}/deliveries.
func (s *Server) listDeliveries(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	list, err := s.store.ListDeliveryAttempts(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if list == nil {
		list = []store.DeliveryAttempt{}
	}
	writeJSON(w, http.StatusOK, list)
}
