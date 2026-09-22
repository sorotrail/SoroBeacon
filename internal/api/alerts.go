package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// listAlerts serves GET /alerts with query filters:
// monitor_id, rule_id, contract_id, from, to (RFC 3339), sort
// (created_at_desc default, created_at_asc), limit, cursor (last seen
// alert id; comparison follows sort).
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
	if v := q.Get("rule_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeErr(w, r, http.StatusBadRequest, "invalid rule_id")
			return
		}
		f.RuleID = id
	}
	f.ContractID = q.Get("contract_id")
	if v := q.Get("sort"); v != "" {
		switch v {
		case "created_at_desc", "created_at_asc":
			f.Sort = v
		default:
			writeErr(w, r, http.StatusBadRequest, "invalid sort")
			return
		}
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

// retryDelivery serves POST /alerts/{id}/deliveries/{channelID}/retry.
// It records a new attempt rather than mutating the failed one, rejects a
// channel that already succeeded (409), and bounds repeats with a cooldown
// so the dashboard button cannot spam the destination.
func (s *Server) retryDelivery(w http.ResponseWriter, r *http.Request) {
	alertID, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	channelID, err := pathID(r, "channelID")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid channelID")
		return
	}

	alert, err := s.store.GetAlert(r.Context(), alertID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ch, err := s.store.GetChannel(r.Context(), channelID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	attempts, err := s.store.ListDeliveryAttempts(r.Context(), alertID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := notify.GateRetry(attempts, channelID, *ch, time.Now(), notify.DefaultRetryCooldown); err != nil {
		writeRetryGate(w, r, err)
		return
	}

	na := notifyAlertFromStore(r.Context(), s.store, *alert)
	d := notify.NewDispatcher(s.store, s.factory, s.log)
	attempt := d.Retry(r.Context(), na, *ch)
	writeJSON(w, http.StatusOK, attempt)
}

func writeRetryGate(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, notify.ErrAlreadySucceeded):
		writeErr(w, r, http.StatusConflict, err.Error())
	case errors.Is(err, notify.ErrChannelDisabled):
		writeErr(w, r, http.StatusBadRequest, err.Error())
	case errors.Is(err, notify.ErrNoAttempt):
		writeErr(w, r, http.StatusNotFound, err.Error())
	case errors.Is(err, notify.ErrRetryCooldown):
		writeErr(w, r, http.StatusTooManyRequests, err.Error())
	default:
		writeErr(w, r, http.StatusBadRequest, err.Error())
	}
}

func notifyAlertFromStore(ctx context.Context, st store.Store, a store.Alert) notify.Alert {
	na := notify.Alert{
		ID:        a.ID,
		MonitorID: a.MonitorID,
		RuleID:    a.RuleID,
		EventID:   a.EventID,
		Payload:   a.Payload,
		CreatedAt: a.CreatedAt,
	}
	var p struct {
		ContractID string `json:"contract_id"`
		EventName  string `json:"event_name"`
		Ledger     uint32 `json:"ledger"`
		TxHash     string `json:"tx_hash"`
	}
	_ = json.Unmarshal(a.Payload, &p)
	na.ContractID = p.ContractID
	na.EventName = p.EventName
	na.Ledger = p.Ledger
	na.TxHash = p.TxHash
	if m, err := st.GetMonitor(ctx, a.MonitorID); err == nil && m != nil {
		na.MonitorName = m.Name
	}
	if rule, err := st.GetRule(ctx, a.RuleID); err == nil && rule != nil {
		na.RuleType = rule.Type
	}
	return na
}
