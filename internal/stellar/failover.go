package stellar

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Quarantine bounds for a failing endpoint. The first failure parks an
// endpoint for failoverQuarantineBase and each consecutive failure doubles
// that, up to failoverQuarantineMax. A briefly rate-limited node is therefore
// back in rotation within seconds, while a dead one is only probed
// occasionally instead of on every poll.
const (
	failoverQuarantineBase = 5 * time.Second
	failoverQuarantineMax  = 5 * time.Minute
)

// rpcEndpoint is one RPC URL plus its health state. Each endpoint gets its own
// HTTPClient so the xdrFormat downgrade flag and the request-id counter stay
// per-node, exactly as they would with a single endpoint.
type rpcEndpoint struct {
	url    string
	client *HTTPClient

	mu              sync.Mutex
	failures        int
	quarantineUntil time.Time
}

// quarantined reports whether the endpoint is parked at now. Callers must hold
// ep.mu.
func (ep *rpcEndpoint) quarantined(now time.Time) bool {
	return !ep.quarantineUntil.IsZero() && now.Before(ep.quarantineUntil)
}

// FailoverClient presents an ordered list of RPC endpoints as a single Client.
//
// A public Soroban RPC endpoint rate-limits and goes down, and a monitoring
// tool that stops seeing events is the one failure mode it cannot have. Every
// call here goes to the first endpoint that is not quarantined; a failure that
// is the endpoint's own fault — a transport error, a 429 or a 5xx — parks it
// for an exponentially growing backoff and the call immediately retries on the
// next endpoint, so the poller never sees a gap because the primary was busy.
// A 4xx result or a JSON-RPC error object is *not* a failover trigger: the node
// understood the request and rejected it, so every other endpoint would reject
// it identically and none should be marked unhealthy for it.
//
// A parked endpoint is not silently written off. Once its backoff expires it
// rejoins the rotation and the next call probes it; if that succeeds its
// failure count resets to zero and it is preferred again.
//
// It implements Client and LedgerEntryClient, so it drops in wherever
// *HTTPClient went — the poller, the spec source and the readiness probe all
// keep working against one interface and never learn there is more than one
// endpoint behind it.
type FailoverClient struct {
	endpoints []*rpcEndpoint
	log       *slog.Logger
	// now is time.Now, overridable in tests so a quarantine can be aged out
	// without sleeping.
	now func() time.Time
}

var (
	_ Client            = (*FailoverClient)(nil)
	_ LedgerEntryClient = (*FailoverClient)(nil)
)

// NewFailoverClient returns a Client that tries urls in order. urls must be
// non-empty — config guarantees at least one endpoint. httpClient may be nil,
// and log may be nil, in which case the default logger is used.
func NewFailoverClient(urls []string, httpClient *http.Client, log *slog.Logger) *FailoverClient {
	if log == nil {
		log = slog.Default()
	}
	c := &FailoverClient{log: log, now: time.Now}
	for _, u := range urls {
		c.endpoints = append(c.endpoints, &rpcEndpoint{url: u, client: NewHTTPClient(u, httpClient)})
	}
	return c
}

// Endpoint is one configured RPC endpoint: its URL and the client that talks
// to it, in configured priority order.
type Endpoint struct {
	URL    string
	Client Client
}

// Endpoints returns the underlying per-endpoint clients in priority order.
// Startup uses it to run the network-passphrase check against every endpoint
// rather than only whichever one a call happens to land on.
func (c *FailoverClient) Endpoints() []Endpoint {
	out := make([]Endpoint, 0, len(c.endpoints))
	for _, ep := range c.endpoints {
		out = append(out, Endpoint{URL: ep.url, Client: ep.client})
	}
	return out
}

// EndpointStat is one endpoint's health.
//
// It exists for the structured logs written on quarantine and recovery, and for
// the per-endpoint failure metrics tracked in #26: Stats is the read side of
// that state, so nothing has to reach into the client to get it.
type EndpointStat struct {
	URL string
	// Failures is the number of consecutive failed calls. The first success
	// resets it to zero.
	Failures int
	// Quarantined reports whether calls are currently skipping the endpoint.
	Quarantined bool
	// RetryAt is when the endpoint becomes eligible again, or the zero time
	// if it has never failed.
	RetryAt time.Time
}

// Stats returns every endpoint's health in priority order.
func (c *FailoverClient) Stats() []EndpointStat {
	now := c.now()
	out := make([]EndpointStat, 0, len(c.endpoints))
	for _, ep := range c.endpoints {
		ep.mu.Lock()
		out = append(out, EndpointStat{
			URL:         ep.url,
			Failures:    ep.failures,
			Quarantined: ep.quarantined(now),
			RetryAt:     ep.quarantineUntil,
		})
		ep.mu.Unlock()
	}
	return out
}

func (c *FailoverClient) GetEvents(ctx context.Context, req GetEventsRequest) (*GetEventsResult, error) {
	var res *GetEventsResult
	err := c.call(ctx, "getEvents", func(ctx context.Context, cl *HTTPClient) error {
		got, err := cl.GetEvents(ctx, req)
		if err != nil {
			return err
		}
		res = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (c *FailoverClient) GetLatestLedger(ctx context.Context) (*LatestLedger, error) {
	var res *LatestLedger
	err := c.call(ctx, "getLatestLedger", func(ctx context.Context, cl *HTTPClient) error {
		got, err := cl.GetLatestLedger(ctx)
		if err != nil {
			return err
		}
		res = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (c *FailoverClient) GetHealth(ctx context.Context) (*Health, error) {
	var res *Health
	err := c.call(ctx, "getHealth", func(ctx context.Context, cl *HTTPClient) error {
		got, err := cl.GetHealth(ctx)
		if err != nil {
			return err
		}
		res = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (c *FailoverClient) GetNetwork(ctx context.Context) (*Network, error) {
	var res *Network
	err := c.call(ctx, "getNetwork", func(ctx context.Context, cl *HTTPClient) error {
		got, err := cl.GetNetwork(ctx)
		if err != nil {
			return err
		}
		res = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (c *FailoverClient) GetLedgerEntries(ctx context.Context, keys []string) ([]LedgerEntryResult, error) {
	var res []LedgerEntryResult
	err := c.call(ctx, "getLedgerEntries", func(ctx context.Context, cl *HTTPClient) error {
		got, err := cl.GetLedgerEntries(ctx, keys)
		if err != nil {
			return err
		}
		res = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// call runs fn against the endpoints that are currently in rotation, moving on
// when one fails in a way that is its own fault. method only names the failing
// RPC method in the log line.
func (c *FailoverClient) call(ctx context.Context, method string, fn func(context.Context, *HTTPClient) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	candidates := c.candidates()
	if len(candidates) == 0 {
		return c.quarantinedError()
	}

	var errs []error
	for _, ep := range candidates {
		err := fn(ctx, ep.client)
		switch {
		case err == nil:
			c.recordSuccess(ep)
			return nil
		case ctx.Err() != nil:
			// The caller gave up — a shutdown or an expired deadline. The
			// endpoint told us nothing about its health, and we must not
			// answer a cancelled context by starting another request.
			return err
		case !isFailoverError(err):
			// A legitimate RPC error or a 4xx answer: repeating it on the
			// next endpoint would only repeat the answer, and counting it
			// against this one would quarantine a node that is fine.
			return err
		}
		c.recordFailure(ep, method, err)
		errs = append(errs, fmt.Errorf("rpc endpoint %s: %w", ep.url, err))
	}

	if len(errs) == 1 {
		return errs[0]
	}
	return errors.Join(errs...)
}

// candidates returns the endpoints eligible right now, in priority order.
// Quarantined ones are left out entirely: a parked endpoint is probed once its
// backoff expires, which is what "quarantined with exponential backoff" means —
// retrying it on every call would be exactly the hammering the quarantine
// exists to stop. A caller that finds the list empty gets quarantinedError
// instead of a request.
func (c *FailoverClient) candidates() []*rpcEndpoint {
	now := c.now()
	out := make([]*rpcEndpoint, 0, len(c.endpoints))
	for _, ep := range c.endpoints {
		ep.mu.Lock()
		parked := ep.quarantined(now)
		ep.mu.Unlock()
		if !parked {
			out = append(out, ep)
		}
	}
	return out
}

// quarantinedError explains why no request was made at all: every endpoint is
// parked after failures. It names each one, its failure count and when it will
// be probed again, so an operator reading the poller's error line knows whether
// to wait out the backoff or go fix an endpoint.
func (c *FailoverClient) quarantinedError() error {
	now := c.now()
	parts := make([]string, 0, len(c.endpoints))
	for _, ep := range c.endpoints {
		ep.mu.Lock()
		failures := ep.failures
		remaining := ep.quarantineUntil.Sub(now)
		ep.mu.Unlock()
		if remaining < 0 {
			remaining = 0
		}
		parts = append(parts, fmt.Sprintf("%s (%d failures, retry in %s)",
			ep.url, failures, remaining.Round(time.Second)))
	}
	return fmt.Errorf("all %d RPC endpoints are quarantined after failures: %s",
		len(parts), strings.Join(parts, ", "))
}

// recordFailure parks an endpoint and logs it. The backoff doubles per
// consecutive failure and is capped, so the returned window stops growing
// instead of overflowing once an endpoint has been down for a while.
func (c *FailoverClient) recordFailure(ep *rpcEndpoint, method string, err error) {
	ep.mu.Lock()
	ep.failures++
	backoff := failoverQuarantineBase
	for i := 1; i < ep.failures && backoff < failoverQuarantineMax; i++ {
		backoff *= 2
	}
	if backoff > failoverQuarantineMax {
		backoff = failoverQuarantineMax
	}
	ep.quarantineUntil = c.now().Add(backoff)
	failures := ep.failures
	ep.mu.Unlock()

	c.log.Warn("rpc endpoint quarantined",
		"rpc_url", ep.url,
		"method", method,
		"failures", failures,
		"quarantine", backoff.String(),
		"error", err)
}

// recordSuccess clears an endpoint's failure count. A recovered endpoint is
// worth one line — it is the moment failover undoes itself — but a healthy
// endpoint's routine calls are not.
func (c *FailoverClient) recordSuccess(ep *rpcEndpoint) {
	ep.mu.Lock()
	failures := ep.failures
	ep.failures = 0
	ep.quarantineUntil = time.Time{}
	ep.mu.Unlock()

	if failures > 0 {
		c.log.Info("rpc endpoint recovered", "rpc_url", ep.url, "failures", failures)
	}
}

// isFailoverError reports whether err is the endpoint's fault rather than the
// request's. A 429 or 5xx answer and anything that never produced an HTTP
// response — a refused connection, a TLS failure, a timeout, a body we could
// not parse — are; a 4xx answer or a JSON-RPC error object is not.
func isFailoverError(err error) bool {
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode == http.StatusTooManyRequests || statusErr.StatusCode >= 500
	}
	// A JSON-RPC error object arrives with HTTP 200: the node worked and told
	// us the request was bad, so any other endpoint would say the same.
	var rpcErr *RPCError
	return !errors.As(err, &rpcErr)
}
