package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// --- monitor templates: one-time copy model ---
//
// Instantiation copies the template's rules and channels into a new monitor.
// Editing a template does not retroactively update existing monitors; deleting
// a template leaves its instances untouched.

type templateInput struct {
	Name        string                      `json:"name"`
	Description string                      `json:"description"`
	Rules       []store.MonitorTemplateRule `json:"rules"`
	ChannelIDs  []int64                     `json:"channel_ids"`
	Parameters  []store.TemplateParameter   `json:"parameters"`
}

type instantiateInput struct {
	Name        string            `json:"name"`
	ContractIDs []string          `json:"contract_ids"`
	Parameters  map[string]string `json:"parameters"`
}

type bulkInstantiateInput struct {
	Instances []instantiateInput `json:"instances"`
}

func (s *Server) createTemplate(w http.ResponseWriter, r *http.Request) {
	var in templateInput
	if !readJSON(w, r, &in) {
		return
	}
	var problems []FieldError
	if in.Name == "" {
		problems = append(problems, FieldError{Field: "name", Reason: "required"})
	}
	if len(problems) > 0 {
		writeValidation(w, r, problems)
		return
	}
	t := store.MonitorTemplate{
		Name:        in.Name,
		Description: in.Description,
		Rules:       in.Rules,
		ChannelIDs:  in.ChannelIDs,
		Parameters:  in.Parameters,
	}
	if t.Rules == nil {
		t.Rules = []store.MonitorTemplateRule{}
	}
	if t.ChannelIDs == nil {
		t.ChannelIDs = []int64{}
	}
	if t.Parameters == nil {
		t.Parameters = []store.TemplateParameter{}
	}
	if err := s.store.CreateMonitorTemplate(r.Context(), &t); err != nil {
		s.log.Error("create_template", "err", err)
		writeErr(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request) {
	templates, err := s.store.ListMonitorTemplates(r.Context())
	if err != nil {
		s.log.Error("list_templates", "err", err)
		writeErr(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if templates == nil {
		templates = []store.MonitorTemplate{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": templates})
}

func (s *Server) getTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	t, err := s.store.GetMonitorTemplate(r.Context(), id)
	if err != nil {
		if err == store.ErrNotFound {
			writeErr(w, r, http.StatusNotFound, "template not found")
			return
		}
		s.log.Error("get_template", "err", err)
		writeErr(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) updateTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	existing, err := s.store.GetMonitorTemplate(r.Context(), id)
	if err != nil {
		if err == store.ErrNotFound {
			writeErr(w, r, http.StatusNotFound, "template not found")
			return
		}
		s.log.Error("update_template", "err", err)
		writeErr(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	var in templateInput
	if !readJSON(w, r, &in) {
		return
	}
	if in.Name != "" {
		existing.Name = in.Name
	}
	if in.Description != "" {
		existing.Description = in.Description
	}
	if in.Rules != nil {
		existing.Rules = in.Rules
	}
	if in.ChannelIDs != nil {
		existing.ChannelIDs = in.ChannelIDs
	}
	if in.Parameters != nil {
		existing.Parameters = in.Parameters
	}
	if err := s.store.UpdateMonitorTemplate(r.Context(), existing); err != nil {
		s.log.Error("update_template", "err", err)
		writeErr(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, existing)
}

func (s *Server) deleteTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.store.DeleteMonitorTemplate(r.Context(), id); err != nil {
		if err == store.ErrNotFound {
			writeErr(w, r, http.StatusNotFound, "template not found")
			return
		}
		s.log.Error("delete_template", "err", err)
		writeErr(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	writeNoContent(w)
}

func (s *Server) instantiateTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	tmpl, err := s.store.GetMonitorTemplate(r.Context(), id)
	if err != nil {
		if err == store.ErrNotFound {
			writeErr(w, r, http.StatusNotFound, "template not found")
			return
		}
		s.log.Error("instantiate_template", "err", err)
		writeErr(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	var in instantiateInput
	if !readJSON(w, r, &in) {
		return
	}
	monitor, problems := s.buildMonitorFromTemplate(tmpl, in)
	if len(problems) > 0 {
		writeValidation(w, r, problems)
		return
	}
	if err := s.store.CreateMonitor(r.Context(), monitor); err != nil {
		s.log.Error("instantiate_template", "err", err)
		writeErr(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if len(tmpl.ChannelIDs) > 0 {
		_ = s.store.SetMonitorChannels(r.Context(), monitor.ID, tmpl.ChannelIDs)
	}
	for _, tr := range tmpl.Rules {
		params := substituteParams(tr.Params, in.Parameters)
		if err := s.registry.Validate(tr.Type, params); err != nil {
			continue
		}
		rule := store.Rule{MonitorID: monitor.ID, Type: tr.Type, Params: params, Enabled: true}
		_ = s.store.CreateRule(r.Context(), &rule)
	}
	writeJSON(w, http.StatusCreated, monitor)
}

func (s *Server) bulkInstantiateTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	tmpl, err := s.store.GetMonitorTemplate(r.Context(), id)
	if err != nil {
		if err == store.ErrNotFound {
			writeErr(w, r, http.StatusNotFound, "template not found")
			return
		}
		s.log.Error("bulk_instantiate_template", "err", err)
		writeErr(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	var in bulkInstantiateInput
	if !readJSON(w, r, &in) {
		return
	}
	const maxInstances = 100
	if len(in.Instances) == 0 {
		writeErr(w, r, http.StatusBadRequest, "instances is required and must not be empty")
		return
	}
	if len(in.Instances) > maxInstances {
		writeErr(w, r, http.StatusBadRequest, fmt.Sprintf("at most %d instances per request", maxInstances))
		return
	}

	// Validate all before writing any.
	type prepared struct {
		monitor *store.Monitor
	}
	var allProblems []FieldError
	preps := make([]prepared, len(in.Instances))
	for i, inst := range in.Instances {
		m, problems := s.buildMonitorFromTemplate(tmpl, inst)
		if len(problems) > 0 {
			for _, p := range problems {
				allProblems = append(allProblems, FieldError{
					Field:  fmt.Sprintf("instances[%d].%s", i, p.Field),
					Reason: p.Reason,
				})
			}
		}
		preps[i] = prepared{monitor: m}
	}
	if len(allProblems) > 0 {
		writeValidation(w, r, allProblems)
		return
	}

	type result struct {
		Name string `json:"name"`
		ID   int64  `json:"id"`
	}
	results := make([]result, 0, len(preps))
	for i, p := range preps {
		if err := s.store.CreateMonitor(r.Context(), p.monitor); err != nil {
			s.log.Error("bulk_instantiate_template", "instance", i, "err", err)
			writeErr(w, r, http.StatusInternalServerError, fmt.Sprintf("failed creating instance %d", i))
			return
		}
		if len(tmpl.ChannelIDs) > 0 {
			_ = s.store.SetMonitorChannels(r.Context(), p.monitor.ID, tmpl.ChannelIDs)
		}
		for _, tr := range tmpl.Rules {
			params := substituteParams(tr.Params, in.Instances[i].Parameters)
			if err := s.registry.Validate(tr.Type, params); err != nil {
				continue
			}
			rule := store.Rule{MonitorID: p.monitor.ID, Type: tr.Type, Params: params, Enabled: true}
			_ = s.store.CreateRule(r.Context(), &rule)
		}
		results = append(results, result{Name: p.monitor.Name, ID: p.monitor.ID})
	}
	writeJSON(w, http.StatusCreated, map[string]any{"created": results})
}

func (s *Server) buildMonitorFromTemplate(tmpl *store.MonitorTemplate, in instantiateInput) (*store.Monitor, []FieldError) {
	var problems []FieldError
	name := in.Name
	if name == "" {
		problems = append(problems, FieldError{Field: "name", Reason: "required"})
	}
	if len(in.ContractIDs) == 0 {
		problems = append(problems, FieldError{Field: "contract_ids", Reason: "at least one contract ID is required"})
	}
	for _, p := range tmpl.Parameters {
		if p.Required {
			if _, ok := in.Parameters[p.Name]; !ok {
				if p.Default == "" {
					problems = append(problems, FieldError{
						Field:  "parameters." + p.Name,
						Reason: "required parameter",
					})
				}
			}
		}
	}
	m := &store.Monitor{
		Name:        name,
		ContractIDs: in.ContractIDs,
		Enabled:     true,
	}
	return m, problems
}

func substituteParams(raw json.RawMessage, params map[string]string) json.RawMessage {
	s := string(raw)
	for k, v := range params {
		s = strings.ReplaceAll(s, "{{"+k+"}}", v)
	}
	return json.RawMessage(s)
}
