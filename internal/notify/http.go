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
		return fmt.Errorf("status %d: %s", res.StatusCode, string(snippet))
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
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
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
