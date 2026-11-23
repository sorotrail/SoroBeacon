package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/tlsutil"
)

// ServerTLSEnabled reports whether server-side TLS is configured for the HTTP
// API/dashboard and gRPC server.
func (c Config) ServerTLSEnabled() bool {
	return strings.TrimSpace(c.TLSCertFile) != "" && strings.TrimSpace(c.TLSKeyFile) != ""
}

// ServerMTLSEnabled reports whether client-certificate authentication is
// enabled on the HTTP API and gRPC server.
func (c Config) ServerMTLSEnabled() bool {
	mode := tlsutil.ClientAuthMode(strings.ToLower(strings.TrimSpace(c.TLSClientAuth)))
	return (mode == tlsutil.ClientAuthRequest || mode == tlsutil.ClientAuthRequire) && strings.TrimSpace(c.TLSClientCAFile) != ""
}

// SoroTrailTLSEnabled reports whether custom TLS or client-certificate mTLS is
// configured for the upstream SoroTrail client.
func (c Config) SoroTrailTLSEnabled() bool {
	return strings.TrimSpace(c.SoroTrailTLSCertFile) != "" ||
		strings.TrimSpace(c.SoroTrailTLSKeyFile) != "" ||
		strings.TrimSpace(c.SoroTrailTLSCAFile) != "" ||
		strings.TrimSpace(c.SoroTrailTLSServerName) != ""
}

// DatabaseTLSEnabled reports whether database TLS or mTLS parameters are
// configured via environment variables.
func (c Config) DatabaseTLSEnabled() bool {
	return strings.TrimSpace(c.DatabaseSSLMode) != "" ||
		strings.TrimSpace(c.DatabaseSSLCertFile) != "" ||
		strings.TrimSpace(c.DatabaseSSLKeyFile) != "" ||
		strings.TrimSpace(c.DatabaseSSLRootCertFile) != ""
}

func loadMTLS(cfg *Config, fileValues map[string]string, sqliteBackend bool, ps *problemList) {
	cfg.TLSCertFile = strings.TrimSpace(lookupConfigValue("TLS_CERT_FILE", fileValues))
	cfg.TLSKeyFile = strings.TrimSpace(lookupConfigValue("TLS_KEY_FILE", fileValues))
	cfg.TLSClientCAFile = strings.TrimSpace(lookupConfigValue("TLS_CLIENT_CA_FILE", fileValues))
	rawClientAuth := strings.TrimSpace(lookupConfigValue("TLS_CLIENT_AUTH", fileValues))

	mode, err := tlsutil.ParseClientAuthMode(rawClientAuth, cfg.TLSClientCAFile != "")
	if err != nil {
		ps.add("TLS_CLIENT_AUTH", fmt.Errorf("invalid TLS_CLIENT_AUTH %q: %w", rawClientAuth, err))
		mode = tlsutil.ClientAuthNone
	}
	cfg.TLSClientAuth = string(mode)

	if (cfg.TLSCertFile == "") != (cfg.TLSKeyFile == "") {
		if cfg.TLSCertFile == "" {
			ps.add("TLS_CERT_FILE", fmt.Errorf("TLS_CERT_FILE is required when TLS_KEY_FILE is set"))
		} else {
			ps.add("TLS_KEY_FILE", fmt.Errorf("TLS_KEY_FILE is required when TLS_CERT_FILE is set"))
		}
	}
	if cfg.TLSClientCAFile != "" && cfg.TLSCertFile == "" && cfg.TLSKeyFile == "" {
		ps.add("TLS_CLIENT_CA_FILE", fmt.Errorf("TLS_CLIENT_CA_FILE requires TLS_CERT_FILE and TLS_KEY_FILE"))
	}
	if mode != tlsutil.ClientAuthNone {
		if cfg.TLSCertFile == "" && cfg.TLSKeyFile == "" {
			ps.add("TLS_CLIENT_AUTH", fmt.Errorf("TLS_CLIENT_AUTH=%s requires TLS_CERT_FILE and TLS_KEY_FILE", mode))
		}
		if cfg.TLSClientCAFile == "" {
			ps.add("TLS_CLIENT_AUTH", fmt.Errorf("TLS_CLIENT_AUTH=%s requires TLS_CLIENT_CA_FILE", mode))
		}
	}

	if cfg.TLSCertFile != "" && cfg.TLSKeyFile != "" && !ps.has("TLS_CLIENT_AUTH") {
		if _, vErr := tlsutil.ValidateFiles(tlsutil.PeerConfig{
			Component:  "server",
			CertFile:   cfg.TLSCertFile,
			KeyFile:    cfg.TLSKeyFile,
			CAFile:     cfg.TLSClientCAFile,
			ClientAuth: mode,
		}, time.Now()); vErr != nil {
			field := "TLS_CERT_FILE"
			if strings.Contains(vErr.Error(), "CA ") {
				field = "TLS_CLIENT_CA_FILE"
			}
			ps.add(field, vErr)
		}
	}

	rawPrincipalMap := strings.TrimSpace(lookupConfigValue("MTLS_PRINCIPAL_MAP", fileValues))
	if rawPrincipalMap != "" {
		bindings, pErr := auth.ParseMTLSPrincipalMap(rawPrincipalMap)
		if pErr != nil {
			ps.add("MTLS_PRINCIPAL_MAP", fmt.Errorf("invalid MTLS_PRINCIPAL_MAP: %w", pErr))
		} else {
			cfg.MTLSBindings = bindings
			if cfg.TLSClientCAFile == "" {
				ps.add("MTLS_PRINCIPAL_MAP", fmt.Errorf("MTLS_PRINCIPAL_MAP requires TLS_CLIENT_CA_FILE"))
			}
		}
	}

	// Upstream SoroTrail client TLS / mTLS.
	cfg.SoroTrailTLSCertFile = strings.TrimSpace(lookupConfigValue("SOROTRAIL_TLS_CERT_FILE", fileValues))
	cfg.SoroTrailTLSKeyFile = strings.TrimSpace(lookupConfigValue("SOROTRAIL_TLS_KEY_FILE", fileValues))
	cfg.SoroTrailTLSCAFile = strings.TrimSpace(lookupConfigValue("SOROTRAIL_TLS_CA_FILE", fileValues))
	cfg.SoroTrailTLSServerName = strings.TrimSpace(lookupConfigValue("SOROTRAIL_TLS_SERVER_NAME", fileValues))

	if (cfg.SoroTrailTLSCertFile == "") != (cfg.SoroTrailTLSKeyFile == "") {
		if cfg.SoroTrailTLSCertFile == "" {
			ps.add("SOROTRAIL_TLS_CERT_FILE", fmt.Errorf("SOROTRAIL_TLS_CERT_FILE is required when SOROTRAIL_TLS_KEY_FILE is set"))
		} else {
			ps.add("SOROTRAIL_TLS_KEY_FILE", fmt.Errorf("SOROTRAIL_TLS_KEY_FILE is required when SOROTRAIL_TLS_CERT_FILE is set"))
		}
	} else if cfg.SoroTrailTLSCertFile != "" || cfg.SoroTrailTLSCAFile != "" {
		if _, vErr := tlsutil.ValidateFiles(tlsutil.PeerConfig{
			Component:  "sorotrail_client",
			CertFile:   cfg.SoroTrailTLSCertFile,
			KeyFile:    cfg.SoroTrailTLSKeyFile,
			CAFile:     cfg.SoroTrailTLSCAFile,
			ServerName: cfg.SoroTrailTLSServerName,
		}, time.Now()); vErr != nil {
			field := "SOROTRAIL_TLS_CERT_FILE"
			if cfg.SoroTrailTLSCertFile == "" {
				field = "SOROTRAIL_TLS_CA_FILE"
			}
			ps.add(field, vErr)
		}
	}

	// Database client TLS / mTLS (PostgreSQL).
	cfg.DatabaseSSLMode = strings.ToLower(strings.TrimSpace(lookupConfigValue("DATABASE_SSL_MODE", fileValues)))
	cfg.DatabaseSSLCertFile = strings.TrimSpace(lookupConfigValue("DATABASE_SSL_CERT_FILE", fileValues))
	cfg.DatabaseSSLKeyFile = strings.TrimSpace(lookupConfigValue("DATABASE_SSL_KEY_FILE", fileValues))
	cfg.DatabaseSSLRootCertFile = strings.TrimSpace(lookupConfigValue("DATABASE_SSL_ROOT_CERT_FILE", fileValues))

	if sqliteBackend && cfg.DatabaseTLSEnabled() {
		ps.add("DATABASE_SSL_MODE", fmt.Errorf("DATABASE_SSL_* settings have no effect with a sqlite DATABASE_URL; unset them or use Postgres"))
		return
	}

	if cfg.DatabaseSSLMode != "" {
		switch cfg.DatabaseSSLMode {
		case "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
		default:
			ps.add("DATABASE_SSL_MODE", fmt.Errorf("invalid DATABASE_SSL_MODE %q (want disable|allow|prefer|require|verify-ca|verify-full)", cfg.DatabaseSSLMode))
		}
	}

	if (cfg.DatabaseSSLCertFile == "") != (cfg.DatabaseSSLKeyFile == "") {
		if cfg.DatabaseSSLCertFile == "" {
			ps.add("DATABASE_SSL_CERT_FILE", fmt.Errorf("DATABASE_SSL_CERT_FILE is required when DATABASE_SSL_KEY_FILE is set"))
		} else {
			ps.add("DATABASE_SSL_KEY_FILE", fmt.Errorf("DATABASE_SSL_KEY_FILE is required when DATABASE_SSL_CERT_FILE is set"))
		}
	} else if cfg.DatabaseSSLCertFile != "" || cfg.DatabaseSSLRootCertFile != "" {
		if _, vErr := tlsutil.ValidateFiles(tlsutil.PeerConfig{
			Component: "database_client",
			CertFile:  cfg.DatabaseSSLCertFile,
			KeyFile:   cfg.DatabaseSSLKeyFile,
			CAFile:    cfg.DatabaseSSLRootCertFile,
		}, time.Now()); vErr != nil {
			field := "DATABASE_SSL_CERT_FILE"
			if cfg.DatabaseSSLCertFile == "" {
				field = "DATABASE_SSL_ROOT_CERT_FILE"
			}
			ps.add(field, vErr)
		}
	}
}
