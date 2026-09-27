package web

import (
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"strings"
)

// sep41Slots mirrors the from/to topic indexes documented on
// token_event.go. The dashboard needs the same layout so a transfer
// reads as from/to rather than "topic 1" / "topic 2".
var sep41Slots = map[string][2]int{
	"transfer":  {1, 2},
	"mint":      {1, 2},
	"burn":      {2, 1},
	"clawback":  {2, 1},
	"set_admin": {1, 2},
}

type decodedField struct {
	Label string
	Value string
}

// decodedEvent renders an alert payload as labelled rows next to the
// existing prettyJSON dump. Invalid or empty input yields nothing — the
// raw JSON is still shown, and a decode failure must not blank the page.
func decodedEvent(raw json.RawMessage) template.HTML {
	fields := decodedFields(raw)
	if len(fields) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<table class="decoded">`)
	for _, f := range fields {
		b.WriteString("<tr><th>")
		b.WriteString(html.EscapeString(f.Label))
		b.WriteString("</th><td>")
		b.WriteString(html.EscapeString(f.Value))
		b.WriteString("</td></tr>")
	}
	b.WriteString("</table>")
	return template.HTML(b.String())
}

// decodedFields turns a stored alert payload into display rows.
// SEP-41 events get from/to/amount labels; everything else falls back
// to a numbered topic list so nothing on-chain is hidden.
func decodedFields(raw json.RawMessage) []decodedField {
	if len(raw) == 0 {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	topics, _ := payload["topics"].([]any)
	value := payload["value"]
	name := eventNameFrom(payload, topics)
	if slots, ok := sep41Slots[name]; ok && len(topics) >= 3 {
		out := []decodedField{
			{Label: "event", Value: name},
			{Label: "from", Value: scValString(topics[slots[0]])},
			{Label: "to", Value: scValString(topics[slots[1]])},
		}
		if name != "set_admin" {
			if amount, ok := amountString(value); ok {
				out = append(out, decodedField{Label: "amount", Value: amount})
			} else if value != nil {
				out = append(out, decodedField{Label: "value", Value: scValString(value)})
			}
		}
		return out
	}
	out := make([]decodedField, 0, len(topics)+2)
	if name != "" {
		out = append(out, decodedField{Label: "event", Value: name})
	}
	for i, t := range topics {
		out = append(out, decodedField{Label: fmt.Sprintf("%d", i), Value: scValString(t)})
	}
	if value != nil {
		out = append(out, decodedField{Label: "value", Value: scValString(value)})
	}
	return out
}

func eventNameFrom(payload map[string]any, topics []any) string {
	if len(topics) > 0 {
		if n := scValSymbol(topics[0]); n != "" {
			return n
		}
	}
	if s, ok := payload["event_name"].(string); ok {
		return s
	}
	return ""
}

func scValSymbol(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if m, ok := v.(map[string]any); ok {
		if s, ok := m["symbol"].(string); ok {
			return s
		}
	}
	return ""
}

// scValString flattens a decoded ScVal for a table cell. Both the local
// XDR path (bare strings) and the RPC json wrapper ({"address":...},
// {"symbol":...}, {"i128":...}) collapse to the inner value.
func scValString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if m, ok := v.(map[string]any); ok {
		for _, k := range []string{"address", "symbol", "i128", "u128", "u64", "i64", "bytes", "string"} {
			if s, ok := m[k].(string); ok {
				return s
			}
			if f, ok := m[k].(float64); ok {
				return fmt.Sprintf("%.0f", f)
			}
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func amountString(value any) (string, bool) {
	m, ok := value.(map[string]any)
	if !ok {
		return "", false
	}
	if s, ok := m["i128"].(string); ok {
		return s, true
	}
	if f, ok := m["i128"].(float64); ok {
		return fmt.Sprintf("%.0f", f), true
	}
	return "", false
}
