package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildResolverHappyPath(t *testing.T) {
	t.Helper()
	cfg := Config{SecretsProvider: "env"}
	resolver := BuildResolver(cfg)
	require.NotNil(t, resolver)
}

func TestBuildResolverEdgeCases(t *testing.T) {
	t.Helper()

	tests := []struct {
		name         string
		provider     string
		wantResolver bool
	}{
		{name: "empty provider"},
		{name: "unsupported provider", provider: "unknown"},
		{name: "valid provider with blank tokens", provider: " , env, ", wantResolver: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := BuildResolver(Config{SecretsProvider: tt.provider})
			if tt.wantResolver {
				require.NotNil(t, resolver)
			} else {
				require.Nil(t, resolver)
			}
		})
	}
}
