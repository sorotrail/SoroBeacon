package graphql

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultPersistedOperationsUniqueness(t *testing.T) {
	ops := DefaultPersistedOperations()
	require.NotEmpty(t, ops)

	seenIDs := make(map[string]struct{}, len(ops))
	seenNames := make(map[string]struct{}, len(ops))

	for _, op := range ops {
		t.Run(op.Name, func(t *testing.T) {
			_, dupID := seenIDs[op.ID]
			assert.False(t, dupID, "duplicate operation ID %q", op.ID)
			seenIDs[op.ID] = struct{}{}

			_, dupName := seenNames[op.Name]
			assert.False(t, dupName, "duplicate operation name %q", op.Name)
			seenNames[op.Name] = struct{}{}
		})
	}
}
