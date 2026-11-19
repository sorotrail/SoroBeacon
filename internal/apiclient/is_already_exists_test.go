package apiclient

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/apierr"
)

func TestIsAlreadyExists(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "already exists code",
			err:  &APIError{Status: http.StatusBadRequest, Code: apierr.CodeAlreadyExists},
			want: true,
		},
		{
			name: "legacy duplicate code",
			err:  &APIError{Status: http.StatusBadRequest, Code: "DUPLICATE"},
			want: true,
		},
		{
			name: "conflict status without a code",
			err:  &APIError{Status: http.StatusConflict},
			want: true,
		},
		{
			name: "wrapped already exists",
			err:  fmt.Errorf("create channel: %w", &APIError{Status: http.StatusConflict, Code: apierr.CodeAlreadyExists}),
			want: true,
		},
		{
			name: "invalid argument",
			err:  &APIError{Status: http.StatusBadRequest, Code: apierr.CodeInvalidArgument},
			want: false,
		},
		{
			name: "not found",
			err:  &APIError{Status: http.StatusNotFound, Code: "NOT_FOUND"},
			want: false,
		},
		{
			name: "non-API error",
			err:  errors.New("connection reset"),
			want: false,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsAlreadyExists(tc.err); got != tc.want {
				t.Fatalf("IsAlreadyExists(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
