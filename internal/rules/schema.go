package rules

import "encoding/json"

// FieldSchema describes one parameter field a rule type accepts, used by the
// interactive rule builder to generate a form instead of requiring raw JSON.
type FieldSchema struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"` // "string", "number", "object", "select"
	Required    bool     `json:"required"`
	Description string   `json:"description"`
	Options     []string `json:"options,omitempty"`
	Default     string   `json:"default,omitempty"`
}

// SchemaProvider is implemented by evaluators that can describe their params
// so the dashboard can render a form instead of a JSON textarea. Evaluators
// that do not implement it fall back to the textarea.
type SchemaProvider interface {
	ParamSchema() []FieldSchema
}

// ParamSchema returns the schema for a rule type, or nil when the evaluator
// does not declare one (the builder falls back to the JSON textarea).
func (r *Registry) ParamSchema(ruleType string) []FieldSchema {
	e, ok := r.evaluators[ruleType]
	if !ok {
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
	out := make(map[string][]FieldSchema, len(r.evaluators))
	for name, e := range r.evaluators {
		if sp, ok := e.(SchemaProvider); ok {
			out[name] = sp.ParamSchema()
		}
	}
	return out
}

// ParamsFromForm builds a JSON params blob from form key-value pairs using the
// schema to coerce types. Unknown keys are ignored; missing required fields
// produce an empty value (validation catches them later).
func ParamsFromForm(schema []FieldSchema, values map[string]string) json.RawMessage {
	m := make(map[string]any, len(schema))
	for _, f := range schema {
		v, ok := values[f.Name]
		if !ok || v == "" {
			continue
		}
		switch f.Type {
		case "number":
			n := json.Number(v)
			m[f.Name] = n
		default:
			m[f.Name] = v
		}
	}
	b, _ := json.Marshal(m)
	return b
}

// Implement SchemaProvider on built-in evaluators.

func (EventEmitted) ParamSchema() []FieldSchema {
	return []FieldSchema{
		{Name: "event_name", Type: "string", Description: "Event name to match (first topic)"},
		{Name: "topic_equals", Type: "object", Description: "Topic index to expected value map (JSON)"},
	}
}

func (ValueThreshold) ParamSchema() []FieldSchema {
	return []FieldSchema{
		{Name: "event_name", Type: "string", Description: "Only consider events with this name"},
		{Name: "value_path", Type: "string", Description: "Dot path into the event value"},
		{Name: "comparison", Type: "select", Required: true, Description: "Comparison operator", Options: []string{"gt", "gte", "lt", "lte", "eq", "neq"}},
		{Name: "threshold", Type: "number", Required: true, Description: "Threshold value (number or numeric string for >53-bit)"},
	}
}

func (TokenEvent) ParamSchema() []FieldSchema {
	return []FieldSchema{
		{Name: "event", Type: "select", Required: true, Description: "SEP-41 event type", Options: []string{"transfer", "mint", "burn", "clawback", "set_admin", "*"}},
		{Name: "from", Type: "string", Description: "Exact address in the from slot"},
		{Name: "to", Type: "string", Description: "Exact address in the to slot"},
		{Name: "min_amount", Type: "string", Description: "Minimum amount (decimal integer string)"},
		{Name: "max_amount", Type: "string", Description: "Maximum amount (decimal integer string)"},
	}
}

func (*FrequencyThreshold) ParamSchema() []FieldSchema {
	return []FieldSchema{
		{Name: "event_name", Type: "string", Description: "Only count events with this name"},
		{Name: "count", Type: "number", Required: true, Description: "Fire at this many matches"},
		{Name: "window", Type: "string", Required: true, Description: "Rolling window duration (e.g. 5m, 1h)"},
	}
}

func (*TopicRegex) ParamSchema() []FieldSchema {
	return []FieldSchema{
		{Name: "pattern", Type: "string", Required: true, Description: "Regular expression matched against a topic (e.g. ^swap_)"},
		{Name: "position", Type: "number", Description: "Topic position to match (0 is the event name); omitted matches any topic"},
	}
}

func (*AddressWatchlist) ParamSchema() []FieldSchema {
	return []FieldSchema{
		{Name: "addresses", Type: "object", Required: true, Description: "Watchlist of Stellar addresses (JSON array)"},
		{Name: "match", Type: "select", Description: "Which address slot(s) to watch", Options: []string{"from", "to", "either"}, Default: "either"},
		{Name: "event", Type: "select", Description: "Restrict to one SEP-41 event", Options: []string{"transfer", "mint", "burn", "clawback", "set_admin", "*"}},
	}
}
