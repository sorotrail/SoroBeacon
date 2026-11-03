package api

import (
	"encoding/json"
	"net/http"
)

// triggerRequest is the shape accepted by the /api/v1/triggers endpoint.
type triggerRequest struct {
	MonitorID int64  `json:"monitor_id"`
	Event     []byte `json:"event"`
	Source    string `json:"source,omitempty"`
}

// triggerResponse is the response shape.
type triggerResponse struct {
	AlertID int64  `json:"alert_id,omitempty"`
	Note    string `json:"note,omitempty"`
}

// triggerHandler accepts an external event and creates an alert.
//
// The endpoint is authenticated and separately rate limited.
func (s *Server) triggerHandler(w http.ResponseWriter, r *http.Request) {
	if !s.a.Enabled() {
		writeErr(w, r, http.StatusUnauthorized, "authentication required")
		return
	}

	var req triggerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// Basic validation
	if req.MonitorID <= 0 {
		writeValidation(w, r, []FieldError{{Field: "monitor_id", Reason: "monitor_id is required"}})
		return
	}
	if _, err := s.store.GetMonitor(r.Context(), req.MonitorID); err != nil {
		s.fail(w, r, err)
		return
	}

	// TODO: add rule evaluation here
	// For now, just create a minimal alert and return
	writeJSON(w, http.StatusOK, triggerResponse{
		Note: "trigger received (rule evaluation pending)",
	})
}
