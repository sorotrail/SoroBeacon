package rules

import (
	"encoding/json"
	"strconv"
)

// FieldSchema describes one parameter field a rule type accepts, used by the
// interactive rule builder to generate a form instead of requiring raw JSON.
type FieldSchema struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"` // "string", "number", "boolean", "object", "select"
	Required    bool     `json:"required"`
	Description string   `json:"description"`
	Options     []string `json:"options,omitempty"`
	Default     string   `json:"default,omitempty"`
}

// SchemaProvider is implemented by evaluators that can describe their params
// so the dashboard can render a form instead of a JSON textarea. Evaluators
// that do not implement it fall back to the textarea.
type SchemaProvider interface {
	// ParamSchema describes the parameter fields supported by this evaluator.
	ParamSchema() []FieldSchema
}

// ParamSchema returns the schema for a rule type, or nil when the evaluator
// does not declare one (the builder falls back to the JSON textarea).
func (r *Registry) ParamSchema(ruleType string) []FieldSchema {
	// Absence rule types are registered separately but describe their params
	// the same way, so the builder renders a form for them too.
	var e any
	if ev, ok := r.evaluators[ruleType]; ok {
		e = ev
	} else if ab, ok := r.absences[ruleType]; ok {
		e = ab
	} else {
		return nil
	}
	provider, ok := e.(SchemaProvider)
	if !ok {
		return nil
	}
	return provider.ParamSchema()
}

// AllSchemas returns schemas keyed by rule type name, for the builder endpoint.
func (r *Registry) AllSchemas() map[string][]FieldSchema {
	out := make(map[string][]FieldSchema, len(r.evaluators)+len(r.absences))
	for name, e := range r.evaluators {
		if sp, ok := e.(SchemaProvider); ok {
			out[name] = sp.ParamSchema()
		}
	}
	for name, a := range r.absences {
		if sp, ok := a.(SchemaProvider); ok {
			out[name] = sp.ParamSchema()
		}
	}
	return out
}

// ParamsFromForm builds a JSON params blob from form key-value pairs using the
// schema to coerce types. Unknown keys are ignored; missing required fields
// produce an empty value (validation catches them later).
//
// A number field that does not hold a number is reported by name instead of
// being dropped, and a boolean field must parse as true or false. The builder
// used to put the text into the map regardless,
// ignore the error json.Marshal returned for it, and hand back no params at
// all — so one typo in one field lost every other field the user had filled
// in, and the dashboard then complained about a field they had never touched.
func ParamsFromForm(schema []FieldSchema, values map[string]string) (json.RawMessage, error) {
	m := make(map[string]any, len(schema))
	var problems FieldErrors
	for _, f := range schema {
		v, ok := values[f.Name]
		if !ok || v == "" {
			continue
		}
		switch f.Type {
		case "number":
			// json.Number carries the text through unchanged, so an integer
			// larger than float64 can hold survives the round trip. The
			// validity check is the encoder's own, so a value that could
			// never be encoded is reported here rather than taking the whole
			// params blob down with it.
			if !isJSONNumber(v) {
				problems = append(problems, FieldError{Field: f.Name, Reason: "must be a number"})
				continue
			}
			m[f.Name] = json.Number(v)
		case "boolean":
			// The dashboard renders a true/false select, so the value is
			// still text at this point. Writing it through unchanged would
			// produce {"exclude":"false"}, which the evaluators cannot
			// unmarshal into a bool.
			b, err := strconv.ParseBool(v)
			if err != nil {
				problems = append(problems, FieldError{Field: f.Name, Reason: "must be true or false"})
				continue
			}
			m[f.Name] = b
		default:
			m[f.Name] = v
		}
	}
	if len(problems) > 0 {
		return nil, problems
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// isJSONNumber reports whether s is a JSON number literal. "50" and
// "123456789012345678901234567890" are numbers; "5x", "1,5" and "" are not.
func isJSONNumber(s string) bool {
	_, err := json.Marshal(json.Number(s))
	return err == nil
}

// Implement SchemaProvider on built-in evaluators.

func (EventEmitted) ParamSchema() []FieldSchema {
	return []FieldSchema{
		{Name: "event_name", Type: "string", Description: "Event name to match (first topic)"},
		{Name: "topic_equals", Type: "object", Description: "Topic index to expected value map (JSON)"},
		{Name: CooldownParam, Type: "string", Description: "Suppress repeat alerts for this duration"},
	}
}

func (*ContractAllowlist) ParamSchema() []FieldSchema {
	return []FieldSchema{
		{Name: "contract_ids", Type: "object", Required: true, Description: "Allowlisted contract IDs (JSON array); most useful AND-combined with a payload rule inside a composite rule"},
		{Name: "exclude", Type: "boolean", Description: "Invert into a denylist (default false)", Default: "false"},
	}
}

func (TokenSupplyChange) ParamSchema() []FieldSchema {
	return []FieldSchema{
		{Name: "direction", Type: "select", Description: "Supply direction to match (default any)", Options: []string{"mint", "burn", "any"}, Default: "any"},
		{Name: "min_amount", Type: "string", Description: "Minimum amount (decimal integer string)"},
	}
}

func (ValueThreshold) ParamSchema() []FieldSchema {
	return []FieldSchema{
		{Name: "event_name", Type: "string", Description: "Only consider events with this name"},
		{Name: "value_path", Type: "string", Description: "Dot path into the event value"},
		{Name: "comparison", Type: "select", Required: true, Description: "Comparison operator", Options: []string{"gt", "gte", "lt", "lte", "eq", "neq"}},
		{Name: "threshold", Type: "number", Required: true, Description: "Threshold value (number or numeric string for >53-bit)"},
		{Name: CooldownParam, Type: "string", Description: "Suppress repeat alerts for this duration"},
	}
}

func (EventNameGlob) ParamSchema() []FieldSchema {
	return []FieldSchema{
		{Name: "patterns", Type: "object", Required: true, Description: "Glob patterns matched against the whole event name (JSON array, e.g. [\"swap_*\"])"},
	}
}

func (NumericRange) ParamSchema() []FieldSchema {
	return []FieldSchema{
		{Name: "min", Type: "string", Description: "Inclusive lower bound (decimal integer string); at least one of min/max required"},
		{Name: "max", Type: "string", Description: "Inclusive upper bound (decimal integer string); at least one of min/max required"},
		{Name: "inclusive", Type: "boolean", Description: "Boundary values match (default true)", Default: "true"},
		{Name: "outside", Type: "boolean", Description: "Invert the match: fire outside the range (default false)", Default: "false"},
		{Name: "event_name", Type: "string", Description: "Only consider events with this name"},
		{Name: "value_path", Type: "string", Description: "Dot path into the event value"},
	}
}

func (TokenEvent) ParamSchema() []FieldSchema {
	return []FieldSchema{
		{Name: "event", Type: "select", Required: true, Description: "SEP-41 event type", Options: []string{"transfer", "mint", "burn", "clawback", "set_admin", "*"}},
		{Name: "from", Type: "string", Description: "Exact address in the from slot"},
		{Name: "to", Type: "string", Description: "Exact address in the to slot"},
		{Name: "min_amount", Type: "string", Description: "Minimum amount (decimal integer string)"},
		{Name: "max_amount", Type: "string", Description: "Maximum amount (decimal integer string)"},
		{Name: CooldownParam, Type: "string", Description: "Suppress repeat alerts for this duration"},
	}
}

func (*FrequencyThreshold) ParamSchema() []FieldSchema {
	return []FieldSchema{
		{Name: "event_name", Type: "string", Description: "Only count events with this name"},
		{Name: "count", Type: "number", Required: true, Description: "Fire at this many matches"},
		{Name: "window", Type: "string", Required: true, Description: "Rolling window duration (e.g. 5m, 1h)"},
		{Name: CooldownParam, Type: "string", Description: "Suppress repeat alerts for this duration"},
	}
}

func (*TopicRegex) ParamSchema() []FieldSchema {
	return []FieldSchema{
		{Name: "pattern", Type: "string", Required: true, Description: "Regular expression matched against a topic (e.g. ^swap_)"},
		{Name: "position", Type: "number", Description: "Topic position to match (0 is the event name); omitted matches any topic"},
	}
}

func (*Composite) ParamSchema() []FieldSchema {
	return []FieldSchema{
		{Name: "op", Type: "select", Required: true, Description: "How the child results combine", Options: []string{opAnd, opOr, opNot}, Default: opAnd},
		{Name: "rules", Type: "object", Required: true, Description: "Child rules as a JSON array of {\"type\": ..., \"params\": {...}}; exactly one for \"not\", at least one otherwise"},
	}
}

func (SelfTransfer) ParamSchema() []FieldSchema {
	return []FieldSchema{
		{Name: "min_amount", Type: "string", Description: "Only match self-transfers of at least this amount (decimal integer string)"},
	}
}

func (TimeWindow) ParamSchema() []FieldSchema {
	return []FieldSchema{
		{Name: "start", Type: "string", Required: true, Description: "Window start in UTC (HH:MM, e.g. 09:00)"},
		{Name: "end", Type: "string", Required: true, Description: "Window end in UTC (HH:MM); earlier than start crosses midnight"},
		{Name: "days", Type: "object", Description: "Days to match (JSON array of mon|tue|wed|thu|fri|sat|sun); omit to match every day"},
		{Name: "outside", Type: "boolean", Description: "Invert the match: fire outside the window (default false)", Default: "false"},
	}
}

func (*AddressWatchlist) ParamSchema() []FieldSchema {
	return []FieldSchema{
		{Name: "addresses", Type: "object", Required: true, Description: "Watchlist of Stellar addresses (JSON array)"},
		{Name: "match", Type: "select", Description: "Which address slot(s) to watch", Options: []string{"from", "to", "either"}, Default: "either"},
		{Name: "event", Type: "select", Description: "Restrict to one SEP-41 event", Options: []string{"transfer", "mint", "burn", "clawback", "set_admin", "*"}},
	}
}

func (Absence) ParamSchema() []FieldSchema {
	return []FieldSchema{
		{Name: "event_name", Type: "string", Required: true, Description: "The event whose absence fires the rule (matched on the first topic)"},
		{Name: "window", Type: "string", Required: true, Description: "How long silence is tolerated before alerting (Go duration, e.g. 30m)"},
	}
}
