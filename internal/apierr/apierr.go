package apierr

import (
	"errors"
	"net/http"

	"google.golang.org/grpc/codes"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// Standard taxonomy of stable, machine-readable error codes.
const (
	CodeNotFound           = "NOT_FOUND"
	CodeAlreadyExists      = "ALREADY_EXISTS"
	CodeDuplicate          = CodeAlreadyExists
	CodeInvalidArgument    = "INVALID_ARGUMENT"
	CodeConflict           = "CONFLICT"
	CodeUnauthenticated    = "UNAUTHENTICATED"
	CodePermissionDenied   = "PERMISSION_DENIED"
	CodeRateLimited        = "RATE_LIMITED"
	CodeQuotaExceeded      = "QUOTA_EXCEEDED"
	CodePayloadTooLarge    = "PAYLOAD_TOO_LARGE"
	CodeMethodNotAllowed   = "METHOD_NOT_ALLOWED"
	CodeNotImplemented     = "NOT_IMPLEMENTED"
	CodeServiceUnavailable = "SERVICE_UNAVAILABLE"
	CodeInternalError      = "INTERNAL_ERROR"
)

// AllCodes returns all defined taxonomy error codes.
func AllCodes() []string {
	return []string{
		CodeNotFound,
		CodeAlreadyExists,
		CodeInvalidArgument,
		CodeConflict,
		CodeUnauthenticated,
		CodePermissionDenied,
		CodeRateLimited,
		CodeQuotaExceeded,
		CodePayloadTooLarge,
		CodeMethodNotAllowed,
		CodeNotImplemented,
		CodeServiceUnavailable,
		CodeInternalError,
	}
}

// CodeFromStatus maps an HTTP status code to its default taxonomy error code.
func CodeFromStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return CodeInvalidArgument
	case http.StatusUnauthorized:
		return CodeUnauthenticated
	case http.StatusForbidden:
		return CodePermissionDenied
	case http.StatusNotFound:
		return CodeNotFound
	case http.StatusMethodNotAllowed:
		return CodeMethodNotAllowed
	case http.StatusConflict:
		return CodeConflict
	case http.StatusRequestEntityTooLarge:
		return CodePayloadTooLarge
	case http.StatusUnprocessableEntity:
		return CodeInvalidArgument
	case http.StatusTooManyRequests:
		return CodeRateLimited
	case http.StatusNotImplemented:
		return CodeNotImplemented
	case http.StatusServiceUnavailable:
		return CodeServiceUnavailable
	default:
		return CodeInternalError
	}
}

// StatusFromCode maps a taxonomy error code to its canonical HTTP status code.
func StatusFromCode(code string) int {
	switch code {
	case CodeInvalidArgument, CodeQuotaExceeded:
		return http.StatusBadRequest
	case CodeUnauthenticated:
		return http.StatusUnauthorized
	case CodePermissionDenied:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeMethodNotAllowed:
		return http.StatusMethodNotAllowed
	case CodeConflict:
		return http.StatusConflict
	case CodeAlreadyExists:
		return http.StatusUnprocessableEntity
	case CodePayloadTooLarge:
		return http.StatusRequestEntityTooLarge
	case CodeRateLimited:
		return http.StatusTooManyRequests
	case CodeNotImplemented:
		return http.StatusNotImplemented
	case CodeServiceUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// GRPCCode maps a taxonomy error code to its standard gRPC status code.
func GRPCCode(code string) codes.Code {
	switch code {
	case CodeNotFound:
		return codes.NotFound
	case CodeAlreadyExists:
		return codes.AlreadyExists
	case CodeInvalidArgument:
		return codes.InvalidArgument
	case CodeConflict:
		return codes.FailedPrecondition
	case CodeUnauthenticated:
		return codes.Unauthenticated
	case CodePermissionDenied:
		return codes.PermissionDenied
	case CodeRateLimited, CodeQuotaExceeded:
		return codes.ResourceExhausted
	case CodePayloadTooLarge:
		return codes.InvalidArgument
	case CodeMethodNotAllowed, CodeNotImplemented:
		return codes.Unimplemented
	case CodeServiceUnavailable:
		return codes.Unavailable
	default:
		return codes.Internal
	}
}

// Classified represents a classified error across HTTP, gRPC, and REST/GraphQL surfaces.
type Classified struct {
	Status   int
	Code     string
	Message  string
	GRPCCode codes.Code
}

// Classify converts an error into a structured Classified result.
// Crucially, it never leaks internal database errors, SQL queries, or hostnames to callers.
func Classify(err error) Classified {
	if err == nil {
		return Classified{Status: http.StatusOK, GRPCCode: codes.OK}
	}
	if errors.Is(err, store.ErrNotFound) {
		return Classified{
			Status:   http.StatusNotFound,
			Code:     CodeNotFound,
			Message:  "not found",
			GRPCCode: codes.NotFound,
		}
	}
	if errors.Is(err, store.ErrConstraint) {
		kind, ok := store.ConstraintKindOf(err)
		if ok && kind == store.ConstraintUnique {
			return Classified{
				Status:   http.StatusUnprocessableEntity,
				Code:     CodeAlreadyExists,
				Message:  "the request conflicts with an existing record",
				GRPCCode: codes.AlreadyExists,
			}
		}
		if ok && kind == store.ConstraintForeignKey {
			return Classified{
				Status:   http.StatusUnprocessableEntity,
				Code:     CodeNotFound,
				Message:  "the request references a record that does not exist",
				GRPCCode: codes.NotFound,
			}
		}
		if ok && kind == store.ConstraintCheck {
			return Classified{
				Status:   http.StatusUnprocessableEntity,
				Code:     CodeInvalidArgument,
				Message:  "the request contains a value the database refuses",
				GRPCCode: codes.InvalidArgument,
			}
		}
		return Classified{
			Status:   http.StatusUnprocessableEntity,
			Code:     CodeInvalidArgument,
			Message:  "the request violates a database constraint",
			GRPCCode: codes.InvalidArgument,
		}
	}
	if errors.Is(err, store.ErrChannelInUse) {
		return Classified{
			Status:   http.StatusConflict,
			Code:     CodeConflict,
			Message:  "channel is referenced by an escalation policy",
			GRPCCode: codes.FailedPrecondition,
		}
	}
	if errors.Is(err, store.ErrIdempotencyKeyConflict) {
		return Classified{
			Status:   http.StatusConflict,
			Code:     CodeConflict,
			Message:  "idempotency key was already used with a different request payload",
			GRPCCode: codes.FailedPrecondition,
		}
	}
	if errors.Is(err, store.ErrIdempotencyKeyInProgress) {
		return Classified{
			Status:   http.StatusConflict,
			Code:     CodeConflict,
			Message:  "a request with this idempotency key is already in progress",
			GRPCCode: codes.Aborted,
		}
	}
	var quotaErr *store.QuotaExceededError
	if errors.As(err, &quotaErr) || errors.Is(err, store.ErrQuotaExceeded) {
		msg := "workspace quota exceeded"
		if quotaErr != nil {
			msg = quotaErr.Error()
		}
		return Classified{
			Status:   http.StatusTooManyRequests,
			Code:     CodeQuotaExceeded,
			Message:  msg,
			GRPCCode: codes.ResourceExhausted,
		}
	}
	if errors.Is(err, store.ErrUnavailable) {
		return Classified{
			Status:   http.StatusServiceUnavailable,
			Code:     CodeServiceUnavailable,
			Message:  "database unavailable",
			GRPCCode: codes.Unavailable,
		}
	}

	// Fallback for unclassified / internal errors: generic code, safe message.
	return Classified{
		Status:   http.StatusInternalServerError,
		Code:     CodeInternalError,
		Message:  "internal error",
		GRPCCode: codes.Internal,
	}
}
