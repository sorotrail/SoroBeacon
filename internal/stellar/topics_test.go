package stellar

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSymbolTopicScVal pins the encoding against the example in the
// getEvents reference: the symbol "transfer" is the base64 ScVal
// AAAADwAAAAh0cmFuc2Zlcg==. A plain "transfer" in a topic filter matches
// nothing, so this byte-for-byte match is what makes filters work.
func TestSymbolTopicScVal(t *testing.T) {
	got, err := SymbolTopicScVal("transfer")
	require.NoError(t, err)
	assert.Equal(t, "AAAADwAAAAh0cmFuc2Zlcg==", got)
}

func TestSymbolTopicFilterTrailsWithWildcard(t *testing.T) {
	filter, err := SymbolTopicFilter("mint")
	require.NoError(t, err)
	require.Len(t, filter, 2)
	assert.Equal(t, TopicWildcardMany, filter[1], "the trailing ** keeps events with extra topics matching")
}
