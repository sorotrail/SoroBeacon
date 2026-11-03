package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// defaultBuilderParams is the text the raw-JSON box starts with, so the
// advanced path has something to edit.
const defaultBuilderParams = `{"event_name": "transfer"}`

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
	fmt.Fprint(w, builderFields(schema, nil))
}

// builderFields renders one form field per schema entry. values carries what
// the user already typed (nil for the htmx fragment, which is fetched before
// anything is typed); a field they filled in comes back with that value in it,
// so a rejected form does not empty itself.
func builderFields(schema []rules.FieldSchema, values map[string]string) string {
	var b strings.Builder
	for _, f := range schema {
		req := ""
		if f.Required {
			req = " required"
		}
		// A field with a documented default starts there instead of on the
		// empty option, so the form shows the same value the evaluator will
		// use when the user leaves it alone. A submitted value wins.
		value := values[f.Name]
		if value == "" && f.Default != "" {
			value = f.Default
		}
		switch f.Type {
		case "select", "boolean":
			options := f.Options
			if f.Type == "boolean" {
				options = []string{"true", "false"}
			}
			fmt.Fprintf(&b, `<label for="param_%s">%s</label>`, f.Name, f.Description)
			fmt.Fprintf(&b, `<select id="param_%s" name="param_%s"%s>`, f.Name, f.Name, req)
			fmt.Fprint(&b, `<option value="">--</option>`)
			for _, opt := range options {
				sel := ""
				if opt == value {
					sel = " selected"
				}
				fmt.Fprintf(&b, `<option value="%s"%s>%s</option>`, opt, sel, opt)
			}
			fmt.Fprint(&b, `</select>`)
		case "number":
			fmt.Fprintf(&b, `<label for="param_%s">%s</label>`, f.Name, f.Description)
			fmt.Fprintf(&b, `<input type="number" id="param_%s" name="param_%s" step="any" value="%s"%s>`,
				f.Name, f.Name, html.EscapeString(value), req)
		case "object":
			fmt.Fprintf(&b, `<label for="param_%s">%s</label>`, f.Name, f.Description)
			fmt.Fprintf(&b, `<textarea id="param_%s" name="param_%s" rows="2" placeholder="{}">%s</textarea>`,
				f.Name, f.Name, html.EscapeString(value))
		default:
			fmt.Fprintf(&b, `<label for="param_%s">%s</label>`, f.Name, f.Description)
			fmt.Fprintf(&b, `<input type="text" id="param_%s" name="param_%s" value="%s"%s>`,
				f.Name, f.Name, html.EscapeString(value), req)
		}
	}
	return b.String()
}

// createRuleFromBuilder handles the form-based rule creation. It reads the
// builder form fields, constructs JSON params, validates them against the
// registry, and creates the rule.
//
// Malformed input is reported field by field and the form comes back with the
// user's own values still in it. The builder used to swallow the mistake: an
// unparseable number took the whole params blob down with it, and an object
// field that was not valid JSON was quietly left as the text the user typed,
// so the registry then complained that a field they had filled in was missing.
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
			if v := r.FormValue("param_" + f.Name); v != "" {
				values[f.Name] = v
			}
		}
		raw, err := rules.ParamsFromForm(schema, values)
		if err != nil {
			s.renderRuleBuilderError(w, r, id, ruleType, values, builderProblems(err))
			return
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			// ParamsFromForm builds the blob itself, so this can only happen
			// if the schema grows a field it cannot encode.
			s.renderRuleBuilderError(w, r, id, ruleType, values,
				rules.FieldErrors{{Reason: "params could not be read"}})
			return
		}
		// Object fields hold JSON the user typed by hand, so they are parsed
		// here rather than by ParamsFromForm. A field that is not valid JSON
		// is named in the error instead of being left in the params as the
		// text the user typed, which the registry would report as a missing
		// or wrongly typed field.
		var problems rules.FieldErrors
		for _, f := range schema {
			if f.Type != "object" {
				continue
			}
			v := r.FormValue("param_" + f.Name)
			if v == "" {
				continue
			}
			var obj any
			if err := json.Unmarshal([]byte(v), &obj); err != nil {
				problems = append(problems, rules.FieldError{Field: f.Name, Reason: "is not valid JSON"})
				continue
			}
			m[f.Name] = obj
		}
		if len(problems) > 0 {
			s.renderRuleBuilderError(w, r, id, ruleType, values, problems)
			return
		}
		b, err := json.Marshal(m)
		if err != nil {
			s.renderRuleBuilderError(w, r, id, ruleType, values,
				rules.FieldErrors{{Reason: "params could not be encoded"}})
			return
		}
		params = b
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
	rawRunbook := r.FormValue("runbook_url")
	if rawRunbook == "" {
		rawRunbook = r.FormValue("param_runbook_url")
	}
	rawGuidance := r.FormValue("response_guidance")
	if rawGuidance == "" {
		rawGuidance = r.FormValue("param_response_guidance")
	}
	var guidanceProblems rules.FieldErrors
	runbookURL, err := rules.ValidateRunbookURL(rawRunbook)
	if err != nil {
		guidanceProblems = append(guidanceProblems, builderProblems(err)...)
	}
	responseGuidance, err := rules.ValidateResponseGuidance(rawGuidance)
	if err != nil {
		guidanceProblems = append(guidanceProblems, builderProblems(err)...)
	}
	if len(guidanceProblems) > 0 {
		if schema != nil && r.FormValue("use_builder") == "true" {
			values := make(map[string]string, len(schema))
			for _, f := range schema {
				if v := r.FormValue("param_" + f.Name); v != "" {
					values[f.Name] = v
				}
			}
			s.renderRuleBuilderError(w, r, id, ruleType, values, guidanceProblems)
			return
		}
		http.Error(w, guidanceProblems.Error(), http.StatusBadRequest)
		return
	}
	if pURL, pGuidance, perr := rules.ParseGuidance(params); perr == nil {
		if runbookURL == "" {
			runbookURL = pURL
		}
		if responseGuidance == "" {
			responseGuidance = pGuidance
		}
	}
	rule := store.Rule{
		MonitorID:        id,
		Type:             ruleType,
		Params:           params,
		Enabled:          true,
		RunbookURL:       runbookURL,
		ResponseGuidance: responseGuidance,
	}
	if err := s.store.CreateRule(r.Context(), &rule); err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, store.AuditActionCreate, "rule", rule.ID, "type", "params")
	http.Redirect(w, r, fmt.Sprintf("/monitors/%d", id), http.StatusSeeOther)
}

// builderProblems turns a params error into the field problems the builder
// reports by name. An error that names no field (a failure to encode, say) is
// reported as one unnamed problem rather than dropped.
func builderProblems(err error) rules.FieldErrors {
	var fields rules.FieldErrors
	if errors.As(err, &fields) && len(fields) > 0 {
		return fields
	}
	var one rules.FieldError
	if errors.As(err, &one) && (one.Field != "" || one.Reason != "") {
		return rules.FieldErrors{one}
	}
	return rules.FieldErrors{{Reason: err.Error()}}
}

// renderRuleBuilderError re-renders the monitor page with the builder form
// filled in as the user left it and the malformed fields named. The page is
// the same one the form lives on, so the user sees their input next to the
// complaint instead of a bare error page.
func (s *Server) renderRuleBuilderError(
	w http.ResponseWriter, r *http.Request, monitorID int64, ruleType string,
	values map[string]string, problems rules.FieldErrors,
) {
	data, err := s.monitorPage(r, monitorID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.notFound(w, r)
			return
		}
		s.fail(w, err)
		return
	}
	messages := make([]string, 0, len(problems))
	for _, p := range problems {
		if p.Field == "" {
			messages = append(messages, p.Reason)
			continue
		}
		messages = append(messages, p.Field+" "+p.Reason)
	}
	data["BuilderType"] = ruleType
	// The fragment is markup, and every value in it was escaped when it was
	// rendered, so it is marked as such rather than escaped a second time.
	data["BuilderFields"] = template.HTML(builderFields(s.registry.ParamSchema(ruleType), values))
	data["BuilderRawParams"] = r.FormValue("params")
	data["BuilderRunbookURL"] = r.FormValue("runbook_url")
	data["BuilderResponseGuidance"] = r.FormValue("response_guidance")
	data["BuilderErrors"] = messages
	s.renderStatus(w, r, http.StatusBadRequest, "monitor", data)
}
