package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGraphQLConfigPlaygroundEnabled(t *testing.T) {
	tests := []struct {
		name string
		cfg  GraphQLConfig
		want bool
	}{
		{
			name: "enabled",
			cfg:  GraphQLConfig{EnablePlayground: true},
			want: true,
		},
		{
			name: "disabled",
			cfg:  GraphQLConfig{EnablePlayground: false},
			want: false,
		},
		{
			name: "zero value is disabled",
			cfg:  GraphQLConfig{},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.cfg.PlaygroundEnabled())
		})
	}
}
