package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// dryRunRequest is the request body for a dry run.
type dryRunRequest struct {
	Type       *string          `json:"type"`
	Params     *json.RawMessage `json:"params"`
	WindowDays *int           `json:"window_days"`
	MaxSamples *int           `json:"max_samples"`
}

// dryRunResponse is the response from a dry run.
type dryRunResponse struct {
	Count          int      `json:"count"`
	Matches        []any    `json:"matches"`
	TotalEvaluated int      `json:"total_evaluated"`
	Note           string   `json:"note"`
}

// dryRun evaluates a candidate rule against historical events
// using the production evaluation path. Nothing is persisted and
// nothing is delivered.
func (s *Server) dryRun(w http.ResponseWriter, r *http.Request) {
	monitorID, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid monitor id")
		return
	}
	if _, err := s.store.GetMonitor(r.Context(), monitorID); err != nil {
		s.fail(w, r, err)
		return
	}

	var req dryRunRequest
	if !readJSON(w, r, &req) {
		return
	}

	params := json.RawMessage(`{}`)
	if req.Params != nil {
		params = *req.Params
	}

	if req.Type == nil || *req.Type == "" {
		writeValidation(w, r, []FieldError{{Field: "type", Reason: "type is required"}})
		return
	}
	if err := s.registry.Validate(*req.Type, params); err != nil {
		writeValidation(w, r, ruleParamDetails(err))
		return
	}

	windowDays := 30
	if req.WindowDays != nil {
		windowDays = *req.WindowDays
	}
	clampedDays := store.ClampAlertSeriesDays(windowDays)
	since := time.Now().UTC().AddDate(0, 0, -clampedDays)

	maxSamples := 100
	if req.MaxSamples != nil {
		maxSamples = *req.MaxSamples
	}
	if maxSamples < 1 {
		maxSamples = 1
	}
	if maxSamples > 1000 {
		maxSamples = 1000
	}

	alerts, err := s.store.ListAlerts(r.Context(), store.AlertFilter{
		MonitorID: monitorID,
		From:      since,
		Sort:      "created_at_asc",
		Limit:     maxSamples,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}

	events := make([]*stellar.DecodedEvent, 0, len(alerts))
	for _, a := range alerts {
		decoded, ok := decodeAlertToEvent(a)
		if !ok {
			continue
		}
		events = append(events, decoded)
	}

	matches, err := s.registry.DryRun(r.Context(), *req.Type, events, params)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "evaluation failed")
		s.log.Error("dry run evaluation failed", "monitor_id", monitorID, "err", err)
		return
	}

	matchSamples := make([]any, 0, len(matches))
	for _, m := range matches {
		matchSamples = append(matchSamples, map[string]any{
			"event_id": m.ID,
			"contract_id": m.ContractID,
			"event_name": m.EventName(),
			"ledger": m.Ledger,
		})
	}

	writeJSON(w, http.StatusOK, dryRunResponse{
		Count:          len(matches),
		Matches:        matchSamples,
		TotalEvaluated: len(events),
		Note:           "stored alerts are biased: only matched events are captured, so dry-run results underrepresent the true event corpus",
	})
}

// decodeAlertToEvent reconstructs a DecodedEvent from an alert's
// stored payload. Returns false if the payload cannot be parsed.
func decodeAlertToEvent(a store.Alert) (*stellar.DecodedEvent, bool) {
	var body map[string]any
	if err := json.Unmarshal(a.Payload, &body); err != nil {
		return nil, false
	}
	if body == nil {
		return nil, false
	}

	ev := &stellar.DecodedEvent{
		ID:      a.EventID,
		TxHash:  toString(body["tx_hash"]),
	}

	if cid, ok := body["contract_id"].(string); ok {
		ev.ContractID = cid
	}
	if ledger, ok := body["ledger"].(float64); ok {
		ev.Ledger = uint32(ledger)
	}
	if topics, ok := body["topics"].([]any); ok {
		ev.Topics = topics
	}
	if value, ok := body["value"].(string); ok {
		ev.Value = value
	}
	if fields, ok := body["fields"].(map[string]any); ok {
		ev.Fields = fields
	}

	return ev, true
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
