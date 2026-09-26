package web

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// ruleBuilderFields returns an htmx fragment with form fields for the selected
// rule type. Rule types without a declared schema get an empty response so the
// client keeps the JSON textarea visible.
func (s *Server) ruleBuilderFields(w http.ResponseWriter, r *http.Request) {
	ruleType := chi.URLParam(r, "type")
	schema := s.registry.ParamSchema(ruleType)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if schema == nil {
		return
	}
	for _, f := range schema {
		req := ""
		if f.Required {
			req = " required"
		}
		switch f.Type {
		case "select":
			fmt.Fprintf(w, `<label for="param_%s">%s</label>`, f.Name, f.Description)
			fmt.Fprintf(w, `<select id="param_%s" name="param_%s"%s>`, f.Name, f.Name, req)
			fmt.Fprint(w, `<option value="">--</option>`)
			for _, opt := range f.Options {
				fmt.Fprintf(w, `<option value="%s">%s</option>`, opt, opt)
			}
			fmt.Fprint(w, `</select>`)
		case "number":
			fmt.Fprintf(w, `<label for="param_%s">%s</label>`, f.Name, f.Description)
			fmt.Fprintf(w, `<input type="number" id="param_%s" name="param_%s" step="any"%s>`, f.Name, f.Name, req)
		case "object":
			fmt.Fprintf(w, `<label for="param_%s">%s</label>`, f.Name, f.Description)
			fmt.Fprintf(w, `<textarea id="param_%s" name="param_%s" rows="2" placeholder="{}"></textarea>`, f.Name, f.Name)
		default:
			fmt.Fprintf(w, `<label for="param_%s">%s</label>`, f.Name, f.Description)
			fmt.Fprintf(w, `<input type="text" id="param_%s" name="param_%s"%s>`, f.Name, f.Name, req)
		}
	}
}

// createRuleFromBuilder handles the form-based rule creation. It reads the
// builder form fields, constructs JSON params, validates them against the
// registry, and creates the rule.
func (s *Server) createRuleFromBuilder(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ruleType := r.FormValue("type")

	schema := s.registry.ParamSchema(ruleType)
	var params json.RawMessage
	if schema != nil && r.FormValue("use_builder") == "true" {
		values := make(map[string]string, len(schema))
		for _, f := range schema {
			v := r.FormValue("param_" + f.Name)
			if v != "" {
				values[f.Name] = v
			}
		}
		// Handle object-type fields specially: parse as raw JSON.
		m := make(map[string]any, len(schema))
		raw := rules.ParamsFromForm(schema, values)
		_ = json.Unmarshal(raw, &m)
		for _, f := range schema {
			if f.Type == "object" {
				v := r.FormValue("param_" + f.Name)
				if v != "" {
					var obj any
					if err := json.Unmarshal([]byte(v), &obj); err == nil {
						m[f.Name] = obj
					}
				}
			}
		}
		params, _ = json.Marshal(m)
	} else {
		params = []byte(r.FormValue("params"))
		if len(params) == 0 {
			params = []byte(`{}`)
		}
	}

	if err := s.registry.Validate(ruleType, params); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rule := store.Rule{MonitorID: id, Type: ruleType, Params: params, Enabled: true}
	if err := s.store.CreateRule(r.Context(), &rule); err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, store.AuditActionCreate, "rule", rule.ID, "type", "params")
	http.Redirect(w, r, fmt.Sprintf("/monitors/%d", id), http.StatusSeeOther)
}
