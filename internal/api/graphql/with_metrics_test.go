package graphql

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/metrics"
)

func TestWithMetrics(t *testing.T) {
	m := metrics.New()

	t.Run("attaches metrics and returns the receiver", func(t *testing.T) {
		r := NewResolver(nil, discardLogger())
		got := r.WithMetrics(m)
		require.Same(t, r, got, "WithMetrics must return the receiver for chaining")
		assert.Same(t, m, got.metrics)
	})

	t.Run("nil metrics detaches metrics", func(t *testing.T) {
		r := NewResolver(nil, discardLogger()).WithMetrics(m)
		require.Same(t, m, r.metrics)

		assert.Same(t, r, r.WithMetrics(nil))
		assert.Nil(t, r.metrics)
	})

	t.Run("a nil resolver is left nil", func(t *testing.T) {
		var r *Resolver
		assert.NotPanics(t, func() {
			assert.Nil(t, r.WithMetrics(m))
		})
	})
}
