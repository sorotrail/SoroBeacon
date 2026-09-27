package rules

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFieldErrorError(t *testing.T) {
	tests := []struct {
		name   string
		err    FieldError
		want   string
	}{
		{
			name: "with field and reason",
			err:  FieldError{Field: "count", Reason: "must be positive"},
			want: "count: must be positive",
		},
		{
			name: "empty field returns only reason",
			err:  FieldError{Field: "", Reason: "general error"},
			want: "general error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.err.Error())
		})
	}
}

func TestFieldErrorsError(t *testing.T) {
	tests := []struct {
		name   string
		errs   FieldErrors
		want   string
	}{
		{
			name: "empty slice",
			errs: FieldErrors{},
			want: "",
		},
		{
			name: "single entry",
			errs: FieldErrors{{Field: "window", Reason: "is required"}},
			want: "window: is required",
		},
		{
			name: "multiple entries",
			errs: FieldErrors{
				{Field: "count", Reason: "must be positive"},
				{Field: "window", Reason: "is required"},
			},
			want: "count: must be positive; window: is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.errs.Error())
		})
	}
}

func TestFieldErrorsAsRecovery(t *testing.T) {
	orig := FieldErrors{
		{Field: "threshold", Reason: "invalid number"},
	}

	var err error = orig
	var target FieldErrors

	require.True(t, errors.As(err, &target))
	assert.Equal(t, orig, target)
}
