package api

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/store"
)

func TestWithBreaker(t *testing.T) {
	tests := []struct {
		name    string
		breaker *store.StoreBreaker
	}{
		{name: "installs a replacement breaker", breaker: store.NewStoreBreaker(7, time.Minute)},
		{name: "nil clears the breaker", breaker: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := New(nil, nil, nil, nil, nil)
			require.NotNil(t, srv.breaker, "New should install a default breaker")

			got := srv.WithBreaker(tt.breaker)

			assert.Same(t, srv, got, "WithBreaker should return the server for chaining")
			assert.Equal(t, tt.breaker, srv.breaker)
		})
	}
}
