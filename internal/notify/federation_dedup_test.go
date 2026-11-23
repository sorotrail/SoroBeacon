package notify

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A federated alert has no local rule, so the channels that key on the
// alert's identity need the instance it came from as well: two peers
// forwarding the same event id must not share one incident.
func TestDedupKeySeparatesFederatedPeers(t *testing.T) {
	local := Alert{RuleID: 3, EventID: "ev-1"}
	assert.Equal(t, "3:ev-1", dedupKey(local))

	fromA := Alert{RuleID: 0, EventID: "ev-1", FederationOrigin: "peer-a"}
	fromB := Alert{RuleID: 0, EventID: "ev-1", FederationOrigin: "peer-b"}
	assert.NotEqual(t, dedupKey(fromA), dedupKey(fromB),
		"two peers forwarding the same event must not share a dedup key")
	assert.NotEqual(t, dedupKey(fromA), dedupKey(local),
		"a forwarded alert must not share a dedup key with a local one")
	assert.Equal(t, dedupKey(fromA), dedupKey(fromA),
		"the same forwarded alert must reuse its key")
}

// The Opsgenie alias has the same property: it deduplicates repeated alerts
// for the same condition, so two peers must not collapse into one alert.
func TestOpsgenieAliasSeparatesFederatedPeers(t *testing.T) {
	o := &Opsgenie{}
	local := Alert{RuleID: 3, EventID: "ev-1"}
	assert.Equal(t, "sorobeacon-3-ev-1", o.alias(local))

	fromA := Alert{RuleID: 0, EventID: "ev-1", FederationOrigin: "peer-a"}
	fromB := Alert{RuleID: 0, EventID: "ev-1", FederationOrigin: "peer-b"}
	assert.NotEqual(t, o.alias(fromA), o.alias(fromB))
	assert.NotEqual(t, o.alias(fromA), o.alias(local))
}
