package alerts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnricherSlowLookupIsBoundedByTheBudget(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	e, err := NewEnricher(srv.URL, 100*time.Millisecond, time.Minute)
	require.NoError(t, err)
	start := time.Now()
	_, err = e.Fetch(context.Background(), "C1", "t")
	require.Error(t, err)
	assert.Less(t, time.Since(start), 1500*time.Millisecond, "a hung source costs the budget, not more")
}

func TestEnricherCacheIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"label":"x"}`)) }))
	defer srv.Close()
	e, err := NewEnricher(srv.URL, time.Second, time.Minute)
	require.NoError(t, err)
	e.maxEntries = 5
	for i := 0; i < 50; i++ {
		_, err := e.Fetch(context.Background(), "C"+strconv.Itoa(i), "t")
		require.NoError(t, err)
	}
	assert.LessOrEqual(t, e.Len(), 5)
}

func TestEnricherServesStaleWhenSourceFailsThenGivesUp(t *testing.T) {
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"label":"Treasury","_source":"attacker","_stale":false}`))
	}))
	defer srv.Close()
	e, err := NewEnricher(srv.URL, time.Second, time.Minute)
	require.NoError(t, err)
	clock := time.Now()
	e.now = func() time.Time { return clock }

	fresh, err := e.Fetch(context.Background(), "C1", "t")
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(fresh, &m))
	assert.Equal(t, EnrichmentSourceOperator, m[EnrichmentSourceKey], "the source cannot name itself")
	assert.Nil(t, m[EnrichmentStaleKey])

	fail.Store(true)
	clock = clock.Add(2 * time.Minute) // past the TTL, inside the stale window
	stale, err := e.Fetch(context.Background(), "C1", "t")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(stale, &m))
	assert.Equal(t, true, m[EnrichmentStaleKey])

	clock = clock.Add(48 * time.Hour) // beyond the stale window: unenriched
	_, err = e.Fetch(context.Background(), "C1", "t")
	assert.Error(t, err)
}

func TestEnricherInvalidate(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"label":"x"}`))
	}))
	defer srv.Close()
	e, err := NewEnricher(srv.URL, time.Second, time.Minute)
	require.NoError(t, err)
	_, _ = e.Fetch(context.Background(), "C1", "t")
	e.Invalidate("C1")
	_, _ = e.Fetch(context.Background(), "C1", "t")
	assert.EqualValues(t, 2, calls.Load())
}
