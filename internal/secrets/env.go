package secrets

import (
	"context"
	"fmt"
	"os"
)

// EnvProvider resolves references from the process environment:
//
//	${secret:env:SLACK_WEBHOOK_URL}
//
// The path names the environment variable. When a key is given it names the
// variable instead, so both ${secret:env:SLACK_WEBHOOK_URL} and
// ${secret:env:slack#SLACK_WEBHOOK_URL} resolve the same variable — the
// latter form is convenient when a config is generated from a template that
// always emits a key.
type EnvProvider struct {
	// Getenv defaults to os.Getenv; tests can substitute a map.
	Getenv func(string) string
}

// NewEnvProvider returns a provider backed by the process environment.
func NewEnvProvider() *EnvProvider {
	return &EnvProvider{Getenv: os.Getenv}
}

// Scheme implements Provider.
func (p *EnvProvider) Scheme() string { return "env" }

// Fetch implements Provider. The error names the variable but never its
// value.
func (p *EnvProvider) Fetch(_ context.Context, path, key string) (string, error) {
	name := path
	if key != "" {
		name = key
	}
	get := p.Getenv
	if get == nil {
		get = os.Getenv
	}
	v := get(name)
	if v == "" {
		return "", fmt.Errorf("environment variable %s is not set", name)
	}
	return v, nil
}
