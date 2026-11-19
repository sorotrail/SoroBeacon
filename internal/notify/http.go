package notify

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/telemetry"
)

// httpClient is shared by all outbound webhook-style notifiers.
//
// Delivery timeouts are enforced per-delivery on the request context rather
// than globally on the client. This allows each channel to configure its own
// timeout while sharing a single http.Client and Transport, preserving
// connection pooling and HTTP keep-alive across deliveries.
var httpClient = &http.Client{
	Transport: http.DefaultTransport,
}

// HTTPStatusError is a non-2xx answer from a webhook-style destination. It is
// typed so channel health tracking can tell a permanent failure from a
// transient one without matching on message text: 401/403/404 mean the
// credential or the endpoint is gone and retrying can only fail again, while
// a 5xx or a 429 is the provider having a bad day. Body is the truncated
// response snippet, and it never holds channel config because the request URL
// is not part of it.
type HTTPStatusError struct {
	StatusCode int
	Body       string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("status %d: %s", e.StatusCode, e.Body)
}

// postJSON POSTs a JSON body and treats any non-2xx status as an error.
// Error messages include a truncated response body but never the URL,
// since webhook URLs are secrets.
func postJSON(ctx context.Context, url string, body []byte, headers map[string]string) error {
	return requestJSON(ctx, http.MethodPost, url, body, headers)
}

// requestJSON sends a JSON body with an explicit method and treats any
// non-2xx status as an error. Error messages include a truncated response
// body but never the URL, since URLs can embed credentials (a bot token in
// the path, a webhook secret in the query).
func requestJSON(ctx context.Context, method, url string, body []byte, headers map[string]string) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, store.DefaultChannelTimeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	telemetry.InjectHTTP(ctx, req.Header)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", strings.ToLower(method), redactURLError(err))
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		snippet, readErr := io.ReadAll(io.LimitReader(res.Body, 300))
		if ctx.Err() != nil {
			return fmt.Errorf("%s: %w", strings.ToLower(method), redactURLError(ctx.Err()))
		}
		if readErr != nil {
			return fmt.Errorf("%s: %w", strings.ToLower(method), redactURLError(readErr))
		}
		return &StatusError{Code: res.StatusCode, Snippet: string(snippet)}
	}

	// Reading the response body must also honor the timeout: a server that
	// accepts and sends headers but then hangs is cut off at the deadline.
	// Draining the body also allows the TCP connection to be reused for keep-alive.
	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		return fmt.Errorf("%s: %w", strings.ToLower(method), redactURLError(err))
	}
	return nil
}

// StatusError is returned by the shared request helpers for a non-2xx
// response. Its message keeps the historical "status %d: <body>" shape; a
// channel whose destination may echo sensitive input back in the body can
// use errors.As to report Code alone.
type StatusError struct {
	Code    int
	Snippet string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("status %d: %s", e.Code, e.Snippet)
}

// postForm POSTs an application/x-www-form-urlencoded body and treats any
// non-2xx status as an error, with the same no-URL error guarantees as
// requestJSON. It exists for APIs such as Zulip and Pushover that take form
// fields rather than JSON; Authorization and other headers go in headers.
func postForm(ctx context.Context, endpoint string, form url.Values, headers map[string]string) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, store.DefaultChannelTimeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		// url.Parse errors quote the input, which may be a secret.
		return fmt.Errorf("build request: %w", redactURLError(err))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	telemetry.InjectHTTP(ctx, req.Header)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", redactURLError(err))
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		snippet, readErr := io.ReadAll(io.LimitReader(res.Body, 300))
		if ctx.Err() != nil {
			return fmt.Errorf("post: %w", redactURLError(ctx.Err()))
		}
		if readErr != nil {
			return fmt.Errorf("post: %w", redactURLError(readErr))
		}
		return &StatusError{Code: res.StatusCode, Snippet: string(snippet)}
	}
	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		return fmt.Errorf("post: %w", redactURLError(err))
	}
	return nil
}

// redactURLError strips the request URL from net/http errors, which would
// otherwise leak webhook URLs into logs and delivery_attempts.
func redactURLError(err error) error {
	if uerr, ok := err.(interface{ Unwrap() error }); ok {
		if inner := uerr.Unwrap(); inner != nil {
			return inner
		}
	}
	return err
}

// postJSONWithResponse POSTs a JSON body and returns the response body.
// It treats any non-2xx status as an error. Error messages include a
// truncated response body but never the URL, since webhook URLs are secrets.
func postJSONWithResponse(ctx context.Context, url string, body []byte, headers map[string]string) ([]byte, error) {
	return requestJSONWithResponse(ctx, http.MethodPost, url, body, headers)
}

// requestJSONWithResponse sends a JSON body with an explicit method and returns
// the response body. It treats any non-2xx status as an error. Error messages
// include a truncated response body but never the URL, since URLs can embed
// credentials (a bot token in the path, a webhook secret in the query).
func requestJSONWithResponse(ctx context.Context, method, url string, body []byte, headers map[string]string) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, store.DefaultChannelTimeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	telemetry.InjectHTTP(ctx, req.Header)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", strings.ToLower(method), redactURLError(err))
	}
	defer res.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, fmt.Errorf("status %d: %s", res.StatusCode, string(respBody))
	}
	return respBody, nil
}
