package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/metrics"
)

// TestAddUTCMonthsBoundaryArithmetic verifies that month-offset calculation
// never suffers from time.AddDate day-overflow across 28/29/30/31-day months,
// leap years, year boundaries, or DST/timezone transitions.
func TestAddUTCMonthsBoundaryArithmetic(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	tokyo := time.FixedZone("JST", 9*3600)

	cases := []struct {
		name         string
		from         time.Time
		months       int
		wantMonth    time.Time
		wantSequence []string
	}{
		{
			name:      "Jan 31 across leap-year February (no March overflow)",
			from:      time.Date(2024, time.January, 31, 23, 59, 59, 999999999, time.UTC),
			months:    4,
			wantMonth: time.Date(2024, time.February, 1, 0, 0, 0, 0, time.UTC),
			wantSequence: []string{
				"alerts_2024_01",
				"alerts_2024_02",
				"alerts_2024_03",
				"alerts_2024_04",
			},
		},
		{
			name:      "Jan 31 across non-leap-year February",
			from:      time.Date(2025, time.January, 31, 14, 0, 0, 0, time.UTC),
			months:    3,
			wantMonth: time.Date(2025, time.February, 1, 0, 0, 0, 0, time.UTC),
			wantSequence: []string{
				"alerts_2025_01",
				"alerts_2025_02",
				"alerts_2025_03",
			},
		},
		{
			name:      "leap day Feb 29 2024 forward 12 months",
			from:      time.Date(2024, time.February, 29, 18, 30, 0, 0, time.UTC),
			months:    3,
			wantMonth: time.Date(2024, time.March, 1, 0, 0, 0, 0, time.UTC),
			wantSequence: []string{
				"alerts_2024_02",
				"alerts_2024_03",
				"alerts_2024_04",
			},
		},
		{
			name:      "May 31 across 30-day June",
			from:      time.Date(2026, time.May, 31, 22, 0, 0, 0, time.UTC),
			months:    3,
			wantMonth: time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC),
			wantSequence: []string{
				"alerts_2026_05",
				"alerts_2026_06",
				"alerts_2026_07",
			},
		},
		{
			name:      "US spring-forward DST transition in America/New_York",
			from:      time.Date(2026, time.March, 8, 2, 30, 0, 0, ny),
			months:    3,
			wantMonth: time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC),
			wantSequence: []string{
				"alerts_2026_03",
				"alerts_2026_04",
				"alerts_2026_05",
			},
		},
		{
			name:      "US fall-back DST transition on month boundary in America/New_York",
			from:      time.Date(2026, time.October, 31, 21, 0, 0, 0, ny), // 2026-11-01 01:00 UTC
			months:    3,
			wantMonth: time.Date(2026, time.December, 1, 0, 0, 0, 0, time.UTC),
			wantSequence: []string{
				"alerts_2026_11",
				"alerts_2026_12",
				"alerts_2027_01",
			},
		},
		{
			name:      "positive UTC offset before local midnight belongs to prior UTC month",
			from:      time.Date(2026, time.April, 1, 3, 0, 0, 0, tokyo), // 2026-03-31 18:00 UTC
			months:    3,
			wantMonth: time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC),
			wantSequence: []string{
				"alerts_2026_03",
				"alerts_2026_04",
				"alerts_2026_05",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := addUTCMonths(tc.from, 1)
			assert.Equal(t, tc.wantMonth, next)
			got := requiredAlertPartitions(tc.from, tc.months)
			assert.Equal(t, tc.wantSequence, got)
		})
	}
}

func TestComputePartitionRunway(t *testing.T) {
	now := time.Date(2026, time.November, 28, 15, 0, 0, 0, time.UTC)

	t.Run("full 3-month runway across year boundary", func(t *testing.T) {
		existing := []string{
			"alerts_default",
			"alerts_2026_10",
			"alerts_2026_11",
			"alerts_2026_12",
			"alerts_2027_01",
		}
		assert.Equal(t, 3, ComputePartitionRunway(existing, now))
	})

	t.Run("missing current month returns zero even if future months exist", func(t *testing.T) {
		existing := []string{"alerts_default", "alerts_2026_12", "alerts_2027_01"}
		assert.Equal(t, 0, ComputePartitionRunway(existing, now))
	})

	t.Run("gap in future months stops runway at first missing month", func(t *testing.T) {
		existing := []string{
			"alerts_2026_11",
			"alerts_2026_12",
			// alerts_2027_01 is missing
			"alerts_2027_02",
		}
		assert.Equal(t, 2, ComputePartitionRunway(existing, now))
	})
}

func TestEffectiveRetentionCutoffAndExpiredPartitions(t *testing.T) {
	now := time.Date(2026, time.June, 15, 12, 0, 0, 0, time.UTC)

	t.Run("unset retention disables partition drops", func(t *testing.T) {
		cutoff, ok := EffectiveRetentionCutoff(now, 0, nil)
		assert.False(t, ok)
		assert.Zero(t, cutoff)
	})

	t.Run("longest per-workspace retention wins over shorter global retention", func(t *testing.T) {
		global := 30 * 24 * time.Hour
		workspaces := map[string]time.Duration{
			"ws-short": 14 * 24 * time.Hour,
			"ws-long":  90 * 24 * time.Hour,
		}
		cutoff, ok := EffectiveRetentionCutoff(now, global, workspaces)
		require.True(t, ok)
		assert.Equal(t, now.Add(-90*24*time.Hour), cutoff)

		existing := []string{
			"alerts_default",
			"alerts_2026_02", // [2026-02-01, 2026-03-01) -> ends before 2026-03-17 cutoff -> expired
			"alerts_2026_03", // [2026-03-01, 2026-04-01) -> ends after 2026-03-17 cutoff -> retained!
			"alerts_2026_04",
			"alerts_2026_05",
			"alerts_2026_06",
		}
		drops := ExpiredAlertPartitions(existing, cutoff)
		require.Len(t, drops, 1)
		assert.Equal(t, "alerts_2026_02", drops[0].Name)
	})

	t.Run("workspace with indefinite retention prevents dropping shared partitions", func(t *testing.T) {
		cutoff, ok := EffectiveRetentionCutoff(now, 30*24*time.Hour, map[string]time.Duration{
			"ws-regulated": 0,
		})
		assert.False(t, ok)
		assert.Zero(t, cutoff)
	})

	t.Run("exact boundary at partition end is eligible while 1ns inside is retained", func(t *testing.T) {
		existing := []string{"alerts_2026_04", "alerts_2026_05"}
		exactEnd := time.Date(2026, time.May, 1, 0, 0, 0, 0, time.UTC)

		dropsAtBoundary := ExpiredAlertPartitions(existing, exactEnd)
		require.Len(t, dropsAtBoundary, 1)
		assert.Equal(t, "alerts_2026_04", dropsAtBoundary[0].Name)

		dropsBeforeBoundary := ExpiredAlertPartitions(existing, exactEnd.Add(-time.Nanosecond))
		assert.Empty(t, dropsBeforeBoundary)
	})
}

type fakeLifecycleStore struct {
	mu           sync.Mutex
	ensureCalls  int
	checkCalls   int
	dropCalls    int
	ensureErr    error
	checkErr     error
	dropErr      error
	droppedRows  int64
	lastMonths   int
	lastCutoff   time.Time
}

func (f *fakeLifecycleStore) EnsureAlertPartitions(_ context.Context, _ time.Time, months int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureCalls++
	f.lastMonths = months
	return f.ensureErr
}

func (f *fakeLifecycleStore) CheckAlertPartitionRunway(_ context.Context, _ time.Time, months int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkCalls++
	f.lastMonths = months
	return f.checkErr
}

func (f *fakeLifecycleStore) DropExpiredPartitions(_ context.Context, cutoff time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropCalls++
	f.lastCutoff = cutoff
	if f.dropErr != nil {
		return 0, f.dropErr
	}
	return f.droppedRows, nil
}

func scrapeMetricsText(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	return rec.Body.String()
}

func TestMaintainAlertPartitionsOnce_DryRunYieldAndFailureMetrics(t *testing.T) {
	now := time.Date(2026, time.October, 31, 23, 0, 0, 0, time.UTC)

	t.Run("dry-run checks runway without creating or dropping partitions", func(t *testing.T) {
		st := &fakeLifecycleStore{droppedRows: 42}
		m := metrics.New()
		plan, err := MaintainAlertPartitionsOnce(context.Background(), st, PartitionLifecycleOptions{
			Now:          now,
			RunwayMonths: 3,
			Retention:    60 * 24 * time.Hour,
			DryRun:       true,
			Metrics:      m,
		})
		require.NoError(t, err)
		require.NotNil(t, plan)
		assert.True(t, plan.DryRun)
		assert.Equal(t, 0, st.ensureCalls, "dry-run must not create partitions")
		assert.Equal(t, 1, st.checkCalls, "dry-run still inspects runway")
		assert.Equal(t, 0, st.dropCalls, "dry-run must not drop partitions")
	})

	t.Run("ShouldYield defers partition drops while still ensuring future partitions", func(t *testing.T) {
		st := &fakeLifecycleStore{droppedRows: 10}
		m := metrics.New()
		plan, err := MaintainAlertPartitionsOnce(context.Background(), st, PartitionLifecycleOptions{
			Now:          now,
			RunwayMonths: 4,
			Retention:    60 * 24 * time.Hour,
			ShouldYield:  func(context.Context) bool { return true },
			Metrics:      m,
		})
		require.NoError(t, err)
		require.NotNil(t, plan)
		assert.True(t, plan.YieldedDrops)
		assert.Equal(t, 1, st.ensureCalls)
		assert.Equal(t, 4, st.lastMonths)
		assert.Equal(t, 0, st.dropCalls, "drops must yield when ingestion load is high")

		body := scrapeMetricsText(t, m)
		assert.Contains(t, body, "sorobeacon_alert_partition_runway_months 4")
	})

	t.Run("creation failure logs loud error and increments error metric", func(t *testing.T) {
		st := &fakeLifecycleStore{ensureErr: errors.New("lock timeout exceeded")}
		m := metrics.New()
		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, nil))

		_, err := MaintainAlertPartitionsOnce(context.Background(), st, PartitionLifecycleOptions{
			Now:          now,
			RunwayMonths: 3,
			Metrics:      m,
			Log:          log,
		})
		require.Error(t, err)
		assert.Contains(t, buf.String(), "level=ERROR")
		assert.Contains(t, buf.String(), "alert partition maintenance failed")

		body := scrapeMetricsText(t, m)
		assert.Contains(t, body, `sorobeacon_alert_partition_errors_total{operation="create"} 1`)
	})

	t.Run("runway check failure logs loud error and increments runway error metric", func(t *testing.T) {
		st := &fakeLifecycleStore{checkErr: errors.New("missing alert partitions: alerts_2026_12")}
		m := metrics.New()
		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, nil))

		_, err := MaintainAlertPartitionsOnce(context.Background(), st, PartitionLifecycleOptions{
			Now:          now,
			RunwayMonths: 3,
			Metrics:      m,
			Log:          log,
		})
		require.Error(t, err)
		assert.Contains(t, buf.String(), "level=ERROR")

		body := scrapeMetricsText(t, m)
		assert.Contains(t, body, `sorobeacon_alert_partition_errors_total{operation="runway"} 1`)
	})
}

func TestRunAlertPartitionLifecycle_PeriodicExecution(t *testing.T) {
	st := &fakeLifecycleStore{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunAlertPartitionLifecycle(ctx, st, PartitionLifecycleOptions{
			RunwayMonths:  3,
			CheckInterval: 10 * time.Millisecond,
			Log:           slog.New(slog.DiscardHandler),
		})
	}()

	require.Eventually(t, func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.ensureCalls >= 2
	}, time.Second, 5*time.Millisecond)
	cancel()
	<-done
}

func TestPostgresMaintainAlertPartitionsDryRunAndAuditLog(t *testing.T) {
	p := newPartitionStore(t)
	ctx := context.Background()

	const (
		expiredPart = "alerts_2001_02"
		future1     = "alerts_2098_05"
		future2     = "alerts_2098_06"
		future3     = "alerts_2098_07"
	)
	for _, name := range []string{expiredPart, future1, future2, future3} {
		dropPartition(t, p, name)
		name := name
		t.Cleanup(func() { dropPartition(t, p, name) })
	}

	require.NoError(t, p.EnsureAlertPartitions(ctx, time.Date(2001, time.February, 1, 0, 0, 0, 0, time.UTC), 1))

	m := &Monitor{Name: "lifecycle-mon", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, p.CreateMonitor(ctx, m))
	r := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, p.CreateRule(ctx, r))
	a := &Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "ev-lifecycle-2001"}
	_, err := p.CreateAlert(ctx, a)
	require.NoError(t, err)
	require.NoError(t, p.setAlertCreatedAt(ctx, a.ID, time.Date(2001, time.February, 10, 12, 0, 0, 0, time.UTC)))

	now := time.Date(2098, time.May, 20, 0, 0, 0, 0, time.UTC)
	retention := 90 * 365 * 24 * time.Hour // cutoff ~2008, so 2001-02 is expired while recent partitions are not

	// 1. Dry-run reports planned creations and drops without mutating partitions or audit_log.
	dryPlan, err := p.MaintainAlertPartitions(ctx, PartitionLifecycleOptions{
		Now:          now,
		RunwayMonths: 3,
		Retention:    retention,
		DryRun:       true,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{future1, future2, future3}, dryPlan.ToCreate)
	assert.Empty(t, dryPlan.Created)
	assert.NotEmpty(t, dryPlan.ToDrop)

	namesAfterDryRun, err := p.alertPartitions(ctx)
	require.NoError(t, err)
	assert.Contains(t, namesAfterDryRun, expiredPart, "dry-run must not drop expired partition")
	assert.NotContains(t, namesAfterDryRun, future1, "dry-run must not create future partition")

	// 2. Live execution creates future runway, drops expired partition, and records audit_log entry.
	liveMetrics := metrics.New()
	livePlan, err := p.MaintainAlertPartitions(ctx, PartitionLifecycleOptions{
		Now:          now,
		RunwayMonths: 3,
		Retention:    retention,
		DryRun:       false,
		Actor:        "test:partition_lifecycle",
		Metrics:      liveMetrics,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{future1, future2, future3}, livePlan.Created)
	assert.Equal(t, 3, livePlan.RunwayMonths)
	assert.GreaterOrEqual(t, livePlan.AlertsDropped, int64(1))

	namesAfterLive, err := p.alertPartitions(ctx)
	require.NoError(t, err)
	assert.NotContains(t, namesAfterLive, expiredPart)
	assert.Contains(t, namesAfterLive, future1)
	assert.Contains(t, namesAfterLive, future2)
	assert.Contains(t, namesAfterLive, future3)

	entries, err := p.ListAuditEntries(ctx, AuditFilter{
		TargetType: AuditTargetAlertPartition,
		TargetID:   200102,
		Actor:      "test:partition_lifecycle",
	})
	require.NoError(t, err)
	require.NotEmpty(t, entries, "dropping an expired partition must write an audit_log entry")
	assert.Equal(t, AuditActionDelete, entries[0].Action)
	assert.Contains(t, string(entries[0].Diff), expiredPart)
}
