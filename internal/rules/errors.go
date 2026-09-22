package rules

import (
	"strings"
)

// FieldError names one invalid params field. Validate implementations
// return these (usually as FieldErrors) so the API can put a path and
// reason on the error envelope instead of a single flattened message.
type FieldError struct {
	Field  string
	Reason string
}

func (e FieldError) Error() string {
	if e.Field == "" {
		return e.Reason
	}
	return e.Field + ": " + e.Reason
}

// FieldErrors is every params problem found in one Validate call.
type FieldErrors []FieldError

func (e FieldErrors) Error() string {
	parts := make([]string, 0, len(e))
	for _, d := range e {
		parts = append(parts, d.Error())
	}
	return strings.Join(parts, "; ")
}
