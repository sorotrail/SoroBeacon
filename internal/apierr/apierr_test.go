package apierr

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"

	"github.com/sorotrail/sorobeacon/internal/store"
)

func TestClassify_StoreSentinels(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantStatus   int
		wantCode     string
		wantMessage  string
		wantGRPCCode codes.Code
	}{
		{
			name:         "not found sentinel",
			err:          store.ErrNotFound,
			wantStatus:   http.StatusNotFound,
			wantCode:     CodeNotFound,
			wantMessage:  "not found",
			wantGRPCCode: codes.NotFound,
		},
		{
			name: "unique constraint violation",
			err: &store.ConstraintError{
				Kind:       store.ConstraintUnique,
				Constraint: "monitors_name_key",
			},
			wantStatus:   http.StatusUnprocessableEntity,
			wantCode:     CodeAlreadyExists,
			wantMessage:  "the request conflicts with an existing record",
			wantGRPCCode: codes.AlreadyExists,
		},
		{
			name: "foreign key constraint violation",
			err: &store.ConstraintError{
				Kind:       store.ConstraintForeignKey,
				Constraint: "rules_monitor_id_fkey",
			},
			wantStatus:   http.StatusUnprocessableEntity,
			wantCode:     CodeNotFound,
			wantMessage:  "the request references a record that does not exist",
			wantGRPCCode: codes.NotFound,
		},
		{
			name: "check constraint violation",
			err: &store.ConstraintError{
				Kind:       store.ConstraintCheck,
				Constraint: "channels_type_check",
			},
			wantStatus:   http.StatusUnprocessableEntity,
			wantCode:     CodeInvalidArgument,
			wantMessage:  "the request contains a value the database refuses",
			wantGRPCCode: codes.InvalidArgument,
		},
		{
			name:         "channel in use sentinel",
			err:          store.ErrChannelInUse,
			wantStatus:   http.StatusConflict,
			wantCode:     CodeConflict,
			wantMessage:  "channel is referenced by an escalation policy",
			wantGRPCCode: codes.FailedPrecondition,
		},
		{
			name:         "unmapped database error never leaks internals",
			err:          errors.New("pq: connection refused to postgres-primary.internal:5432"),
			wantStatus:   http.StatusInternalServerError,
			wantCode:     CodeInternalError,
			wantMessage:  "internal error",
			wantGRPCCode: codes.Internal,
		},
		{
			name:         "nil error returns ok",
			err:          nil,
			wantStatus:   http.StatusOK,
			wantCode:     "",
			wantMessage:  "",
			wantGRPCCode: codes.OK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.err)
			assert.Equal(t, tt.wantStatus, got.Status)
			assert.Equal(t, tt.wantCode, got.Code)
			assert.Equal(t, tt.wantMessage, got.Message)
			assert.Equal(t, tt.wantGRPCCode, got.GRPCCode)
		})
	}
}

func TestCodeFromStatusAndStatusFromCode(t *testing.T) {
	for _, code := range AllCodes() {
		status := StatusFromCode(code)
		assert.True(t, status >= 400 && status < 600, "status for %s must be 4xx or 5xx, got %d", code, status)
		grpcCode := GRPCCode(code)
		assert.NotEqual(t, codes.OK, grpcCode, "gRPC code for error %s must not be OK", code)
	}

	assert.Equal(t, CodeInvalidArgument, CodeFromStatus(http.StatusBadRequest))
	assert.Equal(t, CodeNotFound, CodeFromStatus(http.StatusNotFound))
	assert.Equal(t, CodeUnauthenticated, CodeFromStatus(http.StatusUnauthorized))
	assert.Equal(t, CodePermissionDenied, CodeFromStatus(http.StatusForbidden))
	assert.Equal(t, CodeRateLimited, CodeFromStatus(http.StatusTooManyRequests))
	assert.Equal(t, CodePayloadTooLarge, CodeFromStatus(http.StatusRequestEntityTooLarge))
	assert.Equal(t, CodeInternalError, CodeFromStatus(http.StatusInternalServerError))
}
