package secrets

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileProviderRawFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "webhook.txt")
	require.NoError(t, os.WriteFile(path, []byte("https://example.com/webhook\n"), 0o600))

	p := NewFileProvider()
	val, err := p.Fetch(context.Background(), path, "")
	require.NoError(t, err)
	assert.Equal(t, "https://example.com/webhook", val)
}

func TestFileProviderJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"token":"secret-token-123","user":"ops"}`), 0o600))

	p := NewFileProvider()
	val, err := p.Fetch(context.Background(), path, "token")
	require.NoError(t, err)
	assert.Equal(t, "secret-token-123", val)

	_, err = p.Fetch(context.Background(), path, "missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "key \"missing\" not found")
}

func TestFileProviderKeyValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env.txt")
	content := "# comment line\nDATABASE_URL=postgres://user:pass@db:5432/app\nAPI_KEY: key-456\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	p := NewFileProvider()
	val1, err := p.Fetch(context.Background(), path, "DATABASE_URL")
	require.NoError(t, err)
	assert.Equal(t, "postgres://user:pass@db:5432/app", val1)

	val2, err := p.Fetch(context.Background(), path, "API_KEY")
	require.NoError(t, err)
	assert.Equal(t, "key-456", val2)
}

func TestFileProviderMissingFile(t *testing.T) {
	p := NewFileProvider()
	_, err := p.Fetch(context.Background(), "/nonexistent/secret/path", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "file: read /nonexistent/secret/path")
}

func TestFileProviderSizeLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.txt")
	require.NoError(t, os.WriteFile(path, make([]byte, 2048), 0o600))

	p := NewFileProvider()
	p.MaxBytes = 1024

	_, err := p.Fetch(context.Background(), path, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds maximum allowed size")
}

func TestFileProviderNoLeakAssertion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sensitive.json")
	secretVal := "super-sensitive-never-leak-me"
	require.NoError(t, os.WriteFile(path, []byte(`{"secret":"`+secretVal+`"}`), 0o600))

	p := NewFileProvider()
	_, err := p.Fetch(context.Background(), path, "nonexistent-key")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secretVal)
}
