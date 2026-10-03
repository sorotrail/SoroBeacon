// Package oidctest is a working OpenID Connect provider for tests: it serves
// discovery, a JWKS and a token endpoint over httptest, and signs real RS256
// ID tokens with a real key, so the code under test exercises the library's
// actual verification path rather than a stub of it.
//
// It exists because the alternative — pointing the auth code at a public
// provider — is not available to a test suite, and because a fake that accepts
// anything proves nothing. This one enforces what a provider enforces: the
// exchange must present the client credentials, the code must be one it handed
// out, and the PKCE verifier must hash to the challenge that code was
// authorised with. Delete PKCE from the production code and the happy path
// fails here, which is the point.
package oidctest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

const (
	// KeyID is the single key this provider publishes.
	keyID = "oidctest-key-1"

	// codeTTL is how long an authorised code may be exchanged. Short, because
	// the tests that care about expiry assert on it.
	codeTTL = 2 * time.Minute
)

// Provider is one fake identity provider, with its own key and its own
// httptest server.
type Provider struct {
	t     testing.TB
	srv   *httptest.Server
	key   *rsa.PrivateKey
	mu    sync.Mutex
	codes map[string]*authCode

	// ClientID and ClientSecret are the registration the tests configure. The
	// token endpoint checks both, so a client that forgets to authenticate is
	// rejected here rather than accepted somewhere.
	ClientID     string
	ClientSecret string

	// next is what the next code exchange returns. Tests set it through
	// RespondWith and RespondWithError.
	next exchangeResponse

	// Exchanges records every token-endpoint request, so a test can assert what
	// was actually sent (a PKCE verifier, a client secret) rather than what the
	// caller hoped would be sent.
	Exchanges []url.Values

	// ExchangeDelay is inserted before the response, 0 by default.
	ExchangeDelay time.Duration

	// FailExchange makes the token endpoint answer 400 regardless of the
	// request, for the "provider is not cooperating" branch.
	FailExchange bool
}

type authCode struct {
	challenge string
	nonce     string
	issued    time.Time
}

type exchangeResponse struct {
	status   int
	idToken  string
	omitID   bool
	accessTk string
}

// New starts a provider and closes it with the test.
func New(t testing.TB) *Provider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("oidctest: generate key: %v", err)
	}
	p := &Provider{
		t:            t,
		key:          key,
		codes:        map[string]*authCode{},
		ClientID:     "sorobeacon-test",
		ClientSecret: "test-client-secret",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("/jwks", p.jwks)
	mux.HandleFunc("/token", p.token)
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

// Issuer is the provider's issuer URL, which is also where discovery is read
// from.
func (p *Provider) Issuer() string { return p.srv.URL }

// AuthURL is the endpoint a browser is sent to. Production code never calls it
// directly — it comes from discovery — but a test that wants to inspect the
// authorize request needs the base.
func (p *Provider) AuthURL() string { return p.srv.URL + "/authorize" }

func (p *Provider) discovery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"issuer":                                p.srv.URL,
		"authorization_endpoint":                p.AuthURL(),
		"token_endpoint":                        p.srv.URL + "/token",
		"jwks_uri":                              p.srv.URL + "/jwks",
		"userinfo_endpoint":                     p.srv.URL + "/userinfo",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported":                      []string{"openid", "profile", "email"},
		"code_challenge_methods_supported":      []string{"S256"},
	})
}

func (p *Provider) jwks(w http.ResponseWriter, r *http.Request) {
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key:       &p.key.PublicKey,
		KeyID:     keyID,
		Algorithm: "RS256",
		Use:       "sig",
	}}}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(set)
}

// Authorize plays the provider's part between the redirect and the callback: it
// checks the authorize request the way a provider would (client id, redirect
// URI, response type, scope, PKCE challenge) and hands out a code bound to it.
//
// The request comes from the URL production code built, so nothing here is
// shaped by the test: a bug in how the authorize request is assembled shows up
// as an error from this function.
func (p *Provider) Authorize(q url.Values) (code string, err error) {
	check := func(name, want string) error {
		if got := q.Get(name); got != want {
			return fmt.Errorf("oidctest: authorize request has %s=%q, want %q", name, got, want)
		}
		return nil
	}
	if err := check("response_type", "code"); err != nil {
		return "", err
	}
	if err := check("client_id", p.ClientID); err != nil {
		return "", err
	}
	if err := check("code_challenge_method", "S256"); err != nil {
		return "", err
	}
	if q.Get("code_challenge") == "" {
		return "", fmt.Errorf("oidctest: authorize request sent no code_challenge")
	}
	if q.Get("nonce") == "" {
		return "", fmt.Errorf("oidctest: authorize request sent no nonce")
	}
	if q.Get("state") == "" {
		return "", fmt.Errorf("oidctest: authorize request sent no state")
	}
	if !hasScope(q.Get("scope"), "openid") {
		return "", fmt.Errorf("oidctest: authorize request did not ask for the openid scope: %q", q.Get("scope"))
	}
	code = fmt.Sprintf("code-%d", time.Now().UnixNano())
	p.mu.Lock()
	p.codes[code] = &authCode{
		challenge: q.Get("code_challenge"),
		nonce:     q.Get("nonce"),
		issued:    time.Now(),
	}
	p.mu.Unlock()
	return code, nil
}

// ClaimsFor is a correctly-formed claim set for one issued code: right issuer,
// audience, times and the nonce that code was authorised with. A test takes the
// map and edits the one field it is probing.
func (p *Provider) ClaimsFor(code string) map[string]any {
	p.mu.Lock()
	entry, ok := p.codes[code]
	p.mu.Unlock()
	nonce := ""
	if ok {
		nonce = entry.nonce
	}
	now := time.Now()
	return map[string]any{
		"iss":   p.srv.URL,
		"sub":   "user-123",
		"aud":   p.ClientID,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
		"nonce": nonce,
		"email": "ops@example.com",
		"name":  "Ops Person",
	}
}

// IDToken signs an ID token with the provider's key.
func (p *Provider) IDToken(claims map[string]any) string {
	return p.IDTokenWith(p.key, claims)
}

// IDTokenWith signs with a key the provider does not publish, which is how a
// test produces a token that looks right and is not.
func (p *Provider) IDTokenWith(key *rsa.PrivateKey, claims map[string]any) string {
	p.t.Helper()
	raw, err := json.Marshal(claims)
	if err != nil {
		p.t.Fatalf("oidctest: marshal claims: %v", err)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: keyID}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		p.t.Fatalf("oidctest: signer: %v", err)
	}
	obj, err := signer.Sign(raw)
	if err != nil {
		p.t.Fatalf("oidctest: sign: %v", err)
	}
	compact, err := obj.CompactSerialize()
	if err != nil {
		p.t.Fatalf("oidctest: serialize: %v", err)
	}
	return compact
}

// OtherKey is a private key this provider has never published, for the
// forged-signature case.
func (p *Provider) OtherKey() *rsa.PrivateKey {
	p.t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		p.t.Fatalf("oidctest: generate key: %v", err)
	}
	return key
}

// LastExchange is the form the token endpoint received most recently, so a test
// can assert what was sent to the provider rather than what was meant to be.
func (p *Provider) LastExchange() url.Values {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.Exchanges) == 0 {
		return url.Values{}
	}
	return p.Exchanges[len(p.Exchanges)-1]
}

// RespondWith is the ID token the next code exchange will hand out.
func (p *Provider) RespondWith(idToken string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.next = exchangeResponse{status: http.StatusOK, idToken: idToken, accessTk: "test-access-token"}
}

// RespondWithoutIDToken makes the next exchange succeed but carry no ID token,
// which is the response a provider gives when the scope list left `openid` out.
func (p *Provider) RespondWithoutIDToken() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.next = exchangeResponse{status: http.StatusOK, omitID: true, accessTk: "test-access-token"}
}

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	p.mu.Lock()
	p.Exchanges = append(p.Exchanges, r.PostForm)
	next := p.next
	fail := p.FailExchange
	p.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if r.PostFormValue("grant_type") != "authorization_code" {
		http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
		return
	}
	if r.PostFormValue("client_id") != p.ClientID || r.PostFormValue("client_secret") != p.ClientSecret {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
		return
	}
	code := r.PostFormValue("code")
	p.mu.Lock()
	entry, ok := p.codes[code]
	delete(p.codes, code) // a code is spendable once
	p.mu.Unlock()
	if !ok {
		http.Error(w, `{"error":"invalid_grant","error_description":"unknown or already spent code"}`, http.StatusBadRequest)
		return
	}
	if fail || time.Since(entry.issued) > codeTTL {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
		return
	}
	// The PKCE check is the reason a stolen authorisation code is not a stolen
	// session, so the fake enforces it rather than trusting the client.
	if !verifyPKCE(entry.challenge, r.PostFormValue("code_verifier")) {
		http.Error(w, `{"error":"invalid_grant","error_description":"pkce verification failed"}`, http.StatusBadRequest)
		return
	}
	if next.status == 0 {
		next = exchangeResponse{status: http.StatusOK, omitID: true}
	}
	w.WriteHeader(next.status)
	tok := map[string]any{
		"token_type":   "Bearer",
		"expires_in":   300,
		"scope":        "openid profile email",
		"access_token": next.accessTk,
	}
	if tok["access_token"] == "" {
		tok["access_token"] = "test-access-token"
	}
	if !next.omitID {
		tok["id_token"] = next.idToken
	}
	_ = json.NewEncoder(w).Encode(tok)
}

func verifyPKCE(challenge, verifier string) bool {
	return verifier != "" && S256Challenge(verifier) == challenge
}

// S256Challenge is the challenge for a verifier (RFC 7636's S256 method), so a
// test can assert that the pair the production code chose really does match.
func S256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func hasScope(scopes, want string) bool {
	return strings.Contains(" "+scopes+" ", " "+want+" ")
}
