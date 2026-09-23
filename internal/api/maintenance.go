package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// maintenanceWindowRequest carries writes. Fields are pointers so PATCH can
// distinguish "leave unchanged" from "clear", matching channelRequest.
type maintenanceWindowRequest struct {
	Reason     *string    `json:"reason"`
	Scope      *string    `json:"scope"`
	MonitorID  *int64     `json:"monitor_id"`
	ContractID *string    `json:"contract_id"`
	StartAt    *time.Time `json:"start_at"`
	EndAt      *time.Time `json:"end_at"`
}

// validateWindow checks one window's fields and returns envelope details.
// It is deliberately strict about scope/identifier pairings: a monitor-scoped
// window without a monitor_id can never match, and the store's CHECK would
// reject it with an opaque database error instead of a useful field path.
func validateWindow(w store.MaintenanceWindow) []FieldError {
	var details []FieldError
	if strings.TrimSpace(w.Reason) == "" {
		details = append(details, FieldError{Field: "reason", Reason: "reason is required"})
	}
	if !store.ValidMaintenanceScope(w.Scope) {
		details = append(details, FieldError{Field: "scope", Reason: "scope must be one of global, monitor, contract"})
	}
	if w.StartAt.IsZero() {
		details = append(details, FieldError{Field: "start_at", Reason: "start_at is required"})
	}
	if w.EndAt.IsZero() {
		details = append(details, FieldError{Field: "end_at", Reason: "end_at is required (an open-ended window would be forgotten)"})
	}
	if !w.StartAt.IsZero() && !w.EndAt.IsZero() && !w.EndAt.After(w.StartAt) {
		details = append(details, FieldError{Field: "end_at", Reason: "end_at must be after start_at"})
	}
	hasMonitor := w.MonitorID != nil && *w.MonitorID != 0
	hasContract := w.ContractID != nil && strings.TrimSpace(*w.ContractID) != ""
	switch w.Scope {
	case store.MaintenanceScopeGlobal:
		if hasMonitor || hasContract {
			details = append(details, FieldError{Field: "scope", Reason: "a global window takes no monitor_id or contract_id"})
		}
	case store.MaintenanceScopeMonitor:
		if !hasMonitor {
			details = append(details, FieldError{Field: "monitor_id", Reason: "monitor_id is required for a monitor-scoped window"})
		}
		if hasContract {
			details = append(details, FieldError{Field: "contract_id", Reason: "a monitor-scoped window takes no contract_id"})
		}
	case store.MaintenanceScopeContract:
		if !hasContract {
			details = append(details, FieldError{Field: "contract_id", Reason: "contract_id is required for a contract-scoped window"})
		}
		if hasMonitor {
			details = append(details, FieldError{Field: "monitor_id", Reason: "a contract-scoped window takes no monitor_id"})
		}
	}
	return details
}

func (s *Server) createMaintenanceWindow(w http.ResponseWriter, r *http.Request) {
	var req maintenanceWindowRequest
	if !readJSON(w, r, &req) {
		return
	}
	mw := store.MaintenanceWindow{Scope: store.MaintenanceScopeGlobal}
	if req.Reason != nil {
		mw.Reason = *req.Reason
	}
	if req.Scope != nil {
		mw.Scope = *req.Scope
	}
	mw.MonitorID = req.MonitorID
	mw.ContractID = req.ContractID
	if req.StartAt != nil {
		mw.StartAt = *req.StartAt
	}
	if req.EndAt != nil {
		mw.EndAt = *req.EndAt
	}
	if details := validateWindow(mw); len(details) > 0 {
		writeValidation(w, r, details)
		return
	}
	if err := s.store.CreateMaintenanceWindow(r.Context(), &mw); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, mw)
}

func (s *Server) listMaintenanceWindows(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.MaintenanceWindowFilter{
		Active:   q.Get("active") == "true",
		Upcoming: q.Get("upcoming") == "true",
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeErr(w, r, http.StatusBadRequest, "invalid limit")
			return
		}
		f.Limit = n
	}
	list, err := s.store.ListMaintenanceWindows(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if list == nil {
		list = []store.MaintenanceWindow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"maintenance_windows": list})
}

func (s *Server) getMaintenanceWindow(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	mw, err := s.store.GetMaintenanceWindow(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, mw)
}

func (s *Server) updateMaintenanceWindow(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	mw, err := s.store.GetMaintenanceWindow(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var req maintenanceWindowRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Reason != nil {
		mw.Reason = *req.Reason
	}
	if req.Scope != nil {
		mw.Scope = *req.Scope
	}
	// Assigning the scope's identifiers wholesale avoids leaving a stale
	// monitor_id behind when the scope changes.
	if req.MonitorID != nil {
		mw.MonitorID = req.MonitorID
	}
	if req.ContractID != nil {
		mw.ContractID = req.ContractID
	}
	if req.StartAt != nil {
		mw.StartAt = *req.StartAt
	}
	if req.EndAt != nil {
		mw.EndAt = *req.EndAt
	}
	if details := validateWindow(*mw); len(details) > 0 {
		writeValidation(w, r, details)
		return
	}
	if err := s.store.UpdateMaintenanceWindow(r.Context(), mw); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, mw)
}

func (s *Server) deleteMaintenanceWindow(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.store.DeleteMaintenanceWindow(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	writeNoContent(w)
}
