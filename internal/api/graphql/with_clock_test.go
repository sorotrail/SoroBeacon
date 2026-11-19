package graphql

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithClock(t *testing.T) {
	fixed := time.Date(2026, 11, 19, 12, 34, 56, 0, time.FixedZone("UTC+2", 2*60*60))

	t.Run("overrides now and normalises to UTC", func(t *testing.T) {
		r := NewResolver(nil, discardLogger())
		got := r.WithClock(func() time.Time { return fixed })
		require.Same(t, r, got, "WithClock must return the receiver for chaining")
		assert.True(t, fixed.Equal(r.now()), "now must return the injected instant")
		assert.Equal(t, time.UTC, r.now().Location(), "now must be normalised to UTC")
	})

	t.Run("a nil clock falls back to the wall clock", func(t *testing.T) {
		r := NewResolver(nil, discardLogger()).WithClock(func() time.Time { return fixed })
		require.True(t, fixed.Equal(r.now()))

		r.WithClock(nil)
		assert.WithinDuration(t, time.Now().UTC(), r.now(), time.Second)
	})

	t.Run("a nil resolver is left nil", func(t *testing.T) {
		var r *Resolver
		assert.NotPanics(t, func() {
			assert.Nil(t, r.WithClock(time.Now))
		})
	})
}
