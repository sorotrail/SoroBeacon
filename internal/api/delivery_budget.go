package api

import (
	"net/http"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// deliveryBudgetResponse is the budget plus the derived fields a dashboard or
// script would otherwise recompute. Remaining is null for an unlimited budget.
type deliveryBudgetResponse struct {
	MonitorID   int64      `json:"monitor_id"`
	Limit       *int       `json:"limit"`
	Used        int        `json:"used"`
	Remaining   *int       `json:"remaining"`
	Exhausted   bool       `json:"exhausted"`
	WindowStart *time.Time `json:"window_start"`
	ResetsAt    *time.Time `json:"resets_at"`
	ExhaustedAt *time.Time `json:"exhausted_at"`
}

func deliveryBudgetResponseFrom(b *store.DeliveryBudget) deliveryBudgetResponse {
	resp := deliveryBudgetResponse{
		MonitorID:   b.MonitorID,
		Limit:       b.Limit,
		Used:        b.Used,
		Exhausted:   b.Exhausted(),
		ExhaustedAt: b.ExhaustedAt,
	}
	if b.Limit != nil {
		rem := b.Remaining()
		resp.Remaining = &rem
		start, resets := b.WindowStart, b.ResetsAt()
		resp.WindowStart, resp.ResetsAt = &start, &resets
	}
	return resp
}

// budgetMonitorID parses the path id and confirms the monitor exists in the
// caller's workspace. The budget store is keyed by monitor id alone, so this
// check is what keeps one workspace from touching another's budget.
func (s *Server) budgetMonitorID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	if _, err := s.store.GetMonitor(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return 0, false
	}
	return id, true
}

func (s *Server) respondDeliveryBudget(w http.ResponseWriter, r *http.Request, id int64) {
	b, err := s.store.GetDeliveryBudget(r.Context(), id, time.Now())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, deliveryBudgetResponseFrom(b))
}

// getDeliveryBudget serves GET /monitors/{id}/delivery-budget.
func (s *Server) getDeliveryBudget(w http.ResponseWriter, r *http.Request) {
	id, ok := s.budgetMonitorID(w, r)
	if !ok {
		return
	}
	s.respondDeliveryBudget(w, r, id)
}

// putDeliveryBudget serves PUT /monitors/{id}/delivery-budget.
func (s *Server) putDeliveryBudget(w http.ResponseWriter, r *http.Request) {
	id, ok := s.budgetMonitorID(w, r)
	if !ok {
		return
	}
	var raw map[string]*int
	if !readJSON(w, r, &raw) {
		return
	}
	limit, present := raw["limit"]
	switch {
	case !present:
		writeValidation(w, r, []FieldError{{Field: "limit", Reason: "required; use null for unlimited"}})
		return
	case limit != nil && *limit <= 0:
		writeValidation(w, r, []FieldError{{Field: "limit", Reason: "must be a positive number of deliveries per day, or null for unlimited"}})
		return
	}
	if err := s.store.SetDeliveryBudget(r.Context(), id, limit); err != nil {
		s.fail(w, r, err)
		return
	}
	s.respondDeliveryBudget(w, r, id)
}

// resetDeliveryBudget serves POST /monitors/{id}/delivery-budget/reset, the
// manual way out of an exhausted budget before the window rolls over.
func (s *Server) resetDeliveryBudget(w http.ResponseWriter, r *http.Request) {
	id, ok := s.budgetMonitorID(w, r)
	if !ok {
		return
	}
	if err := s.store.ResetDeliveryBudget(r.Context(), id, time.Now()); err != nil {
		s.fail(w, r, err)
		return
	}
	s.respondDeliveryBudget(w, r, id)
}
