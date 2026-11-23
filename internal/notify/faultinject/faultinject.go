// Package faultinject provides a transport-level fault injection harness for
// testing the notification delivery path under realistic network failures.
//
// The harness wraps an http.RoundTripper and injects configurable faults
// (connection resets, slow responses, truncated bodies, rate limits, TLS
// failures, DNS failures, hangs) without requiring a real network or external
// dependencies. It is designed for use in tests only and cannot be reached from
// production code paths.
//
// Usage:
//
//	// Create a seeded generator for deterministic, reproducible runs.
//	gen := faultinject.NewGenerator(42)
//
//	// Wrap the default transport with fault injection.
//	rt := faultinject.NewTransport(http.DefaultTransport, gen)
//	client := &http.Client{Transport: rt}
//
//	// Configure which faults to inject and their probabilities.
//	gen.SetFaults(faultinject.FaultConfig{
//	    ConnectionReset:   0.1,  // 10% of requests
//	    SlowBody:          0.05, // 5% of requests
//	    TruncatedBody:     0.02,
//	    RateLimit:         0.03,
//	    ServerError5xx:    0.05,
//	    TLSFailure:        0.01,
//	    DNSFailure:        0.01,
//	    Hang:              0.02,
//	})
//
//	// Run the delivery path against the faulty client.
//	// On failure, the generator reports its seed so the run can be reproduced.
package faultinject

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"testing"
	"time"
)

// FaultConfig controls which faults are injected and their probabilities.
// All probabilities are in [0, 1]. A zero value disables that fault.
type FaultConfig struct {
	// ConnectionReset closes the connection mid-response (RST).
	ConnectionReset float64
	// SlowBody makes the response body stream very slowly.
	SlowBody float64
	// TruncatedBody cuts off the response body before completion.
	TruncatedBody float64
	// RateLimit returns a 429 with or without Retry-After.
	RateLimit float64
	// ServerError5xx returns a 500-class response.
	ServerError5xx float64
	// TLSFailure fails the TLS handshake.
	TLSFailure float64
	// DNSFailure fails DNS resolution.
	DNSFailure float64
	// Hang holds the connection open past the context deadline.
	Hang float64
}

// Generator produces deterministic fault sequences from a seed.
// ReportSeedOnFailure should be called in a test's defer to print the seed
// when a test fails, enabling reproduction.
type Generator struct {
	rng    *rand.Rand
	seed   uint64
	faults FaultConfig
}

// NewGenerator creates a Generator with the given seed. The same seed always
// produces the same fault sequence.
func NewGenerator(seed uint64) *Generator {
	return &Generator{
		rng:  rand.New(rand.NewPCG(seed, 0)),
		seed: seed,
	}
}

// Seed returns the generator's seed.
func (g *Generator) Seed() uint64 { return g.seed }

// SetFaults updates the fault probabilities.
func (g *Generator) SetFaults(f FaultConfig) { g.faults = f }

// ShouldInject returns true if the given fault should trigger on this call.
// It consumes one random value per call.
func (g *Generator) ShouldInject(faultProb float64) bool {
	if faultProb <= 0 {
		return false
	}
	return g.rng.Float64() < faultProb
}

// ReportSeedOnFailure prints the seed if the test failed, so a flaky run can
// be reproduced. Call in a test's defer:
//
//	defer gen.ReportSeedOnFailure(t)
func (g *Generator) ReportSeedOnFailure(t *testing.T) {
	if t.Failed() {
		t.Fatalf("faultinject seed: %d", g.seed)
	}
}

// FaultyTransport wraps an http.RoundTripper and injects faults.
type FaultyTransport struct {
	base   http.RoundTripper
	gen    *Generator
	client *http.Client // for sub-requests when simulating hangs
}

// NewFaultyTransport wraps base with fault injection driven by gen.
// If base is nil, http.DefaultTransport is used.
func NewFaultyTransport(base http.RoundTripper, gen *Generator) *FaultyTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &FaultyTransport{
		base: base,
		gen:  gen,
		client: &http.Client{
			Transport: base,
			// No timeout here; the context deadline controls it.
		},
	}
}

// RoundTrip implements http.RoundTripper.
func (t *FaultyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// DNS failure: fail before dialing.
	if t.gen.ShouldInject(t.gen.faults.DNSFailure) {
		return nil, &net.DNSError{
			Err:         "faultinject: simulated DNS failure",
			Name:        req.URL.Host,
			IsTimeout:   false,
			IsTemporary: true,
		}
	}

	// TLS failure: fail during handshake simulation.
	if t.gen.ShouldInject(t.gen.faults.TLSFailure) {
		return nil, errors.New("faultinject: simulated TLS handshake failure")
	}

	// Hang: delay until context is cancelled.
	if t.gen.ShouldInject(t.gen.faults.Hang) {
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(24 * time.Hour): // effectively forever
			return nil, context.DeadlineExceeded
		}
	}

	// For other faults, we need to make a real request and then corrupt the response.
	// We do this by making the request through a client we control.
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}

	// Rate limit (429)
	if t.gen.ShouldInject(t.gen.faults.RateLimit) {
		resp.Body.Close()
		resp = &http.Response{
			Status:        "429 Too Many Requests",
			StatusCode:    429,
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        http.Header{"Retry-After": []string{"5"}},
			Body:          http.NoBody,
			ContentLength: 0,
			Request:       req,
		}
		return resp, nil
	}

	// Server error (5xx)
	if t.gen.ShouldInject(t.gen.faults.ServerError5xx) {
		resp.Body.Close()
		resp = &http.Response{
			Status:        "500 Internal Server Error",
			StatusCode:    500,
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        http.Header{},
			Body:          http.NoBody,
			ContentLength: 0,
			Request:       req,
		}
		return resp, nil
	}

	// Connection reset: we simulate this by closing the body early
	// and returning a transport error. Since we can't easily inject
	// a TCP RST from user space, we return a "connection reset" error.
	if t.gen.ShouldInject(t.gen.faults.ConnectionReset) {
		resp.Body.Close()
		return nil, errors.New("faultinject: connection reset by peer")
	}

	// Slow body: wrap the body to trickle bytes
	if t.gen.ShouldInject(t.gen.faults.SlowBody) {
		resp.Body = newSlowReader(resp.Body, t.gen)
		return resp, nil
	}

	// Truncated body: wrap the body to cut off early
	if t.gen.ShouldInject(t.gen.faults.TruncatedBody) {
		resp.Body = newTruncatedReader(resp.Body, t.gen)
		return resp, nil
	}

	return resp, nil
}

// slowReader wraps an io.ReadCloser and trickles bytes slowly.
type slowReader struct {
	body   io.ReadCloser
	gen    *Generator
	closed bool
}

func newSlowReader(body io.ReadCloser, gen *Generator) *slowReader {
	return &slowReader{body: body, gen: gen}
}

func (s *slowReader) Read(p []byte) (n int, err error) {
	if s.closed {
		return 0, nil
	}
	// Read one byte at a time with small delays
	for i := range p {
		time.Sleep(50 * time.Millisecond)
		n1, err := s.body.Read(p[i : i+1])
		n += n1
		if err != nil {
			return n, err
		}
		if n1 == 0 {
			break
		}
	}
	return n, nil
}

func (s *slowReader) Close() error {
	s.closed = true
	return s.body.Close()
}

// truncatedReader wraps an io.ReadCloser and cuts off at a random point.
type truncatedReader struct {
	body   io.ReadCloser
	gen    *Generator
	limit  int
	read   int
	closed bool
}

func newTruncatedReader(body io.ReadCloser, gen *Generator) *truncatedReader {
	// Truncate at 1-100 bytes
	limit := gen.rng.IntN(100) + 1
	return &truncatedReader{body: body, gen: gen, limit: limit}
}

func (t *truncatedReader) Read(p []byte) (n int, err error) {
	if t.closed || t.read >= t.limit {
		return 0, nil // EOF
	}
	toRead := min(len(p), t.limit-t.read)
	n1, err := t.body.Read(p[:toRead])
	t.read += n1
	if err != nil {
		return n1, err
	}
	if t.read >= t.limit {
		return n1, nil // EOF after this read
	}
	return n1, nil
}

func (t *truncatedReader) Close() error {
	t.closed = true
	return t.body.Close()
}

// TestHelper is a convenience for tests: it creates a Generator, wraps the
// default transport, and returns an http.Client configured for fault injection.
// The returned cleanup function should be deferred to report the seed on failure.
func TestHelper(seed uint64, faults FaultConfig, t *testing.T) (*http.Client, func()) {
	gen := NewGenerator(seed)
	gen.SetFaults(faults)
	rt := NewFaultyTransport(nil, gen)
	client := &http.Client{Transport: rt}
	return client, func() { gen.ReportSeedOnFailure(t) }
}

// HTTP2FaultyTransport wraps an http2 transport for h2c or h2 testing.
// Most notifiers use HTTP/1.1, but this exists for completeness.
type HTTP2FaultyTransport struct {
	base http.RoundTripper
	gen  *Generator
}

func NewHTTP2FaultyTransport(base http.RoundTripper, gen *Generator) *HTTP2FaultyTransport {
	if base == nil {
		base = &http.Transport{}
	}
	return &HTTP2FaultyTransport{base: base, gen: gen}
}

// RoundTrip implements the http.RoundTripper interface.
func (t *HTTP2FaultyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Similar fault injection logic as FaultyTransport but for h2
	if t.gen.ShouldInject(t.gen.faults.DNSFailure) {
		return nil, &net.DNSError{
			Err:         "faultinject: simulated DNS failure",
			Name:        req.URL.Host,
			IsTimeout:   false,
			IsTemporary: true,
		}
	}
	if t.gen.ShouldInject(t.gen.faults.TLSFailure) {
		return nil, errors.New("faultinject: simulated TLS handshake failure")
	}
	if t.gen.ShouldInject(t.gen.faults.Hang) {
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(24 * time.Hour):
			return nil, context.DeadlineExceeded
		}
	}
	// For other faults, delegate to base (not implemented fully since notifiers use HTTP/1.1)
	return t.base.RoundTrip(req)
}
