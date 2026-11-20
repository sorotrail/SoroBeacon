package notify

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// channelConformanceFixture defines the test harness inputs and documented
// behavioral properties for one registered channel type in DefaultFactory().
//
// Every channel registered in DefaultFactory().Types() must have an entry in
// channelConformanceFixtures; TestChannelConformance fails immediately if a
// new channel type is registered without a corresponding fixture.
type channelConformanceFixture struct {
	// buildConfig returns a valid channel JSON configuration targeting endpoint
	// (an https://127.0.0.1:... URL for HTTP channels or host:port for SMTP)
	// and embedding secret in the channel's credential field. When customTpl
	// is non-empty and supportsTemplate is true, the config sets the channel's
	// template field to customTpl.
	buildConfig func(endpoint, secret, customTpl string) json.RawMessage

	// supportsTemplate indicates whether the channel exposes a user-configurable
	// Go text/template field ("template" or "body_template"). Channels with a
	// fixed structured schema (pagerduty, opsgenie, sns, webhook, federation,
	// googlechat) or fixed text formatter (ntfy, matrix, gotify) set this false.
	supportsTemplate bool

	// transport classifies how the channel speaks to its destination:
	// "http" (default), "smtp" (email), or "aws-sns" (sns).
	transport string

	// knownUntypedStatusOn401And403 documents channels that intercept 401/403
	// or status errors and return an untyped error string, causing IsPermanent
	// to return false and Retryable to return true on 401/403.
	// See docs/channels/conformance.md for the full discrepancy table.
	knownUntypedStatusOn401And403 bool

	// knownUntypedStatusOn404 documents channels whose 404 path returns an
	// untyped error rather than *StatusError or *HTTPStatusError, causing
	// IsPermanent(err) to return false on HTTP 404.
	knownUntypedStatusOn404 bool

	// knownRetryableOn400 documents channels whose 400 path returns an untyped
	// error rather than *StatusError or *HTTPStatusError, causing Retryable(err)
	// to return true on HTTP 400.
	knownRetryableOn400 bool
}

func withTemplateField(m map[string]any, field, customTpl string) json.RawMessage {
	if customTpl != "" {
		m[field] = customTpl
	}
	raw, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return raw
}

var channelConformanceFixtures = map[string]channelConformanceFixture{
	TypeDiscord: {
		supportsTemplate: true,
		buildConfig: func(endpoint, secret, customTpl string) json.RawMessage {
			return withTemplateField(map[string]any{
				"webhook_url": endpoint + "/api/webhooks/12345/" + secret,
			}, "template", customTpl)
		},
	},
	TypeSlack: {
		supportsTemplate: true,
		buildConfig: func(endpoint, secret, customTpl string) json.RawMessage {
			return withTemplateField(map[string]any{
				"webhook_url": endpoint + "/services/T000/B000/" + secret,
			}, "template", customTpl)
		},
	},
	TypeTeams: {
		supportsTemplate: true,
		buildConfig: func(endpoint, secret, customTpl string) json.RawMessage {
			return withTemplateField(map[string]any{
				"webhook_url": endpoint + "/webhookb2/" + secret,
			}, "template", customTpl)
		},
	},
	TypeTelegram: {
		supportsTemplate: true,
		buildConfig: func(endpoint, secret, customTpl string) json.RawMessage {
			return withTemplateField(map[string]any{
				"bot_token": secret,
				"chat_id":   "-100987654",
				"api_base":  endpoint,
			}, "template", customTpl)
		},
	},
	TypeEmail: {
		supportsTemplate: true,
		transport:        "smtp",
		buildConfig: func(endpoint, secret, customTpl string) json.RawMessage {
			host, portStr, _ := net.SplitHostPort(endpoint)
			var port int
			fmt.Sscanf(portStr, "%d", &port)
			return withTemplateField(map[string]any{
				"host":     host,
				"port":     port,
				"username": "beacon-user",
				"password": secret,
				"from":     "beacon@example.test",
				"to":       []string{"ops@example.test"},
			}, "template", customTpl)
		},
	},
	TypeWebhook: {
		supportsTemplate: false,
		buildConfig: func(endpoint, secret, _ string) json.RawMessage {
			return withTemplateField(map[string]any{
				"url":    endpoint + "/hook/" + secret,
				"secret": secret,
			}, "", "")
		},
	},
	TypeNtfy: {
		supportsTemplate: false,
		buildConfig: func(endpoint, secret, _ string) json.RawMessage {
			return withTemplateField(map[string]any{
				"server_url":   endpoint,
				"topic":        "sorobeacon-alerts",
				"access_token": secret,
				"priority":     "high",
			}, "", "")
		},
	},
	TypeMatrix: {
		supportsTemplate: false,
		buildConfig: func(endpoint, secret, _ string) json.RawMessage {
			return withTemplateField(map[string]any{
				"homeserver_url": endpoint,
				"access_token":   secret,
				"room_id":        "!ops:example.org",
			}, "", "")
		},
	},
	TypePagerDuty: {
		supportsTemplate: false,
		buildConfig: func(endpoint, secret, _ string) json.RawMessage {
			return withTemplateField(map[string]any{
				"routing_key": secret,
				"events_url":  endpoint + "/v2/enqueue",
				"severity":    "critical",
			}, "", "")
		},
	},
	// Push fans out to the device tokens registered for the channel after an
	// OAuth token exchange, so it has no single-request wire contract. Its
	// delivery, classification, rate limiting and secret handling are covered
	// in push_test.go.
	TypePush: {
		supportsTemplate: false,
		transport:        "device-push",
		buildConfig: func(endpoint, secret, _ string) json.RawMessage {
			return withTemplateField(map[string]any{
				"project_id":   "conformance",
				"client_email": "svc@conformance.iam.example",
				"private_key":  secret,
				"endpoint":     endpoint,
				"token_url":    endpoint + "/token",
			}, "", "")
		},
	},
	TypeFederation: {
		supportsTemplate: false,
		buildConfig: func(endpoint, secret, _ string) json.RawMessage {
			return withTemplateField(map[string]any{
				"url":   endpoint,
				"token": secret,
			}, "", "")
		},
	},
	TypeTwilio: {
		supportsTemplate:              true,
		knownUntypedStatusOn401And403: true,
		knownUntypedStatusOn404:       true,
		knownRetryableOn400:           true,
		buildConfig: func(endpoint, secret, customTpl string) json.RawMessage {
			return withTemplateField(map[string]any{
				"account_sid": "AC00000000000000000000000000000001",
				"auth_token":  secret,
				"from":        "+15550001111",
				"to":          []string{"+15550002222"},
				"api_base":    endpoint,
			}, "template", customTpl)
		},
	},
	TypeSignal: {
		supportsTemplate: true,
		buildConfig: func(endpoint, secret, customTpl string) json.RawMessage {
			return withTemplateField(map[string]any{
				"api_url":    endpoint + "/" + secret,
				"number":     "+15550001111",
				"recipients": []string{"+15550002222"},
			}, "template", customTpl)
		},
	},
	TypeWebex: {
		supportsTemplate:              true,
		knownUntypedStatusOn401And403: true,
		buildConfig: func(endpoint, secret, customTpl string) json.RawMessage {
			return withTemplateField(map[string]any{
				"bot_token": secret,
				"room_id":   "Y2lzY29zcGFyazovL3VzL1JPT00vMTIz",
				"api_base":  endpoint,
			}, "template", customTpl)
		},
	},
	TypeLark: {
		supportsTemplate:              true,
		knownUntypedStatusOn401And403: true,
		knownUntypedStatusOn404:       true,
		knownRetryableOn400:           true,
		buildConfig: func(endpoint, secret, customTpl string) json.RawMessage {
			return withTemplateField(map[string]any{
				"webhook_url": endpoint + "/open-apis/bot/v2/hook/" + secret,
				"secret":      secret,
			}, "template", customTpl)
		},
	},
	TypeDingTalk: {
		supportsTemplate:              true,
		knownUntypedStatusOn401And403: true,
		knownUntypedStatusOn404:       true,
		knownRetryableOn400:           true,
		buildConfig: func(endpoint, secret, customTpl string) json.RawMessage {
			return withTemplateField(map[string]any{
				"webhook_url": endpoint + "/robot/send?access_token=" + secret,
				"secret":      secret,
			}, "template", customTpl)
		},
	},
	TypeMattermost: {
		supportsTemplate: true,
		buildConfig: func(endpoint, secret, customTpl string) json.RawMessage {
			return withTemplateField(map[string]any{
				"webhook_url": endpoint + "/hooks/" + secret,
				"channel":     "#alerts",
				"username":    "sorobeacon",
			}, "template", customTpl)
		},
	},
	TypeRocketChat: {
		supportsTemplate:              true,
		knownUntypedStatusOn401And403: true,
		knownUntypedStatusOn404:       true,
		knownRetryableOn400:           true,
		buildConfig: func(endpoint, secret, customTpl string) json.RawMessage {
			return withTemplateField(map[string]any{
				"webhook_url": endpoint + "/hooks/rocketchat/" + secret,
				"alias":       "SoroBeacon",
			}, "template", customTpl)
		},
	},
	TypeZulip: {
		supportsTemplate: true,
		buildConfig: func(endpoint, secret, customTpl string) json.RawMessage {
			return withTemplateField(map[string]any{
				"site":      endpoint,
				"bot_email": "beacon-bot@example.zulipchat.com",
				"api_key":   secret,
				"stream":    "alerts",
				"topic":     "sorobeacon",
			}, "template", customTpl)
		},
	},
	TypePushover: {
		supportsTemplate: true,
		buildConfig: func(endpoint, secret, customTpl string) json.RawMessage {
			return withTemplateField(map[string]any{
				"token":    secret,
				"user":     "uQiRzpo4DXghDmr9QzzfQu27cmVRsG",
				"api_base": endpoint,
			}, "template", customTpl)
		},
	},
	TypeGoogleChat: {
		supportsTemplate: false,
		buildConfig: func(endpoint, secret, _ string) json.RawMessage {
			return withTemplateField(map[string]any{
				"webhook_url": endpoint + "/v1/spaces/AAA/messages?key=" + secret,
			}, "", "")
		},
	},
	TypeOpsgenie: {
		supportsTemplate: false,
		buildConfig: func(endpoint, secret, _ string) json.RawMessage {
			return withTemplateField(map[string]any{
				"api_key":  secret,
				"region":   "us",
				"priority": "P2",
				"api_base": endpoint,
			}, "", "")
		},
	},
	TypeGotify: {
		supportsTemplate: false,
		buildConfig: func(endpoint, secret, _ string) json.RawMessage {
			return withTemplateField(map[string]any{
				"server_url": endpoint,
				"token":      secret,
				"priority":   5,
			}, "", "")
		},
	},
	TypeSNS: {
		supportsTemplate:              false,
		transport:                     "aws-sns",
		knownUntypedStatusOn401And403: true,
		knownUntypedStatusOn404:       true,
		knownRetryableOn400:           true,
		buildConfig: func(_, secret, _ string) json.RawMessage {
			return withTemplateField(map[string]any{
				"topic_arn":         "arn:aws:sns:us-east-1:123456789012:sorobeacon-alerts",
				"region":            "us-east-1",
				"access_key_id":     "AKIAIOSFODNN7EXAMPLE",
				"secret_access_key": secret,
			}, "", "")
		},
	},
	TypeJSONTemplate: {
		supportsTemplate: true,
		buildConfig: func(endpoint, secret, customTpl string) json.RawMessage {
			bodyTpl := `{"monitor":{{printf "%q" .MonitorName}},"event":{{printf "%q" .EventID}}}`
			if customTpl != "" {
				bodyTpl = customTpl
			}
			raw, err := json.Marshal(map[string]any{
				"url":           endpoint + "/custom-hook",
				"method":        "POST",
				"headers":       map[string]string{"Authorization": "Bearer " + secret},
				"body_template": bodyTpl,
			})
			if err != nil {
				panic(err)
			}
			return raw
		},
	},
}

// conformanceAlert returns an Alert populated with characters that exercise
// JSON, form, header, and template escaping across every channel.
func conformanceAlert() Alert {
	return Alert{
		ID:          42,
		MonitorID:   7,
		MonitorName: `vault "alpha" <1> & \path`,
		RuleID:      11,
		RuleType:    "value_threshold",
		ContractID:  "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC",
		EventID:     "0004294967296-0000000001",
		EventName:   "transfer",
		Ledger:      1024,
		TxHash:      "9b8a7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b",
		Severity:    "critical",
		Payload:     json.RawMessage(`{"amount":"1000","memo":"quote \" & <tag>"}`),
		CreatedAt:   time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC),
	}
}

// newConformanceFactory returns a Factory wired like production
// (DefaultFactory().WithFederationOrigin) and overrides SNS client creation
// only to point the real AWS SDK SNS client at the local TLS test server
// with single-attempt retries so no traffic ever leaves loopback.
func newConformanceFactory(srvURL string, client *http.Client) *Factory {
	f := DefaultFactory().WithFederationOrigin("https://beacon.example")
	f.Register(TypeSNS, func(cfg json.RawMessage) (Notifier, error) {
		n, err := NewSNS(cfg)
		if err != nil {
			return nil, err
		}
		s := n.(*SNS)
		awsCfg := aws.Config{
			Region:           s.cfg.Region,
			Credentials:      credentials.NewStaticCredentialsProvider("AKIAIOSFODNN7EXAMPLE", s.cfg.SecretAccessKey, ""),
			HTTPClient:       client,
			RetryMaxAttempts: 1,
		}
		s.client = sns.NewFromConfig(awsCfg, func(o *sns.Options) {
			o.BaseEndpoint = aws.String(srvURL)
		})
		return s, nil
	})
	return f
}

// useTLSClient swaps the package-level httpClient to trust the given
// httptest.NewTLSServer for the duration of the test.
func useTLSClient(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	prev := httpClient
	c := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	if srv != nil && srv.TLS != nil {
		tr := srv.Client().Transport.(*http.Transport).Clone()
		tr.TLSClientConfig.InsecureSkipVerify = true
		c.Transport = tr
	}
	httpClient = c
	t.Cleanup(func() { httpClient = prev })
	return c
}

// writeChannelSuccess writes a 200 OK body that satisfies every HTTP-based
// channel parser (JSON channels including Lark/DingTalk/Telegram, and XML
// for AWS SNS Publish).
func writeChannelSuccess(w http.ResponseWriter, channelType string) {
	if channelType == TypeSNS {
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `<PublishResponse xmlns="http://sns.amazonaws.com/doc/2010-03-31/"><PublishResult><MessageId>conformance-msg-1</MessageId></PublishResult></PublishResponse>`)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"ok":true,"status":"success","code":0,"errcode":0,"errmsg":"ok"}`)
}

// TestChannelConformance verifies that every channel registered in
// DefaultFactory() has a conformance fixture and passes the shared delivery,
// failure classification, retry/rate-limit, timeout/cancellation, secret
// non-leakage, and template/escaping contracts.
func TestChannelConformance(t *testing.T) {
	registered := DefaultFactory().WithFederationOrigin("https://beacon.example").Types()
	require.NotEmpty(t, registered, "DefaultFactory().Types() must not be empty")

	for _, chType := range registered {
		if _, ok := channelConformanceFixtures[chType]; !ok {
			t.Fatalf("channel type %q is registered in DefaultFactory() but missing from channelConformanceFixtures in conformance_test.go", chType)
		}
	}
	for chType := range channelConformanceFixtures {
		if !slicesContains(registered, chType) {
			t.Fatalf("channelConformanceFixtures contains %q which is not registered in DefaultFactory().Types()", chType)
		}
	}

	sort.Strings(registered)
	for _, chType := range registered {
		chType := chType
		fix := channelConformanceFixtures[chType]
		t.Run(chType, func(t *testing.T) {
			if fix.transport == "device-push" {
				t.Skip("push fans out to registered devices; covered in push_test.go")
			}
			t.Run("DeliverySuccess", func(t *testing.T) {
				runConformanceDeliverySuccess(t, chType, fix)
			})
			t.Run("PermanentFailureClassification", func(t *testing.T) {
				runConformancePermanentFailure(t, chType, fix)
			})
			t.Run("TransientFailureAndRetryable", func(t *testing.T) {
				runConformanceTransientFailure(t, chType, fix)
			})
			t.Run("RateLimitAndRetryAfter", func(t *testing.T) {
				runConformanceRateLimitAndRetryAfter(t, chType, fix)
			})
			t.Run("TimeoutAndCancellation", func(t *testing.T) {
				runConformanceTimeoutAndCancellation(t, chType, fix)
			})
			t.Run("SecretNonLeakage", func(t *testing.T) {
				runConformanceSecretNonLeakage(t, chType, fix)
			})
			t.Run("TemplateRenderingAndEscaping", func(t *testing.T) {
				runConformanceTemplateAndEscaping(t, chType, fix)
			})
		})
	}
}

func slicesContains(ss []string, target string) bool {
	for _, s := range ss {
		if s == target {
			return true
		}
	}
	return false
}

func runConformanceDeliverySuccess(t *testing.T, chType string, fix channelConformanceFixture) {
	secret := fmt.Sprintf("CONFORMANCE-SECRET-%s-9F8E7D6C", strings.ToUpper(chType))
	alert := conformanceAlert()

	if fix.transport == "smtp" {
		srv := newSMTPTestServer(t, smtpTestOptions{authMechanisms: []string{"PLAIN"}})
		endpoint := fmt.Sprintf("%s:%d", srv.host(), srv.port())
		f := DefaultFactory().WithFederationOrigin("https://beacon.example")
		n, err := f.New(chType, fix.buildConfig(endpoint, secret, ""))
		require.NoError(t, err)
		require.NoError(t, n.Send(context.Background(), alert))
		sess := srv.waitSession(t)
		assert.Contains(t, sess.data, alert.MonitorName)
		assert.Contains(t, sess.data, alert.ContractID)
		return
	}

	var (
		mu          sync.Mutex
		gotRequests int
		gotBody     []byte
		gotCT       string
	)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotRequests++
		gotBody = body
		gotCT = r.Header.Get("Content-Type")
		mu.Unlock()
		writeChannelSuccess(w, chType)
	}))
	defer srv.Close()
	c := useTLSClient(t, srv)

	f := newConformanceFactory(srv.URL, c)
	n, err := f.New(chType, fix.buildConfig(srv.URL, secret, ""))
	require.NoError(t, err)
	require.NoError(t, n.Send(context.Background(), alert))

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, gotRequests, "successful Send must issue exactly 1 request")
	assert.NotEmpty(t, gotBody, "request body must not be empty")

	// Validate wire format integrity when Alert fields contain quotes, angle
	// brackets, ampersands, and backslashes.
	switch {
	case strings.HasPrefix(gotCT, "application/json"):
		assert.True(t, json.Valid(gotBody), "channel %s claimed application/json but sent invalid JSON: %s", chType, string(gotBody))
	case strings.HasPrefix(gotCT, "application/x-www-form-urlencoded"):
		vals, err := url.ParseQuery(string(gotBody))
		require.NoError(t, err, "channel %s sent malformed form body", chType)
		assert.NotEmpty(t, vals)
	}
}

func runConformancePermanentFailure(t *testing.T, chType string, fix channelConformanceFixture) {
	secret := fmt.Sprintf("CONFORMANCE-SECRET-%s-PERM", strings.ToUpper(chType))
	alert := conformanceAlert()

	if fix.transport == "smtp" {
		srv := newSMTPTestServer(t, smtpTestOptions{
			authMechanisms: []string{"PLAIN"},
			authReply:      535,
			authReplyText:  "5.7.8 Authentication credentials invalid",
		})
		endpoint := fmt.Sprintf("%s:%d", srv.host(), srv.port())
		f := DefaultFactory().WithFederationOrigin("https://beacon.example")
		n, err := f.New(chType, fix.buildConfig(endpoint, secret, ""))
		require.NoError(t, err)

		sendErr := n.Send(context.Background(), alert)
		require.Error(t, sendErr)
		assert.True(t, IsPermanent(sendErr), "SMTP 535 auth rejection must be classified as permanent")
		return
	}

	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusBadRequest} {
		status := status
		t.Run(fmt.Sprintf("HTTP_%d", status), func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":"rejected by upstream"}`)
			}))
			defer srv.Close()
			c := useTLSClient(t, srv)

			f := newConformanceFactory(srv.URL, c)
			n, err := f.New(chType, fix.buildConfig(srv.URL, secret, ""))
			require.NoError(t, err)

			sendErr := n.Send(context.Background(), alert)
			require.Error(t, sendErr, "HTTP %d must return a non-nil error", status)

			switch status {
			case http.StatusUnauthorized, http.StatusForbidden:
				if fix.knownUntypedStatusOn401And403 {
					assert.False(t, IsPermanent(sendErr),
						"documented discrepancy for %s on HTTP %d: returns untyped error (%v)", chType, status, sendErr)
				} else {
					assert.True(t, IsPermanent(sendErr),
						"channel %s must classify HTTP %d as permanent (got err: %v)", chType, status, sendErr)
					assert.False(t, Retryable(sendErr),
						"channel %s must not classify HTTP %d as retryable", chType, status)
				}
			case http.StatusNotFound:
				if fix.knownUntypedStatusOn404 {
					assert.False(t, IsPermanent(sendErr),
						"documented discrepancy for %s on HTTP 404: returns untyped error (%v)", chType, sendErr)
				} else {
					assert.True(t, IsPermanent(sendErr),
						"channel %s must classify HTTP 404 as permanent (got err: %v)", chType, sendErr)
					assert.False(t, Retryable(sendErr),
						"channel %s must not classify HTTP 404 as retryable", chType)
				}
			case http.StatusBadRequest:
				assert.False(t, IsPermanent(sendErr), "HTTP 400 is a bad payload, not a permanent channel health failure")
				if fix.knownRetryableOn400 {
					assert.True(t, Retryable(sendErr),
						"documented discrepancy for %s on HTTP 400: returns untyped error (%v)", chType, sendErr)
				} else {
					assert.False(t, Retryable(sendErr),
						"channel %s must not retry HTTP 400 Bad Request", chType)
				}
			}
		})
	}
}

func runConformanceTransientFailure(t *testing.T, chType string, fix channelConformanceFixture) {
	secret := fmt.Sprintf("CONFORMANCE-SECRET-%s-TRANS", strings.ToUpper(chType))
	alert := conformanceAlert()

	if fix.transport == "smtp" {
		srv := newSMTPTestServer(t, smtpTestOptions{
			authMechanisms: []string{"PLAIN"},
			mailReply:      450,
		})
		endpoint := fmt.Sprintf("%s:%d", srv.host(), srv.port())
		f := DefaultFactory().WithFederationOrigin("https://beacon.example")
		n, err := f.New(chType, fix.buildConfig(endpoint, secret, ""))
		require.NoError(t, err)

		sendErr := n.Send(context.Background(), alert)
		require.Error(t, sendErr)
		assert.False(t, IsPermanent(sendErr), "SMTP 450 transient failure must not be classified as permanent")
		assert.True(t, Retryable(sendErr), "SMTP 450 transient failure must be retryable")
		return
	}

	for _, status := range []int{
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusTooManyRequests,
	} {
		status := status
		t.Run(fmt.Sprintf("HTTP_%d", status), func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if status == http.StatusTooManyRequests {
					w.Header().Set("Retry-After", "2")
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":"temporary upstream outage"}`)
			}))
			defer srv.Close()
			c := useTLSClient(t, srv)

			f := newConformanceFactory(srv.URL, c)
			n, err := f.New(chType, fix.buildConfig(srv.URL, secret, ""))
			require.NoError(t, err)

			sendErr := n.Send(context.Background(), alert)
			require.Error(t, sendErr, "HTTP %d must fail the delivery attempt", status)
			assert.False(t, IsPermanent(sendErr), "HTTP %d must never be classified as permanent", status)
			assert.True(t, Retryable(sendErr), "HTTP %d must be classified as retryable", status)
		})
	}
}

func runConformanceRateLimitAndRetryAfter(t *testing.T, chType string, fix channelConformanceFixture) {
	secret := fmt.Sprintf("CONFORMANCE-SECRET-%s-RL", strings.ToUpper(chType))
	alert := conformanceAlert()

	if fix.transport == "smtp" {
		// Drive Dispatcher retry against an in-process SMTP listener that
		// replies 421 on the first connection and 250 on the second.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })

		var connCount atomic.Int32
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				n := connCount.Add(1)
				go func(c net.Conn, attempt int32) {
					defer c.Close()
					if attempt == 1 {
						_, _ = io.WriteString(c, "421 4.7.0 rate limit exceeded, try again\r\n")
						return
					}
					r := bufio.NewReader(c)
					_, _ = io.WriteString(c, "220 localhost ESMTP\r\n")
					for {
						line, err := r.ReadString('\n')
						if err != nil {
							return
						}
						cmd := strings.ToUpper(strings.TrimSpace(line))
						switch {
						case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
							_, _ = io.WriteString(c, "250-localhost\r\n250 AUTH PLAIN\r\n")
						case strings.HasPrefix(cmd, "AUTH"):
							_, _ = io.WriteString(c, "235 2.7.0 OK\r\n")
						case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
							_, _ = io.WriteString(c, "250 2.1.0 OK\r\n")
						case strings.HasPrefix(cmd, "DATA"):
							_, _ = io.WriteString(c, "354 End data with <CR><LF>.<CR><LF>\r\n")
							for {
								dline, err := r.ReadString('\n')
								if err != nil || strings.TrimRight(dline, "\r\n") == "." {
									break
								}
							}
							_, _ = io.WriteString(c, "250 2.0.0 queued\r\n")
						case strings.HasPrefix(cmd, "QUIT"):
							_, _ = io.WriteString(c, "221 2.0.0 bye\r\n")
							return
						default:
							_, _ = io.WriteString(c, "250 OK\r\n")
						}
					}
				}(conn, n)
			}
		}()

		cfg := fix.buildConfig(ln.Addr().String(), secret, "")
		st := &conformanceStore{
			channels: []store.Channel{{
				ID:      1,
				Name:    "conformance-" + chType,
				Type:    chType,
				Config:  cfg,
				Enabled: true,
			}},
		}
		f := DefaultFactory().WithFederationOrigin("https://beacon.example")
		d := NewDispatcher(st, f, slog.New(slog.NewTextHandler(io.Discard, nil)))
		d.BaseBackoff = 5 * time.Millisecond
		d.MaxAttempts = 3

		d.Dispatch(context.Background(), alert)
		require.Len(t, st.attempts, 2, "dispatcher must retry transient SMTP 421 and succeed on attempt 2")
		assert.Equal(t, "failed", st.attempts[0].Status)
		assert.Equal(t, "success", st.attempts[1].Status)
		return
	}

	var attempts atomic.Int32
	var seenRetryAfter atomic.Bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			if dur, ok := ParseRetryAfter(w.Header().Get("Retry-After")); ok && dur == time.Second {
				seenRetryAfter.Store(true)
			}
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":"rate_limited","retry_after":1}`)
			return
		}
		writeChannelSuccess(w, chType)
	}))
	defer srv.Close()
	c := useTLSClient(t, srv)

	cfg := fix.buildConfig(srv.URL, secret, "")
	st := &conformanceStore{
		channels: []store.Channel{{
			ID:      1,
			Name:    "conformance-" + chType,
			Type:    chType,
			Config:  cfg,
			Enabled: true,
		}},
	}
	f := newConformanceFactory(srv.URL, c)
	d := NewDispatcher(st, f, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.BaseBackoff = 5 * time.Millisecond
	d.MaxAttempts = 3

	d.Dispatch(context.Background(), alert)

	assert.True(t, seenRetryAfter.Load(), "Retry-After header must be parseable")
	require.Len(t, st.attempts, 2, "dispatcher must retry HTTP 429 and succeed on attempt 2")
	assert.Equal(t, "failed", st.attempts[0].Status)
	assert.Equal(t, "success", st.attempts[1].Status)
	require.NotEmpty(t, st.healthUpdates)
	assert.True(t, st.healthUpdates[len(st.healthUpdates)-1].Success)
}

func runConformanceTimeoutAndCancellation(t *testing.T, chType string, fix channelConformanceFixture) {
	secret := fmt.Sprintf("CONFORMANCE-SECRET-%s-TIMEOUT", strings.ToUpper(chType))
	alert := conformanceAlert()

	if fix.transport == "smtp" {
		srv := newSMTPTestServer(t, smtpTestOptions{stallGreeting: true})
		endpoint := fmt.Sprintf("%s:%d", srv.host(), srv.port())
		f := DefaultFactory().WithFederationOrigin("https://beacon.example")
		n, err := f.New(chType, fix.buildConfig(endpoint, secret, ""))
		require.NoError(t, err)

		// 1. Pre-cancelled context
		canceledCtx, cancel := context.WithCancel(context.Background())
		cancel()
		err = n.Send(canceledCtx, alert)
		require.Error(t, err, "Send with pre-cancelled context must fail")
		assert.False(t, IsPermanent(err), "context cancellation must not be classified as permanent")

		// 2. Short deadline against stalled SMTP server
		deadlineCtx, cancelDeadline := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancelDeadline()
		start := time.Now()
		err = n.Send(deadlineCtx, alert)
		elapsed := time.Since(start)
		require.Error(t, err, "Send against stalled SMTP server must time out")
		assert.Less(t, elapsed, 2*time.Second, "SMTP Send must respect context deadline promptly")
		assert.False(t, IsPermanent(err), "SMTP timeout must not be classified as permanent")
		return
	}

	unblock := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-unblock:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(unblock)
	c := useTLSClient(t, srv)

	f := newConformanceFactory(srv.URL, c)
	n, err := f.New(chType, fix.buildConfig(srv.URL, secret, ""))
	require.NoError(t, err)

	// 1. Pre-cancelled context
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	err = n.Send(canceledCtx, alert)
	require.Error(t, err, "Send with pre-cancelled context must fail")
	assert.False(t, IsPermanent(err), "context cancellation must not be classified as permanent")

	// 2. Short deadline against hanging HTTP server
	deadlineCtx, cancelDeadline := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancelDeadline()
	start := time.Now()
	err = n.Send(deadlineCtx, alert)
	elapsed := time.Since(start)
	require.Error(t, err, "Send against hanging server must fail on deadline")
	assert.Less(t, elapsed, 2*time.Second, "Send must abort when context deadline expires")
	assert.False(t, IsPermanent(err), "timeout must not be classified as permanent")
}

func runConformanceSecretNonLeakage(t *testing.T, chType string, fix channelConformanceFixture) {
	secret := fmt.Sprintf("CONFORMANCE-SECRET-%s-DO-NOT-LEAK-9F8E7D6C", strings.ToUpper(chType))
	alert := conformanceAlert()

	assertNoSecret := func(t *testing.T, surface, text string) {
		t.Helper()
		assert.NotContains(t, text, secret, "channel %s leaked credential secret in %s: %q", chType, surface, text)
	}

	if fix.transport == "smtp" {
		// 1. SMTP 535 auth failure where server echoes the password back in the reply text
		srv := newSMTPTestServer(t, smtpTestOptions{
			authMechanisms: []string{"PLAIN"},
			authReply:      535,
			authReplyText:  "5.7.8 rejected password=" + secret,
		})
		endpoint := fmt.Sprintf("%s:%d", srv.host(), srv.port())
		cfg := fix.buildConfig(endpoint, secret, "")

		var logBuf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
		st := &conformanceStore{
			channels: []store.Channel{{
				ID:      1,
				Name:    "conformance-" + chType,
				Type:    chType,
				Config:  cfg,
				Enabled: true,
			}},
		}
		f := DefaultFactory().WithFederationOrigin("https://beacon.example")
		n, err := f.New(chType, cfg)
		require.NoError(t, err)

		sendErr := n.Send(context.Background(), alert)
		require.Error(t, sendErr)
		assertNoSecret(t, "Send() error", sendErr.Error())
		assertNoSecret(t, "TestHealthUpdate.Error", TestHealthUpdate(sendErr, time.Now()).Error)

		d := NewDispatcher(st, f, logger)
		d.BaseBackoff = time.Millisecond
		d.MaxAttempts = 1
		d.Dispatch(context.Background(), alert)

		for _, att := range st.attempts {
			assertNoSecret(t, "DeliveryAttempt.ResponseSnippet", att.ResponseSnippet)
		}
		for _, dl := range st.deadLetters {
			assertNoSecret(t, "DeadLetter.LastError", dl.LastError)
		}
		for _, hu := range st.healthUpdates {
			assertNoSecret(t, "ChannelHealthUpdate.Error", hu.Error)
		}
		assertNoSecret(t, "slog output", logBuf.String())
		return
	}

	// 1. HTTP 401 failure (and echoed token key=value in body)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, fmt.Sprintf(`{"error":"invalid token=%s"}`, secret))
	}))
	c := useTLSClient(t, srv)
	cfg := fix.buildConfig(srv.URL, secret, "")
	f := newConformanceFactory(srv.URL, c)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
	st := &conformanceStore{
		channels: []store.Channel{{
			ID:      1,
			Name:    "conformance-" + chType,
			Type:    chType,
			Config:  cfg,
			Enabled: true,
		}},
	}
	d := NewDispatcher(st, f, logger)
	d.BaseBackoff = time.Millisecond
	d.MaxAttempts = 2
	d.Dispatch(context.Background(), alert)

	for _, att := range st.attempts {
		assertNoSecret(t, "DeliveryAttempt.ResponseSnippet (HTTP 401)", att.ResponseSnippet)
	}
	for _, dl := range st.deadLetters {
		assertNoSecret(t, "DeadLetter.LastError (HTTP 401)", dl.LastError)
	}
	for _, hu := range st.healthUpdates {
		assertNoSecret(t, "ChannelHealthUpdate.Error (HTTP 401)", hu.Error)
	}
	assertNoSecret(t, "slog output (HTTP 401)", logBuf.String())

	// 2. Transport-level connection refusal (closed server, where net/url.Error
	// carries the full request URL including embedded path/query secrets).
	srv.Close()
	n, err := f.New(chType, cfg)
	require.NoError(t, err)
	transportErr := n.Send(context.Background(), alert)
	require.Error(t, transportErr)
	assertNoSecret(t, "Send() transport error", transportErr.Error())
	assertNoSecret(t, "TestHealthUpdate.Error on transport error", TestHealthUpdate(transportErr, time.Now()).Error)

	logBuf.Reset()
	st2 := &conformanceStore{
		channels: []store.Channel{{
			ID:      1,
			Name:    "conformance-" + chType,
			Type:    chType,
			Config:  cfg,
			Enabled: true,
		}},
	}
	d2 := NewDispatcher(st2, f, logger)
	d2.BaseBackoff = time.Millisecond
	d2.MaxAttempts = 1
	d2.Dispatch(context.Background(), alert)

	for _, att := range st2.attempts {
		assertNoSecret(t, "DeliveryAttempt.ResponseSnippet (transport error)", att.ResponseSnippet)
	}
	for _, dl := range st2.deadLetters {
		assertNoSecret(t, "DeadLetter.LastError (transport error)", dl.LastError)
	}
	for _, hu := range st2.healthUpdates {
		assertNoSecret(t, "ChannelHealthUpdate.Error (transport error)", hu.Error)
	}
	assertNoSecret(t, "slog output (transport error)", logBuf.String())
}

func runConformanceTemplateAndEscaping(t *testing.T, chType string, fix channelConformanceFixture) {
	secret := fmt.Sprintf("CONFORMANCE-SECRET-%s-TPL", strings.ToUpper(chType))
	alert := conformanceAlert()

	if !fix.supportsTemplate {
		// Fixed-schema channel: verify special characters in Alert do not break
		// outbound serialization (covered in DeliverySuccess, pin here too).
		runConformanceDeliverySuccess(t, chType, fix)
		return
	}

	customTpl := `CONFORMANCE_TPL[{{.MonitorName}}|{{.RuleType}}|{{.EventID}}]`
	if chType == TypeJSONTemplate {
		customTpl = `{"custom_tpl": "CONFORMANCE_TPL[{{.RuleType}}|{{.EventID}}]"}`
	}

	if fix.transport == "smtp" {
		srv := newSMTPTestServer(t, smtpTestOptions{authMechanisms: []string{"PLAIN"}})
		endpoint := fmt.Sprintf("%s:%d", srv.host(), srv.port())
		f := DefaultFactory().WithFederationOrigin("https://beacon.example")

		// Invalid template must be rejected at construction time
		_, badErr := f.New(chType, fix.buildConfig(endpoint, secret, "{{.Unclosed"))
		require.Error(t, badErr, "channel %s must reject malformed template at construction", chType)

		n, err := f.New(chType, fix.buildConfig(endpoint, secret, customTpl))
		require.NoError(t, err)
		require.NoError(t, n.Send(context.Background(), alert))
		sess := srv.waitSession(t)
		assert.Contains(t, sess.data, "CONFORMANCE_TPL[")
		assert.Contains(t, sess.data, alert.EventID)
		return
	}

	var gotBody []byte
	var gotCT string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotCT = r.Header.Get("Content-Type")
		writeChannelSuccess(w, chType)
	}))
	defer srv.Close()
	c := useTLSClient(t, srv)
	f := newConformanceFactory(srv.URL, c)

	// 1. Invalid template must be rejected at construction time
	_, badErr := f.New(chType, fix.buildConfig(srv.URL, secret, "{{.Unclosed"))
	require.Error(t, badErr, "channel %s must reject malformed template at construction", chType)

	// 2. Custom template renders expected fields and preserves wire escaping
	n, err := f.New(chType, fix.buildConfig(srv.URL, secret, customTpl))
	require.NoError(t, err)
	require.NoError(t, n.Send(context.Background(), alert))

	bodyStr := string(gotBody)
	if strings.HasPrefix(gotCT, "application/x-www-form-urlencoded") {
		vals, parseErr := url.ParseQuery(bodyStr)
		require.NoError(t, parseErr)
		var decoded strings.Builder
		for _, vs := range vals {
			for _, v := range vs {
				decoded.WriteString(v)
			}
		}
		bodyStr = decoded.String()
	} else if strings.HasPrefix(gotCT, "application/json") {
		assert.True(t, json.Valid(gotBody), "channel %s custom template output must remain valid JSON", chType)
	}
	assert.Contains(t, bodyStr, "CONFORMANCE_TPL[", "channel %s must render custom template", chType)
	assert.Contains(t, bodyStr, alert.EventID, "channel %s rendered template must contain EventID", chType)
}

// conformanceStore is an in-memory DispatchStore for driving Dispatcher.Dispatch
// across every registered channel type in conformance tests.
type conformanceStore struct {
	mu            sync.Mutex
	channels      []store.Channel
	attempts      []store.DeliveryAttempt
	deadLetters   []store.DeadLetter
	healthUpdates []store.ChannelHealthUpdate
}

func (s *conformanceStore) ListChannelsForMonitor(_ context.Context, _ int64) ([]store.Channel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.Channel, len(s.channels))
	copy(out, s.channels)
	return out, nil
}

func (s *conformanceStore) ListChannelsByIDs(_ context.Context, _ []int64) ([]store.Channel, error) {
	return s.ListChannelsForMonitor(context.Background(), 0)
}

func (s *conformanceStore) RecordDeliveryAttempt(_ context.Context, d *store.DeliveryAttempt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts = append(s.attempts, *d)
	return nil
}

func (s *conformanceStore) CreateDeadLetter(_ context.Context, d *store.DeadLetter) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deadLetters = append(s.deadLetters, *d)
	return nil
}

func (s *conformanceStore) GetAlert(_ context.Context, id int64) (*store.Alert, error) {
	return &store.Alert{ID: id}, nil
}

func (s *conformanceStore) GetChannel(_ context.Context, id int64) (*store.Channel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.channels {
		if s.channels[i].ID == id {
			ch := s.channels[i]
			return &ch, nil
		}
	}
	return nil, store.ErrNotFound
}

func (s *conformanceStore) ListDeliveryAttempts(_ context.Context, _ int64, _ string) ([]store.DeliveryAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.DeliveryAttempt, len(s.attempts))
	copy(out, s.attempts)
	return out, nil
}

func (s *conformanceStore) DeleteDeadLetter(_ context.Context, _ int64) error { return nil }

func (s *conformanceStore) ListChannels(_ context.Context, _ bool) ([]store.Channel, error) {
	return s.ListChannelsForMonitor(context.Background(), 0)
}

func (s *conformanceStore) ListInhibitionsForTarget(_ context.Context, _ int64) ([]store.Inhibition, error) {
	return nil, nil
}

func (s *conformanceStore) RuleFiredWithin(_ context.Context, _ int64, _ time.Duration) (bool, error) {
	return false, nil
}

func (s *conformanceStore) ListRuleAlertTimes(_ context.Context, _ int64, _ time.Time) ([]time.Time, error) {
	return nil, nil
}

func (s *conformanceStore) MarkAlertInhibited(_ context.Context, _, _ int64) error { return nil }

func (s *conformanceStore) ActiveMaintenanceWindow(_ context.Context, _ int64, _ string, _ time.Time) (*store.MaintenanceWindow, error) {
	return nil, nil
}

func (s *conformanceStore) SetAlertSuppressed(_ context.Context, _ int64, _ string) error {
	return nil
}

func (s *conformanceStore) RecordChannelHealth(_ context.Context, _ int64, u store.ChannelHealthUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.healthUpdates = append(s.healthUpdates, u)
	return nil
}

func (s *conformanceStore) GetEscalationPolicyForMonitor(_ context.Context, _ int64) (*store.EscalationPolicy, error) {
	return nil, store.ErrNotFound
}

func (s *conformanceStore) GetEscalationPolicy(_ context.Context, _ int64) (*store.EscalationPolicy, error) {
	return nil, store.ErrNotFound
}

func (s *conformanceStore) ScheduleEscalation(_ context.Context, _, _ int64, _ json.RawMessage, _ int, _ time.Time) error {
	return nil
}

func (s *conformanceStore) DueEscalations(_ context.Context, _ time.Time, _ int) ([]store.EscalationRun, error) {
	return nil, nil
}

func (s *conformanceStore) AdvanceEscalation(_ context.Context, _ int64, _ int, _ time.Time) error {
	return nil
}

func (s *conformanceStore) CompleteEscalation(_ context.Context, _ int64) error { return nil }
