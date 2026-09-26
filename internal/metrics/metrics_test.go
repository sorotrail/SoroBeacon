package metrics

import (
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The store cannot import these names (they are its own unexported constants),
// so the test and the store agree on the label values by string. If either
// side renames one, these tests fail rather than the dashboard silently going
// blank.
const (
	poolNamePrimary = "primary"
	poolNameReplica = "replica"

	storeReadsName     = "sorobeacon_store_reads_total"
	storeFallbacksName = "sorobeacon_store_replica_fallbacks_total"
	replicaEnabledName = "sorobeacon_store_replica_enabled"
)

// TestRecordStoreReadLabelsByPool checks that a routed read is attributable:
// the pool label is what tells an operator whether routing is actually sending
// reads to the replica, and an unlabelled counter would make the metric
// useless for that one job.
func TestRecordStoreReadLabelsByPool(t *testing.T) {
	m := New()

	m.RecordStoreRead(poolNamePrimary)
	m.RecordStoreRead(poolNamePrimary)
	m.RecordStoreRead(poolNameReplica)

	assert.Equal(t, float64(2), m.sampleValue(t, storeReadsName, map[string]string{"pool": poolNamePrimary}))
	assert.Equal(t, float64(1), m.sampleValue(t, storeReadsName, map[string]string{"pool": poolNameReplica}))
}

func TestRecordReplicaFallbackCounts(t *testing.T) {
	m := New()

	m.RecordReplicaFallback()
	m.RecordReplicaFallback()
	m.RecordReplicaFallback()

	assert.Equal(t, float64(3), m.sampleValue(t, storeFallbacksName, nil))
}

// TestSetReplicaEnabledTracksBothDirections covers the gauge an operator reads
// to confirm the replica URL took effect, in both directions: it has to go
// back to 0 when routing is off, or a deployment that removed the replica
// would keep reporting one.
func TestSetReplicaEnabledTracksBothDirections(t *testing.T) {
	m := New()

	require.Equal(t, float64(0), m.sampleValue(t, replicaEnabledName, nil), "a fresh process reports no replica")

	m.SetReplicaEnabled(true)
	assert.Equal(t, float64(1), m.sampleValue(t, replicaEnabledName, nil))

	m.SetReplicaEnabled(false)
	assert.Equal(t, float64(0), m.sampleValue(t, replicaEnabledName, nil))
}

// TestReadMetricsAreNilSafe pins the package contract that a nil *Metrics
// records nothing rather than panicking. The store holds a *Metrics it was
// handed without a nil check, so this is load-bearing for every embedder and
// every test that constructs a store with no instrumentation.
func TestReadMetricsAreNilSafe(t *testing.T) {
	var m *Metrics

	assert.NotPanics(t, func() {
		m.RecordStoreRead(poolNamePrimary)
		m.RecordStoreRead(poolNameReplica)
		m.RecordReplicaFallback()
		m.SetReplicaEnabled(true)
		m.SetReplicaEnabled(false)
	})
}

// sampleValue returns the current value of the named metric, read out of the
// instance's own registry — what a scrape would see. It reads the gatherer
// directly rather than taking on prometheus/testutil (and the godebug module
// it pulls in) for a handful of numbers.
func (m *Metrics) sampleValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := m.registry.Gather()
	require.NoError(t, err)

	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, sample := range family.GetMetric() {
			if !hasLabels(sample, labels) {
				continue
			}
			if c := sample.GetCounter(); c != nil {
				return c.GetValue()
			}
			if g := sample.GetGauge(); g != nil {
				return g.GetValue()
			}
			t.Fatalf("%s is neither a counter nor a gauge", name)
		}
	}
	t.Fatalf("no sample for %s with labels %v", name, labels)
	return 0
}

// hasLabels reports whether sample carries exactly the wanted label pairs.
// Labels are compared as a set because prometheus does not promise an order.
func hasLabels(sample *dto.Metric, want map[string]string) bool {
	if len(sample.GetLabel()) != len(want) {
		return false
	}
	for _, pair := range sample.GetLabel() {
		if want[pair.GetName()] != pair.GetValue() {
			return false
		}
	}
	return true
}
