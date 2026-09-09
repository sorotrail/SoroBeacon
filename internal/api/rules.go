package api

import (
	"encoding/json"
	"net/http"

	"github.com/sorotrail/sorobeacon/internal/store"
)

type ruleRequest struct {
	Type    *string          `json:"type"`
	Params  *json.RawMessage `json:"params"`
	Enabled *bool            `json:"enabled"`
}

func (s *Server) createRule(w http.ResponseWriter, r *http.Request) {
	monitorID, err := pathID(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid monitor id")
		return
	}
	if _, err := s.store.GetMonitor(r.Context(), monitorID); err != nil {
		s.fail(w, err)
		return
	}
	var req ruleRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Type == nil || *req.Type == "" {
		writeErr(w, http.StatusBadRequest, "type is required")
		return
	}
	params := json.RawMessage(`{}`)
	if req.Params != nil {
		params = *req.Params
	}
	if err := s.registry.Validate(*req.Type, params); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	rule := store.Rule{
		MonitorID: monitorID,
		Type:      *req.Type,
		Params:    params,
		Enabled:   req.Enabled == nil || *req.Enabled,
	}
	if err := s.store.CreateRule(r.Context(), &rule); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rule)
}

func (s *Server) listRules(w http.ResponseWriter, r *http.Request) {
	monitorID, err := pathID(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid monitor id")
		return
	}
	list, err := s.store.ListRules(r.Context(), monitorID, false)
	if err != nil {
		s.fail(w, err)
		return
	}
	if list == nil {
		list = []store.Rule{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) updateRule(w http.ResponseWriter, r *http.Request) {
	rule, ok := s.ruleFromPath(w, r)
	if !ok {
		return
	}
	var req ruleRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Type != nil {
		rule.Type = *req.Type
	}
	if req.Params != nil {
		rule.Params = *req.Params
	}
	if req.Enabled != nil {
		rule.Enabled = *req.Enabled
	}
	if err := s.registry.Validate(rule.Type, rule.Params); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.UpdateRule(r.Context(), rule); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

func (s *Server) deleteRule(w http.ResponseWriter, r *http.Request) {
	rule, ok := s.ruleFromPath(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteRule(r.Context(), rule.ID); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ruleFromPath loads the rule at /monitors/{id}/rules/{ruleID}, verifying
// it belongs to the monitor in the path.
func (s *Server) ruleFromPath(w http.ResponseWriter, r *http.Request) (*store.Rule, bool) {
	monitorID, err := pathID(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid monitor id")
		return nil, false
	}
	ruleID, err := pathID(r, "ruleID")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid rule id")
		return nil, false
	}
	rule, err := s.store.GetRule(r.Context(), ruleID)
	if err != nil {
		s.fail(w, err)
		return nil, false
	}
	if rule.MonitorID != monitorID {
		writeErr(w, http.StatusNotFound, "not found")
		return nil, false
	}
	return rule, true
}
