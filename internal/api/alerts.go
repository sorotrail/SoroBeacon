package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/reqid"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// parseAlertFilter reads the shared alert listing query params
// (monitor_id, rule_id, contract_id, from, to as RFC 3339, sort, limit,
// cursor). GET /alerts and GET /alerts.csv both call it so the two
// endpoints cannot drift on which params exist or how bad values are
// reported. On failure it has already written the error envelope.
func parseAlertFilter(w http.ResponseWriter, r *http.Request) (store.AlertFilter, bool) {
	q := r.URL.Query()
	var f store.AlertFilter

	if v := q.Get("monitor_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeErr(w, r, http.StatusBadRequest, "invalid monitor_id")
			return f, false
		}
		f.MonitorID = id
	}
	if v := q.Get("rule_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeErr(w, r, http.StatusBadRequest, "invalid rule_id")
			return f, false
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
			return f, false
		}
	}
	for name, dst := range map[string]*time.Time{"from": &f.From, "to": &f.To} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				writeErr(w, r, http.StatusBadRequest, "invalid "+name+" (want RFC 3339)")
				return f, false
			}
			*dst = t
		}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeErr(w, r, http.StatusBadRequest, "invalid limit")
			return f, false
		}
		f.Limit = n
	}
	if v := q.Get("cursor"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeErr(w, r, http.StatusBadRequest, "invalid cursor")
			return f, false
		}
		f.AfterID = id
	}
	if v := q.Get("severity"); v != "" {
		if parsed, ok := store.ParseSeverity(v); ok {
			f.Severity = parsed
		} else {
			writeErr(w, r, http.StatusBadRequest, `invalid severity (must be "info", "warning", or "critical")`)
			return f, false
		}
	}
	return f, true
}

// listAlerts serves GET /alerts with query filters:
// monitor_id, rule_id, contract_id, from, to (RFC 3339), sort
// (created_at_desc default, created_at_asc), limit, cursor (last seen
// alert id; comparison follows sort).
func (s *Server) listAlerts(w http.ResponseWriter, r *http.Request) {
	f, ok := parseAlertFilter(w, r)
	if !ok {
		return
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

// maxAlertExportRows is the documented ceiling on a single CSV export when
// the caller does not ask for a smaller limit. The export streams page by
// page, so this bounds the total rows rather than peak memory, but it still
// keeps a hand-typed URL from pinning a connection indefinitely.
const maxAlertExportRows = 10000

// alertExportPageSize is the store's hard maximum page size. ListAlerts
// silently clamps anything larger back to 50, which would make a full-page
// check misfire and re-query the same rows, so the export always pages at
// exactly this size and relies on the keyset cursor for the rest.
const alertExportPageSize = 500

// alertCSVHeader is the fixed column order of the CSV export. The order is
// part of the endpoint's contract: spreadsheet templates and importers key
// off column position, so new columns are appended, never inserted.
var alertCSVHeader = []string{
	"id", "monitor_name", "rule_id", "severity", "contract_id",
	"event_name", "event_id", "ledger", "created_at", "payload",
}

// exportAlertsCSV serves GET /alerts.csv. It shares parseAlertFilter with
// GET /alerts, pages through the store with a keyset cursor (so the whole
// result set is never held in memory) and writes each page straight to the
// response as CSV. An absent limit means "everything", capped at
// maxAlertExportRows; an explicit limit is honoured up to that same cap.
func (s *Server) exportAlertsCSV(w http.ResponseWriter, r *http.Request) {
	f, ok := parseAlertFilter(w, r)
	if !ok {
		return
	}
	remaining := f.Limit
	if remaining <= 0 || remaining > maxAlertExportRows {
		remaining = maxAlertExportRows
	}

	// monitor_name needs a lookup that the alert rows do not carry. Fetch it
	// once up front rather than re-querying every monitor for every page; a
	// failure here can still be reported as a normal error envelope because
	// nothing has been written yet.
	monitors, err := s.store.ListMonitors(r.Context(), false)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	names := make(map[int64]string, len(monitors))
	for _, m := range monitors {
		names[m.ID] = m.Name
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+alertExportFilename(f)+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	cw := csv.NewWriter(w)
	if err := cw.Write(alertCSVHeader); err != nil {
		s.log.Error("alert export write failed", "request_id", reqid.From(r), "err", err)
		return
	}
	for remaining > 0 {
		f.Limit = remaining
		if f.Limit > alertExportPageSize {
			f.Limit = alertExportPageSize
		}
		page, err := s.store.ListAlerts(r.Context(), f)
		if err != nil {
			// Headers and rows are already on the wire, so a JSON error
			// envelope would corrupt the CSV. Log with the request id so the
			// failure is still traceable, then end the stream.
			s.log.Error("alert export list failed", "request_id", reqid.From(r), "err", err)
			cw.Flush()
			return
		}
		for _, a := range page {
			if err := cw.Write(alertCSVRow(a, names[a.MonitorID])); err != nil {
				s.log.Error("alert export write failed", "request_id", reqid.From(r), "err", err)
				return
			}
		}
		cw.Flush()
		if err := cw.Error(); err != nil {
			s.log.Error("alert export flush failed", "request_id", reqid.From(r), "err", err)
			return
		}
		// A short page means the filter is exhausted; anything else would
		// make the cursor loop forever against a full page that never ends.
		if len(page) < f.Limit {
			return
		}
		remaining -= len(page)
		f.AfterID = page[len(page)-1].ID
	}
}

// alertCSVRow flattens one alert into the export's fixed column order. The
// payload's contract id, event name and ledger are extracted for the
// spreadsheet-friendly columns, while the original payload is emitted
// verbatim as the last column so no information is lost.
func alertCSVRow(a store.Alert, monitorName string) []string {
	var p struct {
		ContractID string `json:"contract_id"`
		EventName  string `json:"event_name"`
		Ledger     uint32 `json:"ledger"`
	}
	_ = json.Unmarshal(a.Payload, &p)
	sev := string(a.Severity)
	if sev == "" {
		sev = "warning"
	}
	return []string{
		strconv.FormatInt(a.ID, 10),
		csvSafe(monitorName),
		strconv.FormatInt(a.RuleID, 10),
		csvSafe(sev),
		csvSafe(p.ContractID),
		csvSafe(p.EventName),
		csvSafe(a.EventID),
		strconv.FormatUint(uint64(p.Ledger), 10),
		a.CreatedAt.UTC().Format(time.RFC3339),
		csvSafe(string(a.Payload)),
	}
}

// alertExportFilename names the download after the requested date range so
// several exports from the same dashboard do not collide in a Downloads
// folder. A missing bound reads as "all".
func alertExportFilename(f store.AlertFilter) string {
	from, to := "all", "all"
	if !f.From.IsZero() {
		from = f.From.UTC().Format("20060102")
	}
	if !f.To.IsZero() {
		to = f.To.UTC().Format("20060102")
	}
	return fmt.Sprintf("alerts_%s_%s.csv", from, to)
}

// csvSafe neutralizes spreadsheet formula injection. Excel, Sheets and
// LibreOffice treat a cell starting with =, +, - or @ as a formula, so
// attacker-controlled on-chain strings (contract ids, event names, the
// payload itself) could otherwise execute a command or exfiltrate data when
// the export is opened. Prefixing an apostrophe keeps the value as text;
// the spreadsheet hides the apostrophe from the displayed cell.
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@':
		return "'" + s
	}
	return s
}

// listDeliveries serves GET /alerts/{id}/deliveries.
// Optional ?status=success|failed is applied in SQL; anything else is 400.
func (s *Server) listDeliveries(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	status := r.URL.Query().Get("status")
	if status != "" && !store.ValidDeliveryStatus(status) {
		writeErr(w, r, http.StatusBadRequest, "invalid status")
		return
	}
	list, err := s.store.ListDeliveryAttempts(r.Context(), id, status)
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
	attempts, err := s.store.ListDeliveryAttempts(r.Context(), alertID, "")
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
		Severity:  string(a.Severity),
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
