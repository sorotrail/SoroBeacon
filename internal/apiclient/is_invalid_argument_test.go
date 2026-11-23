package apiclient

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/apierr"
)

func TestIsInvalidArgument(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "invalid argument code",
			err:  &APIError{Status: http.StatusUnprocessableEntity, Code: apierr.CodeInvalidArgument},
			want: true,
		},
		{
			name: "bad request status without a code",
			err:  &APIError{Status: http.StatusBadRequest},
			want: true,
		},
		{
			name: "wrapped invalid argument",
			err:  fmt.Errorf("create monitor: %w", &APIError{Status: http.StatusBadRequest, Code: apierr.CodeInvalidArgument}),
			want: true,
		},
		{
			name: "already exists on a 400 is not an invalid argument",
			err:  &APIError{Status: http.StatusBadRequest, Code: apierr.CodeAlreadyExists},
			want: false,
		},
		{
			name: "legacy duplicate code on a 400 is not an invalid argument",
			err:  &APIError{Status: http.StatusBadRequest, Code: "DUPLICATE"},
			want: false,
		},
		{
			name: "other API error",
			err:  &APIError{Status: http.StatusNotFound, Code: "NOT_FOUND"},
			want: false,
		},
		{
			name: "non-API error",
			err:  errors.New("connection refused"),
			want: false,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			// Empty input: the zero-value APIError carries no code and no
			// status, so it identifies nothing.
			name: "empty API error",
			err:  &APIError{},
			want: false,
		},
		{
			// Invalid input: a blank code on a success status is not a
			// validation failure, however malformed the error looks.
			name: "blank code on a non-error status",
			err:  &APIError{Status: http.StatusOK},
			want: false,
		},
		{
			// Boundary: the stable code decides before the status does, so an
			// invalid-argument code still matches above the 4xx range.
			name: "invalid argument code wins over a 5xx status",
			err:  &APIError{Status: http.StatusInternalServerError, Code: apierr.CodeInvalidArgument},
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsInvalidArgument(tc.err); got != tc.want {
				t.Fatalf("IsInvalidArgument(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
