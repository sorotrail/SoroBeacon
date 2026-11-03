package store

import (
	"fmt"
	"net/url"
	"strings"
)

// ApplyDatabaseTLS returns a Postgres connection URL with sslmode, sslcert,
// sslkey, and sslrootcert query parameters applied when non-empty. SQLite URLs
// and empty URLs are returned unchanged.
func ApplyDatabaseTLS(rawURL, sslMode, certFile, keyFile, rootCertFile string) (string, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" || strings.HasPrefix(strings.ToLower(trimmed), "sqlite://") {
		return rawURL, nil
	}
	sslMode = strings.TrimSpace(sslMode)
	certFile = strings.TrimSpace(certFile)
	keyFile = strings.TrimSpace(keyFile)
	rootCertFile = strings.TrimSpace(rootCertFile)
	if sslMode == "" && certFile == "" && keyFile == "" && rootCertFile == "" {
		return rawURL, nil
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("parse database URL: %w", err)
	}
	q := u.Query()
	switch {
	case sslMode != "":
		q.Set("sslmode", sslMode)
	case rootCertFile != "":
		if cur := q.Get("sslmode"); cur == "" || cur == "disable" {
			q.Set("sslmode", "verify-full")
		}
	case certFile != "":
		if cur := q.Get("sslmode"); cur == "" || cur == "disable" {
			q.Set("sslmode", "require")
		}
	}
	if certFile != "" {
		q.Set("sslcert", certFile)
	}
	if keyFile != "" {
		q.Set("sslkey", keyFile)
	}
	if rootCertFile != "" {
		q.Set("sslrootcert", rootCertFile)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}
