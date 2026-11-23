package grpc

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestWithStreamInterval(t *testing.T) {
	const defaultInterval = 2 * time.Second
	tests := []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{name: "positive interval overrides default", interval: 30 * time.Second, want: 30 * time.Second},
		{name: "zero interval is ignored", interval: 0, want: defaultInterval},
		{name: "negative interval is ignored", interval: -time.Second, want: defaultInterval},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := New(nil, nil, nil)
			assert.Equal(t, defaultInterval, srv.streamInterval)

			got := srv.WithStreamInterval(tt.interval)

			assert.Same(t, srv, got, "WithStreamInterval should return the server for chaining")
			assert.Equal(t, tt.want, srv.streamInterval)
		})
	}
}
