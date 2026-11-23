package faultinject

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGeneratorDeterministic(t *testing.T) {
	g1 := NewGenerator(12345)
	g2 := NewGenerator(12345)

	// Same seed should produce same sequence
	for i := 0; i < 100; i++ {
		assert.Equal(t, g1.rng.Float64(), g2.rng.Float64(), "iteration %d", i)
	}
}

func TestGeneratorDifferentSeeds(t *testing.T) {
	g1 := NewGenerator(111)
	g2 := NewGenerator(222)

	diff := false
	for i := 0; i < 100; i++ {
		if g1.rng.Float64() != g2.rng.Float64() {
			diff = true
			break
		}
	}
	assert.True(t, diff, "different seeds should produce different sequences")
}

func TestShouldInjectProbability(t *testing.T) {
	g := NewGenerator(42)
	g.SetFaults(FaultConfig{ConnectionReset: 1.0}) // 100%

	// Should always inject at 100%
	for i := 0; i < 10; i++ {
		assert.True(t, g.ShouldInject(1.0), "iteration %d", i)
	}

	g = NewGenerator(42)
	g.SetFaults(FaultConfig{ConnectionReset: 0.0}) // 0%

	// Should never inject at 0%
	for i := 0; i < 10; i++ {
		assert.False(t, g.ShouldInject(0.0), "iteration %d", i)
	}
}

func TestFaultyTransportConnectionReset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer server.Close()

	gen := NewGenerator(1)
	gen.SetFaults(FaultConfig{ConnectionReset: 1.0}) // Always inject
	rt := NewFaultyTransport(nil, gen)
	client := &http.Client{Transport: rt}

	resp, err := client.Get(server.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection reset")
	assert.Nil(t, resp)
}

func TestFaultyTransportDNSFailure(t *testing.T) {
	gen := NewGenerator(2)
	gen.SetFaults(FaultConfig{DNSFailure: 1.0}) // Always inject
	rt := NewFaultyTransport(nil, gen)
	client := &http.Client{Transport: rt}

	_, err := client.Get("http://example.com")
	require.Error(t, err)
	var dnsErr *net.DNSError
	assert.ErrorAs(t, err, &dnsErr)
	assert.Contains(t, dnsErr.Err, "faultinject")
}

func TestFaultyTransportTLSFailure(t *testing.T) {
	// We can't easily test TLS failure without a real TLS server,
	// but we can verify the error type is returned.
	gen := NewGenerator(3)
	gen.SetFaults(FaultConfig{TLSFailure: 1.0})
	rt := NewFaultyTransport(nil, gen)
	client := &http.Client{Transport: rt}

	_, err := client.Get("https://example.com")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TLS")
}

func TestFaultyTransportHang(t *testing.T) {
	gen := NewGenerator(4)
	gen.SetFaults(FaultConfig{Hang: 1.0})
	rt := NewFaultyTransport(nil, gen)
	client := &http.Client{Transport: rt}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com", nil)
	_, err := client.Do(req)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled))
}

func TestFaultyTransportRateLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer server.Close()

	gen := NewGenerator(5)
	gen.SetFaults(FaultConfig{RateLimit: 1.0})
	rt := NewFaultyTransport(nil, gen)
	client := &http.Client{Transport: rt}

	resp, err := client.Get(server.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	assert.Equal(t, "5", resp.Header.Get("Retry-After"))
}

func TestFaultyTransportServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer server.Close()

	gen := NewGenerator(6)
	gen.SetFaults(FaultConfig{ServerError5xx: 1.0})
	rt := NewFaultyTransport(nil, gen)
	client := &http.Client{Transport: rt}

	resp, err := client.Get(server.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestFaultyTransportSlowBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello world"))
	}))
	defer server.Close()

	gen := NewGenerator(7)
	gen.SetFaults(FaultConfig{SlowBody: 1.0})
	rt := NewFaultyTransport(nil, gen)
	client := &http.Client{Transport: rt}

	start := time.Now()
	resp, err := client.Get(server.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Read the body - should be slowed down
	body := make([]byte, 100)
	n, _ := resp.Body.Read(body)
	elapsed := time.Since(start)
	// Should take some time due to slow reading (at least one 50ms delay)
	assert.GreaterOrEqual(t, elapsed, 40*time.Millisecond, "should be slowed down")

	// But should eventually succeed and return the full body
	assert.Equal(t, "hello world", string(body[:n]))
}

func TestFaultyTransportTruncatedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Write a body longer than the max truncation limit (100 bytes)
		w.Write([]byte("hello world this is a longer body that exceeds one hundred bytes to ensure truncation happens"))
	}))
	defer server.Close()

	gen := NewGenerator(8)
	gen.SetFaults(FaultConfig{TruncatedBody: 1.0})
	rt := NewFaultyTransport(nil, gen)
	client := &http.Client{Transport: rt}

	resp, err := client.Get(server.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	body := make([]byte, 200)
	n, _ := resp.Body.Read(body)
	// Should be truncated (less than full body which is > 100 bytes)
	assert.Less(t, n, 100, "body should be truncated to < 100 bytes")
}

func TestFaultyTransportMultipleFaults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer server.Close()

	// Use a seed that triggers multiple faults across requests
	gen := NewGenerator(999)
	gen.SetFaults(FaultConfig{
		ConnectionReset: 0.3,
		RateLimit:       0.3,
		ServerError5xx:  0.3,
	})
	rt := NewFaultyTransport(nil, gen)
	client := &http.Client{Transport: rt}

	results := map[int]int{}
	for i := 0; i < 100; i++ {
		resp, err := client.Get(server.URL)
		if err != nil {
			if err.Error() == "faultinject: connection reset by peer" {
				results[1]++
			}
			continue
		}
		defer resp.Body.Close()
		results[resp.StatusCode]++
	}

	// Should see a mix of outcomes
	assert.Greater(t, len(results), 1, "should see multiple fault types")
}

func TestTestHelper(t *testing.T) {
	client, cleanup := TestHelper(42, FaultConfig{ConnectionReset: 1.0}, t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	_, err := client.Get(server.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection reset")
}

func TestReportSeedOnFailure(t *testing.T) {
	gen := NewGenerator(99999)
	// This test passes, so ReportSeedOnFailure should not print
	gen.ReportSeedOnFailure(t)
}

// Test with a custom RoundTripper base
func TestFaultyTransportCustomBase(t *testing.T) {
	customTransport := &http.Transport{}
	gen := NewGenerator(10)
	gen.SetFaults(FaultConfig{ConnectionReset: 1.0})
	rt := NewFaultyTransport(customTransport, gen)

	assert.Equal(t, customTransport, rt.base)
}

func TestFaultyTransportPreservesHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "test-value", r.Header.Get("X-Custom-Header"))
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer server.Close()

	gen := NewGenerator(11)
	gen.SetFaults(FaultConfig{ServerError5xx: 0.0}) // No faults
	rt := NewFaultyTransport(nil, gen)
	client := &http.Client{Transport: rt}

	req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	req.Header.Set("X-Custom-Header", "test-value")
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestFaultyTransportContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer server.Close()

	gen := NewGenerator(12)
	gen.SetFaults(FaultConfig{}) // No faults
	rt := NewFaultyTransport(nil, gen)
	client := &http.Client{Transport: rt}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	_, err := client.Do(req)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded))
}
