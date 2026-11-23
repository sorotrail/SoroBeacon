package usage

import (
	"fmt"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// scrapeMetrics returns the Prometheus text exposition the handler serves.
func scrapeMetrics(t *testing.T, met *metrics.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	met.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	return string(body)
}

func storedRowsLine(met *metrics.Metrics, workspaceID, resource string, value int) string {
	return fmt.Sprintf(`sorobeacon_workspace_stored_rows{resource=%q,workspace=%q} %d`,
		resource, met.WorkspaceMetricLabel(workspaceID), value)
}

func TestObserveStorageCountsSetsEveryStoredRowsGauge(t *testing.T) {
	met := metrics.New()
	meter := NewMeter(nil, met, nil)

	meter.ObserveStorageCounts("acme", store.WorkspaceStorageCounts{
		Monitors:         3,
		Rules:            5,
		Channels:         2,
		Alerts:           11,
		DeliveryAttempts: 7,
		DeadLetters:      1,
	})

	out := scrapeMetrics(t, met)
	for resource, want := range map[string]int{
		"monitors":          3,
		"rules":             5,
		"channels":          2,
		"alerts":            11,
		"delivery_attempts": 7,
		"dead_letters":      1,
	} {
		assert.Contains(t, out, storedRowsLine(met, "acme", resource, want), resource)
	}

	// A later observation replaces the values: these are gauges of the current
	// row counts, not counters.
	meter.ObserveStorageCounts("acme", store.WorkspaceStorageCounts{Monitors: 1})
	out = scrapeMetrics(t, met)
	assert.Contains(t, out, storedRowsLine(met, "acme", "monitors", 1))
	assert.Contains(t, out, storedRowsLine(met, "acme", "alerts", 0))
}

func TestObserveStorageCountsBlankWorkspaceIsTheDefault(t *testing.T) {
	met := metrics.New()
	NewMeter(nil, met, nil).ObserveStorageCounts("  ", store.WorkspaceStorageCounts{Monitors: 4})

	assert.Contains(t, scrapeMetrics(t, met), storedRowsLine(met, string(workspace.Default), "monitors", 4))
}

func TestObserveStorageCountsWithoutMetricsIsANoOp(t *testing.T) {
	counts := store.WorkspaceStorageCounts{Monitors: 1}

	var nilMeter *Meter
	assert.NotPanics(t, func() { nilMeter.ObserveStorageCounts("acme", counts) })
	assert.NotPanics(t, func() { NewMeter(nil, nil, nil).ObserveStorageCounts("acme", counts) })
}
