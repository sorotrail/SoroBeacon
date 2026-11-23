package rules

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Cross-cutting rule parameters for attaching runbook links and triage
// instructions to alerts. Evaluators ignore keys they do not use, so any rule
// type may carry these in its params JSON in addition to top-level Rule fields.
const (
	RunbookURLParam       = "runbook_url"
	ResponseGuidanceParam = "response_guidance"

	MaxRunbookURLLen       = 2048
	MaxResponseGuidanceLen = 500
)

// ValidateRunbookURL trims and validates an optional runbook URL. An empty or
// whitespace-only string is valid and returns "". Non-empty URLs must be valid
// UTF-8, at most MaxRunbookURLLen characters, free of whitespace and control
// characters, use an explicit http or https scheme, and carry a non-empty host.
func ValidateRunbookURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	if !utf8.ValidString(trimmed) {
		return "", FieldError{Field: RunbookURLParam, Reason: "runbook_url: must be valid UTF-8"}
	}
	if len(trimmed) > MaxRunbookURLLen {
		return "", FieldError{
			Field:  RunbookURLParam,
			Reason: fmt.Sprintf("runbook_url: exceeds maximum length of %d characters", MaxRunbookURLLen),
		}
	}
	for _, r := range trimmed {
		if r <= 0x20 || r == 0x7f || unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", FieldError{
				Field:  RunbookURLParam,
				Reason: "runbook_url: must not contain whitespace or control characters",
			}
		}
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", FieldError{Field: RunbookURLParam, Reason: fmt.Sprintf("runbook_url: invalid URL: %v", err)}
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", FieldError{Field: RunbookURLParam, Reason: "runbook_url: must use http or https scheme"}
	}
	if u.Opaque != "" || u.Host == "" || u.Hostname() == "" {
		return "", FieldError{Field: RunbookURLParam, Reason: "runbook_url: must include a valid host"}
	}
	return scheme + trimmed[len(u.Scheme):], nil
}

// ValidateResponseGuidance normalizes line endings, trims surrounding
// whitespace, and validates an optional response guidance note. An empty or
// whitespace-only note returns "". Non-empty notes must be valid UTF-8, at most
// MaxResponseGuidanceLen runes, and free of control characters other than
// newline and tab.
func ValidateResponseGuidance(raw string) (string, error) {
	normalized := strings.ReplaceAll(raw, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	trimmed := strings.TrimSpace(normalized)
	if trimmed == "" {
		return "", nil
	}
	if !utf8.ValidString(trimmed) {
		return "", FieldError{Field: ResponseGuidanceParam, Reason: "response_guidance: must be valid UTF-8"}
	}
	if utf8.RuneCountInString(trimmed) > MaxResponseGuidanceLen {
		return "", FieldError{
			Field:  ResponseGuidanceParam,
			Reason: fmt.Sprintf("response_guidance: exceeds maximum length of %d characters", MaxResponseGuidanceLen),
		}
	}
	for _, r := range trimmed {
		if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f || (unicode.IsControl(r) && r != '\n' && r != '\t') {
			return "", FieldError{
				Field:  ResponseGuidanceParam,
				Reason: "response_guidance: must not contain control characters",
			}
		}
	}
	return trimmed, nil
}

// ParseRunbookURL extracts and validates an optional "runbook_url" from a
// rule's raw JSON params.
func ParseRunbookURL(params json.RawMessage) (string, error) {
	if len(params) == 0 {
		return "", nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(params, &raw); err != nil {
		return "", nil
	}
	value, ok := raw[RunbookURLParam]
	if !ok || len(value) == 0 || string(value) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(value, &s); err != nil {
		return "", FieldError{
			Field:  RunbookURLParam,
			Reason: "runbook_url: must be a string URL using http or https",
		}
	}
	return ValidateRunbookURL(s)
}

// ParseResponseGuidance extracts and validates an optional "response_guidance"
// from a rule's raw JSON params.
func ParseResponseGuidance(params json.RawMessage) (string, error) {
	if len(params) == 0 {
		return "", nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(params, &raw); err != nil {
		return "", nil
	}
	value, ok := raw[ResponseGuidanceParam]
	if !ok || len(value) == 0 || string(value) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(value, &s); err != nil {
		return "", FieldError{
			Field:  ResponseGuidanceParam,
			Reason: "response_guidance: must be a string",
		}
	}
	return ValidateResponseGuidance(s)
}

// ParseGuidance extracts and validates both optional runbook_url and
// response_guidance fields from a rule's raw JSON params.
func ParseGuidance(params json.RawMessage) (runbookURL, responseGuidance string, err error) {
	var problems FieldErrors
	u, uErr := ParseRunbookURL(params)
	if uErr != nil {
		problems = append(problems, asFieldErrors(uErr)...)
	}
	g, gErr := ParseResponseGuidance(params)
	if gErr != nil {
		problems = append(problems, asFieldErrors(gErr)...)
	}
	if len(problems) > 0 {
		return "", "", problems
	}
	return u, g, nil
}

func asFieldErrors(err error) FieldErrors {
	var fe FieldError
	if errors.As(err, &fe) {
		return FieldErrors{fe}
	}
	var fes FieldErrors
	if errors.As(err, &fes) {
		return fes
	}
	return FieldErrors{{Reason: err.Error()}}
}
