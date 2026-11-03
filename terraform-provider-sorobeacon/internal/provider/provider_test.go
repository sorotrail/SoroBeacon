package provider

import (
	"net/http"
	"testing"
)

func TestDecodeAPIErrorAndClassification(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		body          string
		wantCode      string
		wantNotFound  bool
		wantExists    bool
		wantInvalid   bool
	}{
		{
			name:         "not found error",
			status:       http.StatusNotFound,
			body:         `{"error":"monitor not found","code":"NOT_FOUND","request_id":"req-1"}`,
			wantCode:     CodeNotFound,
			wantNotFound: true,
			wantExists:   false,
			wantInvalid:  false,
		},
		{
			name:         "already exists conflict",
			status:       http.StatusUnprocessableEntity,
			body:         `{"error":"the request conflicts with an existing record","code":"ALREADY_EXISTS","request_id":"req-2"}`,
			wantCode:     CodeAlreadyExists,
			wantNotFound: false,
			wantExists:   true,
			wantInvalid:  false,
		},
		{
			name:         "duplicate alias code",
			status:       http.StatusConflict,
			body:         `{"error":"duplicate name","code":"DUPLICATE","request_id":"req-3"}`,
			wantCode:     "DUPLICATE",
			wantNotFound: false,
			wantExists:   true,
			wantInvalid:  false,
		},
		{
			name:         "invalid argument validation failure",
			status:       http.StatusBadRequest,
			body:         `{"error":"name is required","code":"INVALID_ARGUMENT","request_id":"req-4","details":[{"field":"name","reason":"name is required"}]}`,
			wantCode:     CodeInvalidArgument,
			wantNotFound: false,
			wantExists:   false,
			wantInvalid:  true,
		},
		{
			name:         "fallback HTML from proxy with 404",
			status:       http.StatusNotFound,
			body:         `<html>404 Not Found</html>`,
			wantCode:     "Not Found",
			wantNotFound: true,
			wantExists:   false,
			wantInvalid:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := decodeAPIError(tt.status, []byte(tt.body))
			if err == nil {
				t.Fatal("expected error")
			}
			apiErr, ok := err.(*APIError)
			if !ok {
				t.Fatalf("expected *APIError, got %T", err)
			}
			if apiErr.Code != tt.wantCode {
				t.Errorf("got code %q, want %q", apiErr.Code, tt.wantCode)
			}
			if got := IsNotFound(err); got != tt.wantNotFound {
				t.Errorf("IsNotFound = %v, want %v", got, tt.wantNotFound)
			}
			if got := IsAlreadyExists(err); got != tt.wantExists {
				t.Errorf("IsAlreadyExists = %v, want %v", got, tt.wantExists)
			}
			if got := IsInvalidArgument(err); got != tt.wantInvalid {
				t.Errorf("IsInvalidArgument = %v, want %v", got, tt.wantInvalid)
			}
		})
	}
}
