package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// ruleParamDetails maps a registry.Validate error onto envelope fields.
// Unknown types belong on "type"; param problems keep their path under
// "params" rather than flattening into a single message.
func ruleParamDetails(err error) []FieldError {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "unknown rule type") {
		return []FieldError{{Field: "type", Reason: err.Error()}}
	}
	return detailsFromErr("params", err)
}

type ruleRequest struct {
	Type     *string          `json:"type"`
	Params   *json.RawMessage `json:"params"`
	Enabled  *bool            `json:"enabled"`
	Severity *string          `json:"severity"`
}

func (s *Server) createRule(w http.ResponseWriter, r *http.Request) {
	monitorID, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid monitor id")
		return
	}
	if _, err := s.store.GetMonitor(r.Context(), monitorID); err != nil {
		s.fail(w, r, err)
		return
	}
	var req ruleRequest
	if !readJSON(w, r, &req) {
		return
	}
	rule, details := s.ruleFromRequest(monitorID, req)
	if len(details) > 0 {
		writeValidation(w, r, details)
		return
	}
	if err := s.store.CreateRule(r.Context(), &rule); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, rule)
}

// maxBulkRules is the documented ceiling on POST /rules/bulk. Provisioning
// a monitor should never need more than this in one shot; a larger body is
// almost certainly a bug or an unbounded script.
const maxBulkRules = 50

func (s *Server) createRulesBulk(w http.ResponseWriter, r *http.Request) {
	monitorID, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid monitor id")
		return
	}
	if _, err := s.store.GetMonitor(r.Context(), monitorID); err != nil {
		s.fail(w, r, err)
		return
	}
	var reqs []ruleRequest
	if !readJSON(w, r, &reqs) {
		return
	}
	if len(reqs) == 0 {
		writeValidation(w, r, []FieldError{{Field: "rules", Reason: "must not be empty"}})
		return
	}
	if len(reqs) > maxBulkRules {
		writeValidation(w, r, []FieldError{{
			Field:  "rules",
			Reason: "at most 50 rules per request",
		}})
		return
	}

	var details []FieldError
	rules := make([]*store.Rule, 0, len(reqs))
	for i, req := range reqs {
		rule, d := s.ruleFromRequest(monitorID, req)
		if len(d) > 0 {
			prefix := "rules[" + strconv.Itoa(i) + "]"
			for _, fe := range d {
				details = append(details, FieldError{Field: joinPath(prefix, fe.Field), Reason: fe.Reason})
			}
			continue
		}
		created := rule
		rules = append(rules, &created)
	}
	if len(details) > 0 {
		writeValidation(w, r, details)
		return
	}
	if err := s.store.CreateRules(r.Context(), rules); err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]store.Rule, len(rules))
	for i, rule := range rules {
		out[i] = *rule
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) ruleFromRequest(monitorID int64, req ruleRequest) (store.Rule, []FieldError) {
	var details []FieldError
	if req.Type == nil || *req.Type == "" {
		details = append(details, FieldError{Field: "type", Reason: "type is required"})
	}
	params := json.RawMessage(`{}`)
	if req.Params != nil {
		params = *req.Params
	}
	if req.Type != nil && *req.Type != "" {
		if err := s.registry.Validate(*req.Type, params); err != nil {
			details = append(details, ruleParamDetails(err)...)
		}
	}
	severity := store.SeverityWarning
	if req.Severity != nil && *req.Severity != "" {
		parsed, ok := store.ParseSeverity(*req.Severity)
		if !ok {
			details = append(details, FieldError{Field: "severity", Reason: `must be "info", "warning", or "critical"`})
		} else {
			severity = parsed
		}
	}
	if len(details) > 0 {
		return store.Rule{}, details
	}
	return store.Rule{
		MonitorID: monitorID,
		Type:      *req.Type,
		Params:    params,
		Enabled:   req.Enabled == nil || *req.Enabled,
		Severity:  severity,
	}, nil
}

func (s *Server) listRules(w http.ResponseWriter, r *http.Request) {
	monitorID, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid monitor id")
		return
	}
	list, err := s.store.ListRules(r.Context(), monitorID, false)
	if err != nil {
		s.fail(w, r, err)
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
		if *req.Type == "" {
			writeValidation(w, r, []FieldError{{Field: "type", Reason: "type is required"}})
			return
		}
		rule.Type = *req.Type
	}
	if req.Params != nil {
		rule.Params = *req.Params
	}
	if req.Enabled != nil {
		rule.Enabled = *req.Enabled
	}
	if req.Severity != nil {
		if *req.Severity == "" {
			writeValidation(w, r, []FieldError{{Field: "severity", Reason: `must be "info", "warning", or "critical"`}})
			return
		}
		parsed, ok := store.ParseSeverity(*req.Severity)
		if !ok {
			writeValidation(w, r, []FieldError{{Field: "severity", Reason: `must be "info", "warning", or "critical"`}})
			return
		}
		rule.Severity = parsed
	}
	if err := s.registry.Validate(rule.Type, rule.Params); err != nil {
		writeValidation(w, r, ruleParamDetails(err))
		return
	}
	if err := s.store.UpdateRule(r.Context(), rule); err != nil {
		s.fail(w, r, err)
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
		s.fail(w, r, err)
		return
	}
	writeNoContent(w)
}

// ruleFromPath loads the rule at /monitors/{id}/rules/{ruleID}, verifying
// it belongs to the monitor in the path.
func (s *Server) ruleFromPath(w http.ResponseWriter, r *http.Request) (*store.Rule, bool) {
	monitorID, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid monitor id")
		return nil, false
	}
	ruleID, err := pathID(r, "ruleID")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid rule id")
		return nil, false
	}
	rule, err := s.store.GetRule(r.Context(), ruleID)
	if err != nil {
		s.fail(w, r, err)
		return nil, false
	}
	if rule.MonitorID != monitorID {
		writeErr(w, r, http.StatusNotFound, "not found")
		return nil, false
	}
	return rule, true
}
