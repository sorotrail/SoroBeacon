package auth

import (
	"crypto/x509"
	"fmt"
	"strings"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// MTLSBinding maps a verified client certificate identity (SPIFFE URI SAN,
// DNS SAN, email SAN, or Common Name) to a SoroBeacon workspace and role.
type MTLSBinding struct {
	Subject   string
	Workspace workspace.ID
	Role      Role
}

// ParseMTLSPrincipalMap parses a comma-separated list of
// `<subject>=<workspace>[:<role>]` entries from MTLS_PRINCIPAL_MAP.
// When `:<role>` is omitted, RoleAdmin is used.
func ParseMTLSPrincipalMap(raw string) ([]MTLSBinding, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]MTLSBinding, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))

	for _, part := range parts {
		entry := strings.TrimSpace(part)
		if entry == "" {
			return nil, fmt.Errorf("empty entry in principal map")
		}
		eq := strings.IndexByte(entry, '=')
		if eq <= 0 || eq == len(entry)-1 {
			return nil, fmt.Errorf("entry %q must have the form <subject>=<workspace>[:<role>]", entry)
		}
		subj := strings.TrimSpace(entry[:eq])
		target := strings.TrimSpace(entry[eq+1:])
		if subj == "" || target == "" {
			return nil, fmt.Errorf("entry %q must have a non-empty subject and workspace", entry)
		}

		wsPart := target
		role := RoleAdmin
		if colon := strings.LastIndexByte(target, ':'); colon != -1 {
			wsPart = strings.TrimSpace(target[:colon])
			rolePart := strings.TrimSpace(target[colon+1:])
			r, ok := ParseRole(rolePart)
			if !ok {
				return nil, fmt.Errorf("entry %q has invalid role %q (must be viewer, editor, or admin)", entry, rolePart)
			}
			role = r
		}
		if !workspace.Valid(wsPart) {
			return nil, fmt.Errorf("entry %q has invalid workspace %q", entry, wsPart)
		}
		key := normalizeMTLSSubject(subj)
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("duplicate subject %q in principal map", subj)
		}
		seen[key] = struct{}{}
		out = append(out, MTLSBinding{
			Subject:   subj,
			Workspace: workspace.ID(wsPart),
			Role:      role,
		})
	}
	return out, nil
}

func normalizeMTLSSubject(s string) string {
	s = strings.TrimSpace(s)
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "cn:") || strings.HasPrefix(lower, "dns:") || strings.HasPrefix(lower, "email:") {
		return lower
	}
	return s
}

// ExtractClientCertIdentities returns the candidate identity strings carried
// by a verified X.509 client certificate:
//   - URI SANs (e.g. spiffe://cluster.local/ns/prod/sa/ingestor)
//   - DNS SANs (both raw and prefixed with "dns:")
//   - Email SANs (both raw and prefixed with "email:")
//   - Subject CommonName (both prefixed with "cn:" and raw)
func ExtractClientCertIdentities(cert *x509.Certificate) []string {
	if cert == nil {
		return nil
	}
	var ids []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		for _, existing := range ids {
			if existing == v {
				return
			}
		}
		ids = append(ids, v)
	}

	for _, u := range cert.URIs {
		if u != nil {
			add(u.String())
		}
	}
	for _, dns := range cert.DNSNames {
		d := strings.ToLower(strings.TrimSpace(dns))
		if d != "" {
			add("dns:" + d)
			add(d)
		}
	}
	for _, email := range cert.EmailAddresses {
		e := strings.ToLower(strings.TrimSpace(email))
		if e != "" {
			add("email:" + e)
			add(e)
		}
	}
	if cn := strings.TrimSpace(cert.Subject.CommonName); cn != "" {
		add("cn:" + strings.ToLower(cn))
		add("cn:" + cn)
		add(cn)
	}
	return ids
}

// WithMTLS configures client-certificate authentication on the Authenticator.
// When enabled is true (or bindings is non-empty), verified TLS client
// certificates are accepted as credentials on HTTP and gRPC requests.
func (a *Authenticator) WithMTLS(enabled bool, bindings []MTLSBinding) *Authenticator {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mtlsEnabled = enabled || len(bindings) > 0
	a.mtlsBindings = append([]MTLSBinding(nil), bindings...)
	return a
}

// MTLSEnabled reports whether mTLS client-certificate authentication is active.
func (a *Authenticator) MTLSEnabled() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mtlsEnabled || len(a.mtlsBindings) > 0
}

// ResolveClientCertificate maps a verified X.509 client certificate to a
// Principal and AuthStatus. When MTLSBindings are configured, the certificate
// must match at least one binding or AuthForbidden is returned. When mTLS
// client authentication is enabled without explicit bindings, verified client
// certificates authenticate as RoleAdmin in the default workspace.
func (a *Authenticator) ResolveClientCertificate(cert *x509.Certificate) (*Principal, AuthStatus) {
	if a == nil || cert == nil {
		return nil, AuthUnauthenticated
	}
	a.mu.Lock()
	enabled := a.mtlsEnabled
	bindings := append([]MTLSBinding(nil), a.mtlsBindings...)
	a.mu.Unlock()

	if !enabled && len(bindings) == 0 {
		return nil, AuthUnauthenticated
	}
	if len(bindings) == 0 {
		name := "mtls"
		if ids := ExtractClientCertIdentities(cert); len(ids) > 0 {
			name = ids[0]
		}
		return &Principal{
			Workspace: workspace.Default,
			Role:      RoleAdmin,
			Name:      name,
			ActorType: "automation",
		}, AuthOK
	}

	candidates := ExtractClientCertIdentities(cert)
	var grants []WorkspaceGrant
	var matchedSubject string
	for _, b := range bindings {
		normBind := normalizeMTLSSubject(b.Subject)
		for _, cand := range candidates {
			if cand == b.Subject || normalizeMTLSSubject(cand) == normBind {
				if matchedSubject == "" {
					matchedSubject = b.Subject
				}
				grants = append(grants, WorkspaceGrant{
					Workspace: b.Workspace,
					Role:      b.Role,
				})
				break
			}
		}
	}
	if len(grants) == 0 {
		return nil, AuthForbidden
	}
	return &Principal{
		Workspace: grants[0].Workspace,
		Role:      grants[0].Role,
		Grants:    grants,
		Name:      matchedSubject,
		ActorType: "automation",
	}, AuthOK
}
