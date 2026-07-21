package notify

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// httpClient is shared by all outbound webhook-style notifiers.
var httpClient = &http.Client{Timeout: 15 * time.Second}

// postJSON POSTs a JSON body and treats any non-2xx status as an error.
// Error messages include a truncated response body but never the URL,
// since webhook URLs are secrets.
func postJSON(ctx context.Context, url string, body []byte, headers map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", redactURLError(err))
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
