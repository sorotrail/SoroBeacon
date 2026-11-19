package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQuotaResponseRecorderUnwrap(t *testing.T) {
	tests := []struct {
		name       string
		underlying http.ResponseWriter
	}{
		{
			name:       "returns the wrapped writer",
			underlying: httptest.NewRecorder(),
		},
		{
			name:       "a recorder without a writer unwraps to nil",
			underlying: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &quotaResponseRecorder{ResponseWriter: tc.underlying}
			got := rec.Unwrap()
			if tc.underlying == nil {
				assert.Nil(t, got)
				return
			}
			require.Same(t, tc.underlying, got)
		})
	}
}
