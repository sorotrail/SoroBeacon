package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Enrichment as seen by notifications.
//
// The stored Alert.Enrichment is the operator source's JSON object plus the
// reserved _source and _stale keys the Enricher sets itself. Everything read
// from it here is untrusted text (a contract-supplied token symbol can be
// anything, including a look-alike of a well-known token), so each value is
// sanitised, bounded, and then escaped for the channel that renders it. The
// source is printed beside the value so a responder can tell supplied
// metadata from a verified fact.
//
// Amounts: the exact integer from the event payload is always shown. The
// decimal rendering is additional and is built by string arithmetic on the
// integer's digits, never through a float, so an i128 loses no precision.

const (
	maxEnrichLabelRunes  = 64
	maxEnrichSymbolRunes = 24
	maxTokenDecimals     = 38 // i128 has at most 39 digits; Stellar assets use 7
)

type enrichView struct {
	Label    string
	Symbol   string
	Decimals int // -1 when unknown
	Source   string
	Stale    bool
}

func (a Alert) enrichView() enrichView {
	v := enrichView{Decimals: -1}
	if len(a.Enrichment) == 0 {
		return v
	}
	var raw struct {
		Label    json.RawMessage `json:"label"`
		Symbol   json.RawMessage `json:"token_symbol"`
		Decimals json.RawMessage `json:"token_decimals"`
		Source   json.RawMessage `json:"_source"`
		Stale    bool            `json:"_stale"`
	}
	if err := json.Unmarshal(a.Enrichment, &raw); err != nil {
		return v
	}
	v.Label = cleanEnrichText(jsonString(raw.Label), maxEnrichLabelRunes)
	v.Symbol = cleanEnrichText(jsonString(raw.Symbol), maxEnrichSymbolRunes)
	v.Source = cleanEnrichText(jsonString(raw.Source), 32)
	v.Stale = raw.Stale
	if d, ok := jsonInt(raw.Decimals); ok && d >= 0 && d <= maxTokenDecimals {
		v.Decimals = d
	}
	return v
}

func jsonString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

func jsonInt(raw json.RawMessage) (int, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var n json.Number
	if dec.Decode(&n) != nil {
		return 0, false
	}
	i, err := n.Int64()
	if err != nil || i < 0 || i > 1000 {
		return 0, false
	}
	return int(i), true
}

// cleanEnrichText removes control and format characters (which include the
// bidirectional overrides used to make text read backwards) and bounds the
// length. Escaping for the destination happens afterwards.
func cleanEnrichText(s string, maxRunes int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > maxRunes {
		s = string([]rune(s)[:maxRunes])
	}
	return s
}

func (a Alert) escapeEnrich(s string) string {
	if a.esc == nil {
		return s
	}
	return a.esc(s)
}

// ContractLabel is the operator-supplied human label for the contract, or "".
func (a Alert) ContractLabel() string { return a.escapeEnrich(a.enrichView().Label) }

// TokenSymbol is the token symbol the source supplied, or "".
func (a Alert) TokenSymbol() string { return a.escapeEnrich(a.enrichView().Symbol) }

// EnrichmentSource names where the enrichment came from.
func (a Alert) EnrichmentSource() string { return a.escapeEnrich(a.enrichView().Source) }

// EnrichmentStale reports that the enrichment is a cached answer past its TTL
// because the source could not be reached.
func (a Alert) EnrichmentStale() bool { return a.enrichView().Stale }

// payloadInteger returns the exact decimal digits of an integer payload field.
// Both a JSON number and a numeric string are accepted, and both are read as
// text: the value never becomes a float.
func (a Alert) payloadInteger(field string) (string, bool) {
	if len(a.Payload) == 0 {
		return "", false
	}
	dec := json.NewDecoder(bytes.NewReader(a.Payload))
	dec.UseNumber()
	var m map[string]any
	if dec.Decode(&m) != nil {
		return "", false
	}
	var s string
	switch t := m[field].(type) {
	case json.Number:
		s = t.String()
	case string:
		s = t
	default:
		return "", false
	}
	if n, ok := new(big.Int).SetString(s, 10); ok {
		return n.String(), true
	}
	return "", false
}

// FormatDecimal renders an exact integer string with the given decimals using
// string arithmetic only. FormatDecimal("12345000", 7) is "1.2345".
func FormatDecimal(exact string, decimals int) string {
	neg := strings.HasPrefix(exact, "-")
	digits := strings.TrimPrefix(exact, "-")
	if decimals <= 0 {
		return exact
	}
	if len(digits) <= decimals {
		digits = strings.Repeat("0", decimals-len(digits)+1) + digits
	}
	whole, frac := digits[:len(digits)-decimals], strings.TrimRight(digits[len(digits)-decimals:], "0")
	out := whole
	if frac != "" {
		out += "." + frac
	}
	if neg {
		out = "-" + out
	}
	return out
}

// AmountDisplay renders an integer payload field for a responder: the exact
// integer always, and when the source supplied decimals, the token-unit value
// beside it, labelled with where the metadata came from. With no decimals it
// is just the exact integer. It is "" when the field is absent or not an
// integer.
func (a Alert) AmountDisplay(field string) string {
	exact, ok := a.payloadInteger(field)
	if !ok {
		return ""
	}
	v := a.enrichView()
	if v.Decimals < 0 {
		return exact
	}
	sym := ""
	if v.Symbol != "" {
		sym = " " + a.escapeEnrich(v.Symbol)
	}
	src := v.Source
	if src == "" {
		src = "unknown source"
	}
	return fmt.Sprintf("%s%s (exact integer %s; %d decimals; symbol and decimals are unverified metadata from %s)",
		FormatDecimal(exact, v.Decimals), sym, exact, v.Decimals, a.escapeEnrich(src))
}

// PrimaryAmount is AmountDisplay for the payload's conventional amount field.
func (a Alert) PrimaryAmount() string {
	if len(a.Enrichment) == 0 {
		// Without enrichment the output stays exactly what it was before.
		return ""
	}
	for _, f := range []string{"value", "amount"} {
		if s := a.AmountDisplay(f); s != "" {
			return s
		}
	}
	return ""
}

// escapeFor returns the escaper for a channel kind. An unknown kind escapes
// nothing, which is the behaviour for plain-text destinations.
func escapeFor(kind string) func(string) string {
	switch kind {
	case "slack":
		return EscapeSlackMrkdwn
	case "html":
		return EscapeHTML
	case "markdown":
		return EscapeMarkdown
	}
	return nil
}
