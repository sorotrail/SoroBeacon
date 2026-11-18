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

func TestBuildResolverErrorPath(t *testing.T) {
	t.Helper()
	cfg := Config{SecretsProvider: ""}
	resolver := BuildResolver(cfg)
	require.Nil(t, resolver)
}