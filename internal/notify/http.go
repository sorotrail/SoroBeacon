package notify

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// httpClient is shared by all outbound webhook-style notifiers.
var httpClient = &http.Client{Timeout: 15 * time.Second}

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
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", strings.ToLower(method), redactURLError(err))
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(res.Body, 300))
		return &HTTPStatusError{StatusCode: res.StatusCode, Body: string(snippet)}
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
