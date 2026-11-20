package apiclient

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/apierr"
)

func TestIsNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "not found code",
			err:  &APIError{Status: http.StatusNotFound, Code: apierr.CodeNotFound},
			want: true,
		},
		{
			name: "not found status without a code",
			err:  &APIError{Status: http.StatusNotFound},
			want: true,
		},
		{
			name: "not found code wins over a non-404 status",
			err:  &APIError{Status: http.StatusInternalServerError, Code: apierr.CodeNotFound},
			want: true,
		},
		{
			name: "wrapped not found",
			err:  fmt.Errorf("get monitor 42: %w", &APIError{Status: http.StatusNotFound, Code: apierr.CodeNotFound}),
			want: true,
		},
		{
			name: "empty API error",
			err:  &APIError{},
			want: false,
		},
		{
			name: "bad request status is not a not-found",
			err:  &APIError{Status: http.StatusBadRequest, Code: apierr.CodeInvalidArgument},
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
			if got := IsNotFound(tc.err); got != tc.want {
				t.Fatalf("IsNotFound(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
