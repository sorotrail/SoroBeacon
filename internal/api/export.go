package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/sorotrail/sorobeacon/internal/reqid"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// exportAlertsNDJSON serves GET /alerts/export as NDJSON streaming.
// It shares parseAlertFilter with GET /alerts, streams each alert as a
// newline-delimited JSON object, flushes periodically, and caps the
// total at maxAlertExportRows. A repeatable-read snapshot keeps the
// export consistent; request cancellation stops the underlying query.
func (s *Server) exportAlertsNDJSON(w http.ResponseWriter, r *http.Request) {
	f, ok := parseAlertFilter(w, r)
	if !ok {
		return
	}
	if f.Limit <= 0 || f.Limit > maxAlertExportRows {
		f.Limit = maxAlertExportRows
	}

	// monitor_name needs a lookup that the alert rows do not carry.
	// Fetch it once up front rather than re-querying every monitor
	// for every row.
	monitors, err := s.store.ListMonitors(r.Context(), false)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	names := make(map[int64]string, len(monitors))
	for _, m := range monitors {
		names[m.ID] = m.Name
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Request-ID", reqid.From(r))
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.log.Error("NDJSON export requires a Flusher", "request_id", reqid.From(r))
		return
	}

	count := 0
	flushEvery := alertExportPageSize

	err = s.store.ListAlertsStream(r.Context(), f, func(a store.Alert) error {
		line := exportAlertNDJSONRow(a, names[a.MonitorID])
		b, err := json.Marshal(line)
		if err != nil {
			s.log.Error("NDJSON marshal failed", "request_id", reqid.From(r), "err", err)
			return nil // skip, not fatal
		}
		w.Write(append(b, '\n'))
		count++
		if count%flushEvery == 0 {
			flusher.Flush()
		}
		return nil
	})
	if err != nil {
		s.log.Error("NDJSON export stream failed", "request_id", reqid.From(r), "err", err)
		return
	}
	flusher.Flush()

	s.log.Info("NDJSON export complete", "request_id", reqid.From(r), "rows", count)
}

// exportAlertNDJSONRow converts an alert to an NDJSON object with
// expanded monitor_name, event_name, contract_id, and ledger fields
// extracted from the payload.
func exportAlertNDJSONRow(a store.Alert, monitorName string) map[string]any {
	var p struct {
		ContractID string `json:"contract_id"`
		EventName  string `json:"event_name"`
		Ledger     uint32 `json:"ledger"`
	}
	_ = json.Unmarshal(a.Payload, &p)
	return map[string]any{
		"id":          strconv.FormatInt(a.ID, 10),
		"monitor_id":  strconv.FormatInt(a.MonitorID, 10),
		"monitor_name": monitorName,
		"rule_id":     strconv.FormatInt(a.RuleID, 10),
		"event_id":    a.EventID,
		"event_name":  p.EventName,
		"contract_id": p.ContractID,
		"ledger":      p.Ledger,
		"created_at":  a.CreatedAt.UTC().Format(time.RFC3339),
		"payload":     string(a.Payload),
	}
}

// ndjsonRowMap is used by tests to verify NDJSON output structure.
func ndjsonRowMap(a store.Alert, monitorName string) map[string]any {
	return exportAlertNDJSONRow(a, monitorName)
}

// writeNDJSON writes a single NDJSON line to the response writer.
// Exported so tests can verify the format without a live server.
func writeNDJSON(w io.Writer, v map[string]any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}
