package web

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithMaxBodyBytes(t *testing.T) {
	const def = int64(0)
	tests := []struct {
		name string
		in   int64
		want int64
	}{
		{"positive value is applied", 4096, 4096},
		{"zero is ignored", 0, def},
		{"negative is ignored", -1, def},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			require.Equal(t, def, s.MaxBodyBytes(), "default MaxBodyBytes should be zero")

			ret := s.WithMaxBodyBytes(tc.in)
			assert.Equal(t, s, ret, "WithMaxBodyBytes must return the same *Server for chaining")
			assert.Equal(t, tc.want, s.MaxBodyBytes())
		})
	}
}

func TestWithMaxBodyBytesKeepsPreviousOnInvalid(t *testing.T) {
	s := newTestServer(t).WithMaxBodyBytes(8192).WithMaxBodyBytes(0).WithMaxBodyBytes(-10)
	assert.Equal(t, int64(8192), s.MaxBodyBytes(), "invalid values must not overwrite a previously set limit")
}
