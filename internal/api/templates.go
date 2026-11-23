package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// --- monitor templates: one-time copy model ---
//
// Instantiation copies the template's rules and channels into a new monitor.
// Editing a template does not retroactively update existing monitors; deleting
// a template leaves its instances untouched.
//
// Instantiation is all-or-nothing. The template's rules are checked after
// their parameters are substituted, the channel ids are checked against the
// caller's channels, and the monitor, its rules and its channel links are
// written in one transaction. A caller that gets a monitor back has a monitor
// with every rule it asked for: a monitor that watches a contract and never
// says anything is an alerting gap nobody is told about.

type templateInput struct {
	Name        string                      `json:"name"`
	Description string                      `json:"description"`
	Rules       []store.MonitorTemplateRule `json:"rules"`
	ChannelIDs  []int64                     `json:"channel_ids"`
	Parameters  []store.TemplateParameter   `json:"parameters"`
}

type instantiateInput struct {
	Name        string   `json:"name"`
	ContractIDs []string `json:"contract_ids"`
	// Network names the chain the instantiated monitor watches. A pointer so
	// an omitted field (the primary, the only chain a single-network instance
	// polls) stays distinguishable from one sent explicitly empty, which is a
	// 400 rather than a silent default.
	Network    *string           `json:"network"`
	Parameters map[string]string `json:"parameters"`
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
	if len(in.Description) > MaxTemplateBytes {
		problems = append(problems, FieldError{
			Field:  "description",
			Reason: fmt.Sprintf("description exceeds maximum size of %d bytes", MaxTemplateBytes),
		})
		if m := metricsFrom(r.Context()); m != nil {
			m.RecordBodyRejection("template_size")
		}
	}
	problems = append(problems, s.validateTemplateRules(in.Rules)...)
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
		if errors.Is(err, store.ErrNotFound) {
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
		if errors.Is(err, store.ErrNotFound) {
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
	problems := s.validateTemplateRules(existing.Rules)
	if len(existing.Description) > MaxTemplateBytes {
		problems = append(problems, FieldError{
			Field:  "description",
			Reason: fmt.Sprintf("description exceeds maximum size of %d bytes", MaxTemplateBytes),
		})
		if m := metricsFrom(r.Context()); m != nil {
			m.RecordBodyRejection("template_size")
		}
	}
	if len(problems) > 0 {
		writeValidation(w, r, problems)
		return
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
		if errors.Is(err, store.ErrNotFound) {
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
		if errors.Is(err, store.ErrNotFound) {
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
	monitor, rulesToCreate, problems, err := s.buildMonitorFromTemplate(r.Context(), tmpl, in)
	if err != nil {
		s.log.Error("instantiate_template", "err", err)
		writeErr(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if len(problems) > 0 {
		writeUnprocessable(w, r, problems)
		return
	}
	// One transaction: the monitor, its rules and its channel links either
	// all land or none of them do.
	if err := s.store.CreateMonitorWithRules(r.Context(), monitor, rulesToCreate, tmpl.ChannelIDs); err != nil {
		s.log.Error("instantiate_template", "err", err)
		writeErr(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	monitor.ChannelIDs = tmpl.ChannelIDs
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
		if errors.Is(err, store.ErrNotFound) {
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
		rules   []*store.Rule
	}
	var allProblems []FieldError
	preps := make([]prepared, len(in.Instances))
	for i, inst := range in.Instances {
		m, instRules, problems, err := s.buildMonitorFromTemplate(r.Context(), tmpl, inst)
		if err != nil {
			s.log.Error("bulk_instantiate_template", "err", err)
			writeErr(w, r, http.StatusInternalServerError, "internal error")
			return
		}
		for _, p := range problems {
			allProblems = append(allProblems, FieldError{
				Field:  fmt.Sprintf("instances[%d].%s", i, p.Field),
				Reason: p.Reason,
			})
		}
		preps[i] = prepared{monitor: m, rules: instRules}
	}
	if len(allProblems) > 0 {
		writeUnprocessable(w, r, allProblems)
		return
	}

	// Each instance is written in its own transaction, so an instance is
	// never half written. The request as a whole is not one transaction: if
	// instance 7 of 10 fails, the six before it stay, and the response says
	// which instance failed. Retrying the request creates the missing ones
	// again, so a client that wants all-or-nothing should instantiate one
	// instance at a time.
	type result struct {
		Name string `json:"name"`
		ID   int64  `json:"id"`
	}
	results := make([]result, 0, len(preps))
	for i, p := range preps {
		if err := s.store.CreateMonitorWithRules(r.Context(), p.monitor, p.rules, tmpl.ChannelIDs); err != nil {
			s.log.Error("bulk_instantiate_template", "instance", i, "err", err)
			writeErr(w, r, http.StatusInternalServerError, fmt.Sprintf("failed creating instance %d", i))
			return
		}
		results = append(results, result{Name: p.monitor.Name, ID: p.monitor.ID})
	}
	writeJSON(w, http.StatusCreated, map[string]any{"created": results})
}

// buildMonitorFromTemplate turns a template and the values it was given into
// the monitor, the rules and the channel links to write. Everything is
// checked first: a rule whose substituted params do not validate, a parameter
// that is missing or unknown, a channel that does not exist. The problems come
// back for the caller to report, and nothing is written while any of them
// stands.
//
// The rules that come back are ready to be written: their params are
// substituted and their type is known to the registry.
func (s *Server) buildMonitorFromTemplate(
	ctx context.Context, tmpl *store.MonitorTemplate, in instantiateInput,
) (*store.Monitor, []*store.Rule, []FieldError, error) {
	var problems []FieldError
	name := in.Name
	if name == "" {
		problems = append(problems, FieldError{Field: "name", Reason: "required"})
	}
	if len(in.ContractIDs) == 0 {
		problems = append(problems, FieldError{Field: "contract_ids", Reason: "at least one contract ID is required"})
	}
	network, netErr := s.networkDetail(in.Network)
	if netErr != nil {
		problems = append(problems, *netErr)
	}

	values, paramProblems := templateParameters(tmpl, in.Parameters)
	problems = append(problems, paramProblems...)

	rulesToCreate, ruleProblems := s.templateRules(tmpl, values)
	problems = append(problems, ruleProblems...)

	channelProblems, err := s.templateChannelProblems(ctx, tmpl.ChannelIDs)
	if err != nil {
		return nil, nil, nil, err
	}
	problems = append(problems, channelProblems...)

	m := &store.Monitor{
		Name:        name,
		ContractIDs: in.ContractIDs,
		Enabled:     true,
		Network:     network,
	}
	sortProblems(problems)
	return m, rulesToCreate, problems, nil
}

// templateParameters resolves the values the template's placeholders take:
// the ones the request supplied, or the template's own default for the rest.
//
// Three things are reported, because each of them leaves a placeholder
// unsubstituted and the rule it belongs to unable to validate:
//
//   - a parameter the request supplies that the template does not declare
//     (a typo in the request),
//   - a placeholder the template's rules use that the template does not
//     declare (a typo in the template),
//   - a required parameter with no value and no default.
func templateParameters(tmpl *store.MonitorTemplate, supplied map[string]string) (map[string]string, []FieldError) {
	declared := make(map[string]store.TemplateParameter, len(tmpl.Parameters))
	for _, p := range tmpl.Parameters {
		declared[p.Name] = p
	}
	var problems []FieldError
	values := make(map[string]string, len(declared))
	for name, p := range declared {
		if v, ok := supplied[name]; ok {
			values[name] = v
			continue
		}
		if p.Default != "" {
			values[name] = p.Default
			continue
		}
		if p.Required {
			problems = append(problems, FieldError{
				Field:  "parameters." + name,
				Reason: "required parameter",
			})
		}
	}
	for name := range supplied {
		if _, ok := declared[name]; !ok {
			problems = append(problems, FieldError{
				Field:  "parameters." + name,
				Reason: "unknown parameter",
			})
		}
	}
	for _, name := range templatePlaceholders(tmpl) {
		if _, ok := declared[name]; !ok {
			problems = append(problems, FieldError{
				Field:  "parameters." + name,
				Reason: "not a parameter of this template",
			})
		}
	}
	return values, problems
}

// templateRules substitutes the values into the template's rules and checks
// each one. A rule that does not validate is reported by its index in the
// template, so the reply says which rule of the template is at fault rather
// than just that something was wrong.
func (s *Server) templateRules(tmpl *store.MonitorTemplate, values map[string]string) ([]*store.Rule, []FieldError) {
	var problems []FieldError
	out := make([]*store.Rule, 0, len(tmpl.Rules))
	for i, tr := range tmpl.Rules {
		prefix := fmt.Sprintf("rules[%d]", i)
		if !s.knownRuleType(tr.Type) {
			problems = append(problems, FieldError{
				Field:  prefix + ".type",
				Reason: "unknown rule type",
			})
			continue
		}
		params := substituteParams(tr.Params, values)
		if !json.Valid(params) {
			problems = append(problems, FieldError{
				Field:  prefix + ".params",
				Reason: "is not valid JSON once the parameters are substituted",
			})
			continue
		}
		if err := s.registry.Validate(tr.Type, params); err != nil {
			problems = append(problems, detailsFromErr(prefix+".params", err)...)
			continue
		}
		out = append(out, &store.Rule{Type: tr.Type, Params: params, Enabled: true})
	}
	return out, problems
}

// templateChannelProblems checks that every channel the template attaches
// exists in the caller's workspace. A channel that does not exist is
// reported rather than dropped: a monitor that is not attached to the
// channel it was supposed to alert to is a monitor nobody hears from.
func (s *Server) templateChannelProblems(ctx context.Context, channelIDs []int64) ([]FieldError, error) {
	if len(channelIDs) == 0 {
		return nil, nil
	}
	channels, err := s.store.ListChannels(ctx, false)
	if err != nil {
		return nil, err
	}
	known := make(map[int64]bool, len(channels))
	for _, c := range channels {
		known[c.ID] = true
	}
	var problems []FieldError
	for i, id := range channelIDs {
		if known[id] {
			continue
		}
		problems = append(problems, FieldError{
			Field:  fmt.Sprintf("channel_ids[%d]", i),
			Reason: "channel not found",
		})
	}
	return problems, nil
}

// validateTemplateRules checks the rules of a template that is being created
// or updated, without knowing the values its parameters will take.
//
// The type has to be one the registry knows, and the params have to be JSON:
// a template that cannot produce a valid rule fails here rather than at every
// instantiation, where the failure used to be silent.
//
// The value of a parameter is not known yet, so the params are checked
// without the fields that a parameter fills in. A problem that names one of
// those fields is not reported — it cannot be judged until the template is
// instantiated with real values.
func (s *Server) validateTemplateRules(tmplRules []store.MonitorTemplateRule) []FieldError {
	var problems []FieldError
	for i, tr := range tmplRules {
		problems = append(problems, s.validateTemplateRule(i, tr)...)
	}
	sortProblems(problems)
	return problems
}

// validateTemplateRule checks one rule of a template. The params are checked
// without the fields that a parameter fills in: a problem naming one of
// those fields is not reported, because the value is not known until the
// template is instantiated.
func (s *Server) validateTemplateRule(index int, tr store.MonitorTemplateRule) []FieldError {
	prefix := fmt.Sprintf("rules[%d]", index)
	if !s.knownRuleType(tr.Type) {
		return []FieldError{{Field: prefix + ".type", Reason: "unknown rule type"}}
	}
	fields, err := templateParamsFields(tr.Params)
	if err != nil {
		return []FieldError{{Field: prefix + ".params", Reason: err.Error()}}
	}
	dependent := make(map[string]bool, len(fields))
	fixed := make(map[string]json.RawMessage, len(fields))
	for name, value := range fields {
		if placeholderPattern.Match(value) {
			dependent[name] = true
			continue
		}
		fixed[name] = value
	}
	probe, err := json.Marshal(fixed)
	if err != nil {
		return []FieldError{{Field: prefix + ".params", Reason: "is not valid JSON"}}
	}
	err = s.registry.Validate(tr.Type, probe)
	if err == nil {
		return nil
	}
	named, ok := ruleFieldErrors(err)
	if !ok {
		return []FieldError{{Field: prefix + ".params", Reason: err.Error()}}
	}
	var problems []FieldError
	for _, f := range named {
		if dependent[paramField(f.Field)] {
			continue
		}
		problems = append(problems, FieldError{
			Field:  prefix + ".params." + f.Field,
			Reason: f.Reason,
		})
	}
	return problems
}

// knownRuleType reports whether the registry knows the rule type. The
// registry's own error says so too, but it lists every registered type, which
// is noise in a validation reply.
func (s *Server) knownRuleType(ruleType string) bool {
	for _, name := range s.registry.Types() {
		if name == ruleType {
			return true
		}
	}
	return false
}

// placeholderPattern matches the {{name}} placeholders a template's params
// may hold.
var placeholderPattern = regexp.MustCompile(`\{\{[^{}]*\}\}`)

// templatePlaceholders returns the parameter names the template's rules
// substitute, in the order they first appear.
func templatePlaceholders(tmpl *store.MonitorTemplate) []string {
	var out []string
	seen := map[string]bool{}
	for _, tr := range tmpl.Rules {
		for _, m := range placeholderPattern.FindAllStringSubmatch(string(tr.Params), -1) {
			name := strings.TrimSpace(strings.Trim(m[0], "{}"))
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// templateParamsFields reads a template rule's params as a JSON object. The
// empty object is returned for params that are empty, so a rule without
// params is checked rather than skipped.
func templateParamsFields(raw json.RawMessage) (map[string]json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if len(raw) == 0 {
		return fields, nil
	}
	if !json.Valid(raw) {
		return nil, errors.New("is not valid JSON")
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, errors.New("is not a JSON object")
	}
	return fields, nil
}

// ruleFieldErrors returns the params fields a validation error names, so the
// caller can tell which problems belong to a field it substituted.
func ruleFieldErrors(err error) (rules.FieldErrors, bool) {
	var fields rules.FieldErrors
	if errors.As(err, &fields) && len(fields) > 0 {
		return fields, true
	}
	var one rules.FieldError
	if errors.As(err, &one) && (one.Field != "" || one.Reason != "") {
		return rules.FieldErrors{one}, true
	}
	return nil, false
}

// paramField is the params field a validation error names: the first segment
// of its path, so "topic_equals.0" is about "topic_equals".
func paramField(field string) string {
	if i := strings.IndexByte(field, '.'); i >= 0 {
		return field[:i]
	}
	return field
}

// sortProblems orders the problems by field so the reply is the same for the
// same request, whatever order the checks ran in.
func sortProblems(problems []FieldError) {
	sort.SliceStable(problems, func(i, j int) bool {
		return problems[i].Field < problems[j].Field
	})
}

// substituteParams replaces the {{name}} placeholders in a rule's params with
// the values they were given. The keys are substituted in a fixed order, so
// the same request always produces the same params.
func substituteParams(raw json.RawMessage, params map[string]string) json.RawMessage {
	s := string(raw)
	names := make([]string, 0, len(params))
	for k := range params {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		s = strings.ReplaceAll(s, "{{"+k+"}}", params[k])
	}
	return json.RawMessage(s)
}
