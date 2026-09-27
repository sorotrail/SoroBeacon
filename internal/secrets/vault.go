package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// VaultProvider reads secrets from HashiCorp Vault over its HTTP API:
//
//	${secret:vault:kv/data/sorobeacon#slack_token}
//
// The path is appended to /v1/ on the configured address. Both KV v1
// ({"data":{...}}) and KV v2 ({"data":{"data":{...}}}) responses are
// understood, because which one an operator uses is a property of the mount,
// not of SoroBeacon.
type VaultProvider struct {
	// Addr is the Vault server base URL, e.g. https://vault.example:8200.
	Addr string
	// Token is the Vault token used in the X-Vault-Token header.
	Token string
	// Namespace is sent as X-Vault-Namespace when non-empty (Vault
	// Enterprise). Optional.
	Namespace string
	// Client defaults to a client with a short timeout; tests substitute
	// their own.
	Client *http.Client
}

// NewVaultProvider returns a provider for the given address and token.
func NewVaultProvider(addr, token, namespace string) *VaultProvider {
	return &VaultProvider{
		Addr:      strings.TrimRight(addr, "/"),
		Token:     token,
		Namespace: namespace,
		Client: &http.Client{
			// A provider that hangs must not hold up alert delivery;
			// resolution failures fail the send, so a bounded timeout
			// keeps the failure fast and obvious.
			Timeout: 10 * time.Second,
		},
	}
}

// Scheme implements Provider.
func (p *VaultProvider) Scheme() string { return "vault" }

// Fetch implements Provider. Errors name the path and the HTTP status but
// never the response body, which may contain other secrets.
func (p *VaultProvider) Fetch(ctx context.Context, path, key string) (string, error) {
	if p.Addr == "" {
		return "", fmt.Errorf("vault: address is not configured")
	}
	endpoint := p.Addr + "/v1/" + strings.TrimLeft(path, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("vault: read %s: %w", path, err)
	}
	if p.Token != "" {
		req.Header.Set("X-Vault-Token", p.Token)
	}
	if p.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", p.Namespace)
	}
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("vault: read %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Drain a bounded amount so the connection can be reused without
		// ever surfacing the body (it is secret material).
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("vault: read %s: unexpected status %d", path, resp.StatusCode)
	}
	// Bound the response: a KV entry is small, and reading an unbounded
	// body from a misconfigured proxy is a needless risk.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("vault: read %s: %w", path, err)
	}
	fields, err := vaultFields(body)
	if err != nil {
		return "", fmt.Errorf("vault: read %s: %w", path, err)
	}
	return vaultValue(fields, key)
}

// vaultFields unwraps a KV v1 or v2 response into the map of secret fields.
func vaultFields(body []byte) (map[string]json.RawMessage, error) {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("response is not valid JSON")
	}
	if len(envelope.Data) == 0 {
		return nil, fmt.Errorf("response has no data")
	}
	// KV v2 nests the fields one level deeper under data.data.
	var v2 struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(envelope.Data, &v2); err == nil && len(v2.Data) > 0 {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(v2.Data, &fields); err == nil {
			return fields, nil
		}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Data, &fields); err != nil {
		return nil, fmt.Errorf("response data is not an object")
	}
	return fields, nil
}

// vaultValue selects one field and renders it as a string. With no key, the
// conventional "value" field is used, or the single field if there is
// exactly one so a secret written as {"token":"..."} resolves without
// naming it.
func vaultValue(fields map[string]json.RawMessage, key string) (string, error) {
	if key == "" {
		if raw, ok := fields["value"]; ok {
			return rawString(raw)
		}
		if len(fields) == 1 {
			for _, raw := range fields {
				return rawString(raw)
			}
		}
		return "", fmt.Errorf("no key given and the secret has %d fields (expected a \"value\" field or exactly one field)", len(fields))
	}
	raw, ok := fields[key]
	if !ok {
		return "", fmt.Errorf("field %q not found in secret", key)
	}
	return rawString(raw)
}

// rawString renders a JSON scalar as a string. A JSON string is unquoted; a
// number, boolean or null is used verbatim.
func rawString(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", fmt.Errorf("field is empty")
	}
	return trimmed, nil
}
