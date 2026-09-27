package api

import (
	"net/http"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// inhibitionInput is the POST /inhibitions body. Window seconds are
// optional: absent or non-positive means the documented default
// (store.DefaultInhibitionWindowSeconds), applied by the store.
type inhibitionInput struct {
	SourceRuleID        int64 `json:"source_rule_id"`
	TargetRuleID        int64 `json:"target_rule_id"`
	FiringWindowSeconds int   `json:"firing_window_seconds"`
}

// createInhibition serves POST /inhibitions. Both rules must exist (else a
// foreign key would reject the row with an opaque 500); a rule inhibiting
// itself is a 400 since it can never carry information.
func (s *Server) createInhibition(w http.ResponseWriter, r *http.Request) {
	var in inhibitionInput
	if !readJSON(w, r, &in) {
		return
	}
	var problems []FieldError
	if in.SourceRuleID <= 0 {
		problems = append(problems, FieldError{Field: "source_rule_id", Reason: "source_rule_id is required"})
	}
	if in.TargetRuleID <= 0 {
		problems = append(problems, FieldError{Field: "target_rule_id", Reason: "target_rule_id is required"})
	}
	if in.SourceRuleID > 0 && in.SourceRuleID == in.TargetRuleID {
		problems = append(problems, FieldError{Field: "source_rule_id", Reason: "a rule cannot inhibit itself"})
	}
	if in.FiringWindowSeconds < 0 {
		problems = append(problems, FieldError{Field: "firing_window_seconds", Reason: "firing_window_seconds must not be negative"})
	}
	if len(problems) > 0 {
		writeValidation(w, r, problems)
		return
	}
	for _, id := range []int64{in.SourceRuleID, in.TargetRuleID} {
		if _, err := s.store.GetRule(r.Context(), id); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	inh := store.Inhibition{
		SourceRuleID:        in.SourceRuleID,
		TargetRuleID:        in.TargetRuleID,
		FiringWindowSeconds: in.FiringWindowSeconds,
	}
	if err := s.store.CreateInhibition(r.Context(), &inh); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, inh)
}

// listInhibitions serves GET /inhibitions.
func (s *Server) listInhibitions(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListInhibitions(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if list == nil {
		list = []store.Inhibition{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"inhibitions": list})
}

// deleteInhibition serves DELETE /inhibitions/{sourceID}/{targetID}.
func (s *Server) deleteInhibition(w http.ResponseWriter, r *http.Request) {
	sourceID, err := pathID(r, "sourceID")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid sourceID")
		return
	}
	targetID, err := pathID(r, "targetID")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid targetID")
		return
	}
	if err := s.store.DeleteInhibition(r.Context(), sourceID, targetID); err != nil {
		s.fail(w, r, err)
		return
	}
	writeNoContent(w)
}
