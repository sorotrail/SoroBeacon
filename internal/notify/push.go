package notify

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/time/rate"

	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// TypePush is the mobile push channel. It targets Firebase Cloud Messaging
// (HTTP v1), which reaches Android directly and iOS through APNs. FCM is the
// only platform in this first version; the platform-specific parts are
// confined to this file so a second platform is a sibling sender, not a
// rewrite.
const TypePush = "push"

// Push payload and rate limits. The FCM payload ceiling is 4096 bytes; the
// builder stays below pushMaxPayload so a long monitor name or contract id
// is summarised, never cut off mid-field by the platform.
const (
	pushMaxPayload      = 3500
	pushMaxTitleRunes   = 80
	pushMaxBodyRunes    = 180
	pushDeviceBurst     = 6
	pushDeviceEvery     = 10 * time.Second // sustained 6 per minute per device
	pushWorkspaceBurst  = 60
	pushWorkspaceEvery  = time.Second // sustained 60 per minute per workspace
	pushTokenSkew       = time.Minute
	pushDefaultTokenURL = "https://oauth2.googleapis.com/token"
	pushDefaultFCMURL   = "https://fcm.googleapis.com"
	pushScope           = "https://www.googleapis.com/auth/firebase.messaging"
	pushLimiterCap      = 20000
)

// pushConfig configures a push channel:
//
//	{
//	  "project_id": "my-firebase-project",
//	  "client_email": "svc@my-firebase-project.iam.gserviceaccount.com",
//	  "private_key": "-----BEGIN PRIVATE KEY-----...",   // secret
//	  "link_base_url": "https://sorobeacon.example.com"  // optional deep link base
//	}
//
// private_key is the service account's key and goes through the same
// ${secret:...} resolution as every other channel secret. token_url and
// endpoint override the Google endpoints, for tests.
type pushConfig struct {
	ProjectID   string `json:"project_id"`
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	LinkBaseURL string `json:"link_base_url,omitempty"`
	TokenURL    string `json:"token_url,omitempty"`
	Endpoint    string `json:"endpoint,omitempty"`
}

// pushShared is state that must outlive a single notifier, because the
// dispatcher builds a fresh notifier per delivery: the OAuth access tokens
// (so a burst of alerts signs one assertion, not one each) and the rate
// limiters (a limiter that is rebuilt per delivery limits nothing).
type pushShared struct {
	mu       sync.Mutex
	tokens   map[string]pushAccessToken
	limiters map[string]*rate.Limiter
	devices  store.PushDeviceStore
}

type pushAccessToken struct {
	value   string
	expires time.Time
}

func newPushShared() *pushShared {
	return &pushShared{tokens: map[string]pushAccessToken{}, limiters: map[string]*rate.Limiter{}}
}

func (s *pushShared) limiter(key string, every time.Duration, burst int) *rate.Limiter {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.limiters[key]; ok {
		return l
	}
	if len(s.limiters) >= pushLimiterCap {
		// Bounded: a flood of distinct device ids cannot grow the map without
		// limit. Resetting only forgives recent history.
		s.limiters = map[string]*rate.Limiter{}
	}
	l := rate.NewLimiter(rate.Every(every), burst)
	s.limiters[key] = l
	return l
}

// Push delivers an alert to every active device registered on its channel.
type Push struct {
	cfg    pushConfig
	key    *rsa.PrivateKey
	shared *pushShared
	// bound by the dispatcher (see bind): which channel's devices to use.
	channelID int64
	ws        workspace.ID
	bound     bool
	now       func() time.Time
}

// channelBinder is implemented by notifiers that need to know which stored
// channel they serve (the push registry is per channel).
type channelBinder interface {
	bind(channelID int64, ws string) Notifier
}

// NewPush builds a push notifier without a device registry, for validation:
// it checks the configuration and the key, and can send only once bound.
func NewPush(config json.RawMessage) (Notifier, error) {
	return newPush(config, newPushShared())
}

func newPush(config json.RawMessage, shared *pushShared) (Notifier, error) {
	var cfg pushConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("push: invalid config: %w", err)
	}
	if cfg.ProjectID == "" || cfg.ClientEmail == "" || cfg.PrivateKey == "" {
		return nil, fmt.Errorf("push: project_id, client_email and private_key are required")
	}
	key, err := parsePushKey(cfg.PrivateKey)
	if err != nil {
		return nil, err
	}
	if cfg.TokenURL == "" {
		cfg.TokenURL = pushDefaultTokenURL
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = pushDefaultFCMURL
	}
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	cfg.LinkBaseURL = strings.TrimRight(strings.TrimSpace(cfg.LinkBaseURL), "/")
	return &Push{cfg: cfg, key: key, shared: shared, now: time.Now}, nil
}

// parsePushKey reads the PEM private key. The error never includes key
// material.
func parsePushKey(pemText string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(strings.ReplaceAll(pemText, `\n`, "\n")))
	if block == nil {
		return nil, errors.New("push: private_key is not valid PEM")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := k.(*rsa.PrivateKey); ok {
			return rk, nil
		}
		return nil, errors.New("push: private_key must be an RSA key")
	}
	if rk, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return rk, nil
	}
	return nil, errors.New("push: private_key could not be parsed")
}

func (f *Factory) newPush(config json.RawMessage) (Notifier, error) {
	return newPush(config, f.pushState())
}

func (f *Factory) pushState() *pushShared {
	f.pushOnce.Do(func() { f.push = newPushShared() })
	return f.push
}

// WithPushDevices attaches the device registry the push channel reads and
// retires tokens in. Without it a push channel validates but has no devices.
func (f *Factory) WithPushDevices(d store.PushDeviceStore) *Factory {
	f.pushState().mu.Lock()
	f.push.devices = d
	f.push.mu.Unlock()
	return f
}

func (p *Push) bind(channelID int64, ws string) Notifier {
	cp := *p
	cp.channelID = channelID
	cp.bound = true
	if id, err := workspace.Parse(ws); err == nil {
		cp.ws = id
	} else {
		cp.bound = false
	}
	return &cp
}

// PushPriority is the delivery class an alert severity maps to. The mapping
// is deliberate and documented in docs/channels/push.md: only critical alerts
// are allowed to break through a silenced phone.
type PushPriority struct {
	// Android is the FCM Android priority ("high" or "normal").
	Android string
	// APNsPriority is the apns-priority header ("10" immediate, "5" power-aware).
	APNsPriority string
	// InterruptionLevel is the iOS interruption level.
	InterruptionLevel string
	// Critical marks the iOS critical-alert sound, which bypasses silent mode
	// and Do Not Disturb and needs Apple's entitlement on the app.
	Critical bool
}

// PushPriorityFor maps an alert severity to its delivery class. An empty or
// unknown severity is a warning, as everywhere else in the product.
func PushPriorityFor(severity string) PushPriority {
	switch strings.ToLower(severity) {
	case "critical":
		return PushPriority{"high", "10", "critical", true}
	case "info":
		return PushPriority{"normal", "5", "passive", false}
	default:
		return PushPriority{"high", "10", "time-sensitive", false}
	}
}

func pushTruncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func shortContract(id string) string {
	if len(id) > 12 {
		return id[:6] + "…" + id[len(id)-4:]
	}
	return id
}

// buildPushMessage summarises the alert into an FCM message body for one
// token. The notification carries the headline; the data map carries ids and
// a link back for detail, never the event payload.
func (p *Push) buildPushMessage(a Alert, token string) ([]byte, error) {
	pri := PushPriorityFor(a.Severity)
	title := pushTruncate(sanitizePushText(a.MonitorName), pushMaxTitleRunes)
	if title == "" {
		title = "SoroBeacon alert"
	}
	what := a.EventName
	if what == "" {
		what = a.RuleType
	}
	body := sanitizePushText(what)
	if a.ContractID != "" {
		body += " on " + shortContract(a.ContractID)
	}
	if a.Ledger != 0 {
		body += fmt.Sprintf(" (ledger %d)", a.Ledger)
	}
	body = pushTruncate(body, pushMaxBodyRunes)
	data := map[string]string{
		"alert_id": fmt.Sprint(a.ID),
		"severity": strings.ToLower(a.Severity),
	}
	if p.cfg.LinkBaseURL != "" && a.ID != 0 {
		data["link"] = fmt.Sprintf("%s/alerts/%d", p.cfg.LinkBaseURL, a.ID)
	}
	aps := map[string]any{"interruption-level": pri.InterruptionLevel}
	if pri.Critical {
		aps["sound"] = map[string]any{"critical": 1, "name": "default", "volume": 1.0}
	}
	msg := map[string]any{"message": map[string]any{
		"token":        token,
		"notification": map[string]string{"title": title, "body": body},
		"data":         data,
		"android":      map[string]any{"priority": pri.Android},
		"apns": map[string]any{
			"headers": map[string]string{"apns-priority": pri.APNsPriority},
			"payload": map[string]any{"aps": aps},
		},
	}}
	out, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	if len(out) > pushMaxPayload {
		return nil, fmt.Errorf("push: payload is %d bytes, over the %d byte limit", len(out), pushMaxPayload)
	}
	return out, nil
}

// sanitizePushText drops control characters from provider- or
// contract-influenced text.
func sanitizePushText(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

func (p *Push) accessToken(ctx context.Context) (string, error) {
	cacheKey := fmt.Sprintf("%x", sha256.Sum256([]byte(p.cfg.ClientEmail+"|"+p.cfg.TokenURL+"|"+p.cfg.PrivateKey)))[:16]
	p.shared.mu.Lock()
	if t, ok := p.shared.tokens[cacheKey]; ok && p.now().Before(t.expires.Add(-pushTokenSkew)) {
		p.shared.mu.Unlock()
		return t.value, nil
	}
	p.shared.mu.Unlock()

	assertion, err := p.signedAssertion()
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", errors.New("push: build token request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("push: token exchange: %w", redactURLError(err))
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	if res.StatusCode != http.StatusOK {
		// The body can echo the assertion's claims; only the status is kept.
		return "", &HTTPStatusError{StatusCode: res.StatusCode, Body: "token exchange rejected"}
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		return "", errors.New("push: token exchange returned no access token")
	}
	if tok.ExpiresIn <= 0 {
		tok.ExpiresIn = 3600
	}
	p.shared.mu.Lock()
	p.shared.tokens[cacheKey] = pushAccessToken{value: tok.AccessToken, expires: p.now().Add(time.Duration(tok.ExpiresIn) * time.Second)}
	p.shared.mu.Unlock()
	return tok.AccessToken, nil
}

// signedAssertion builds the RS256 JWT Google's OAuth endpoint exchanges for
// an access token. It is the service account's proof of identity.
func (p *Push) signedAssertion() (string, error) {
	now := p.now().Unix()
	b64 := base64.RawURLEncoding
	header := b64.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iss": p.cfg.ClientEmail, "scope": pushScope, "aud": p.cfg.TokenURL,
		"iat": now, "exp": now + 3600,
	})
	if err != nil {
		return "", err
	}
	signing := header + "." + b64.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", errors.New("push: sign assertion")
	}
	return signing + "." + b64.EncodeToString(sig), nil
}

// fcmError is the part of an FCM error response used to classify a failure.
type fcmError struct {
	Error struct {
		Status  string `json:"status"`
		Details []struct {
			ErrorCode string `json:"errorCode"`
		} `json:"details"`
	} `json:"error"`
}

func (e fcmError) code() string {
	for _, d := range e.Error.Details {
		if d.ErrorCode != "" {
			return d.ErrorCode
		}
	}
	return e.Error.Status
}

// Send delivers the alert to each registered device. Invalid-token feedback
// from FCM v1 is synchronous (an UNREGISTERED answer to the send itself), so
// it is consumed here: the device is retired on the spot and never retried.
// It is not an asynchronous feed, so there is nothing else to poll.
//
// The delivery succeeds if at least one device accepted the message. When
// none did, the error carries only a status or an FCM error code: never a
// token, never the key.
func (p *Push) Send(ctx context.Context, a Alert) error {
	if !p.bound || p.shared.devices == nil {
		return errors.New("push: channel has no device registry")
	}
	dctx := workspace.With(ctx, p.ws)
	devices, err := p.shared.devices.ListPushDevices(dctx, p.channelID)
	if err != nil {
		return errors.New("push: list devices")
	}
	if len(devices) == 0 {
		return errors.New("push: no registered devices")
	}
	access, err := p.accessToken(ctx)
	if err != nil {
		return err
	}
	wsLimiter := p.shared.limiter("w:"+string(p.ws), pushWorkspaceEvery, pushWorkspaceBurst)
	sent, limited, retired := 0, 0, 0
	var lastErr error
	for _, d := range devices {
		if !p.shared.limiter(fmt.Sprintf("d:%d:%s", p.channelID, d.DeviceID), pushDeviceEvery, pushDeviceBurst).Allow() || !wsLimiter.Allow() {
			limited++
			continue
		}
		body, err := p.buildPushMessage(a, d.Token)
		if err != nil {
			return err
		}
		switch err := p.post(ctx, access, body); {
		case err == nil:
			sent++
		case isUnregistered(err):
			_ = p.shared.devices.RetirePushDevice(dctx, p.channelID, d.DeviceID)
			retired++
		default:
			lastErr = err
		}
	}
	if sent > 0 {
		return nil
	}
	switch {
	case lastErr != nil:
		return lastErr
	case limited > 0:
		return fmt.Errorf("push: rate limited (%d device(s) skipped)", limited)
	default:
		return fmt.Errorf("push: all %d device token(s) were invalid and have been retired", retired)
	}
}

type unregisteredError struct{}

func (unregisteredError) Error() string { return "push: device token is no longer registered" }

func isUnregistered(err error) bool {
	var u unregisteredError
	return errors.As(err, &u)
}

func (p *Push) post(ctx context.Context, access string, body []byte) error {
	endpoint := p.cfg.Endpoint + "/v1/projects/" + url.PathEscape(p.cfg.ProjectID) + "/messages:send"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return errors.New("push: build request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+access)
	res, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("push: %w", redactURLError(err))
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<14))
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return nil
	}
	var fe fcmError
	_ = json.Unmarshal(raw, &fe)
	code := fe.code()
	if res.StatusCode == http.StatusNotFound || code == "UNREGISTERED" {
		return unregisteredError{}
	}
	// Only the status and FCM's own error code: the response can echo the
	// request, which carries the device token.
	if code == "" {
		code = "error"
	}
	return &HTTPStatusError{StatusCode: res.StatusCode, Body: code}
}
