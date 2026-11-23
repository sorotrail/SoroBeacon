package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// FileProvider reads secrets from local filesystem paths or projected
// container volumes (e.g. Kubernetes Secret projected volumes at
// /var/run/secrets or /etc/secrets):
//
//	${secret:file:/var/run/secrets/db_url}
//	${secret:file:/etc/secrets/credentials.json#token}
//	${secret:file:/etc/secrets/config.env#DATABASE_URL}
//
// When no key is given, the entire file content is returned, trimmed of
// whitespace and trailing newlines.
//
// When a key is given, the file is parsed:
// 1. As a JSON object mapping string keys to values.
// 2. Or as line-delimited KEY=VALUE or KEY: VALUE pairs.
//
// Errors name the path and key but never the secret content.
type FileProvider struct {
	// ReadFile defaults to os.ReadFile; tests can substitute their own.
	ReadFile func(string) ([]byte, error)
	// MaxBytes limits file size to prevent unbounded memory consumption
	// (default 1MB).
	MaxBytes int64
}

// NewFileProvider returns a new FileProvider.
func NewFileProvider() *FileProvider {
	return &FileProvider{
		ReadFile: os.ReadFile,
		MaxBytes: 1 << 20, // 1MB
	}
}

// Scheme implements Provider.
func (p *FileProvider) Scheme() string { return "file" }

// Fetch implements Provider.
func (p *FileProvider) Fetch(_ context.Context, path, key string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("file: path is empty")
	}
	read := p.ReadFile
	if read == nil {
		read = os.ReadFile
	}
	data, err := read(path)
	if err != nil {
		return "", fmt.Errorf("file: read %s: %w", path, err)
	}
	maxBytes := p.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	if int64(len(data)) > maxBytes {
		return "", fmt.Errorf("file: %s exceeds maximum allowed size of %d bytes", path, maxBytes)
	}

	if key == "" {
		trimmed := strings.TrimSpace(string(data))
		if trimmed == "" {
			return "", fmt.Errorf("file: %s is empty", path)
		}
		return trimmed, nil
	}

	// Structured JSON lookup
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err == nil {
		val, err := vaultValue(fields, key)
		if err == nil {
			return val, nil
		}
	}

	// Line-based KEY=VALUE or KEY: VALUE lookup
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			k, v, ok = strings.Cut(line, ":")
		}
		if ok && strings.TrimSpace(k) == key {
			cleanVal := strings.Trim(strings.TrimSpace(v), `"'`)
			if cleanVal != "" {
				return cleanVal, nil
			}
		}
	}

	return "", fmt.Errorf("file: %s: key %q not found", path, key)
}
