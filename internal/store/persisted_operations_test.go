package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testPersistedOperations(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	// 1. Initial lookup on non-existent operation returns ErrNotFound.
	_, err := st.GetPersistedOperation(ctx, "nonexistent-hash")
	assert.ErrorIs(t, err, ErrNotFound)

	// 2. Register operation.
	op := PersistedOperation{
		ID:        "a591a6d40bf420404a011733cfb7b190d62c65bf0bcda32b57b277d9ad9f146e",
		Name:      "DashboardStats",
		Document:  "{ stats { monitors alerts } }",
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}
	err = st.RegisterPersistedOperation(ctx, op)
	require.NoError(t, err)

	// 3. Retrieve registered operation.
	got, err := st.GetPersistedOperation(ctx, op.ID)
	require.NoError(t, err)
	assert.Equal(t, op.ID, got.ID)
	assert.Equal(t, op.Name, got.Name)
	assert.Equal(t, op.Document, got.Document)

	// 4. Upsert (idempotency / update).
	opUpdated := PersistedOperation{
		ID:        op.ID,
		Name:      "DashboardStatsRenamed",
		Document:  "{ stats { monitors alerts rules } }",
		CreatedAt: op.CreatedAt,
	}
	err = st.RegisterPersistedOperation(ctx, opUpdated)
	require.NoError(t, err)

	gotUpdated, err := st.GetPersistedOperation(ctx, op.ID)
	require.NoError(t, err)
	assert.Equal(t, "DashboardStatsRenamed", gotUpdated.Name)
	assert.Equal(t, "{ stats { monitors alerts rules } }", gotUpdated.Document)

	// 5. Register second operation and list.
	op2 := PersistedOperation{
		ID:        "b721a6d40bf420404a011733cfb7b190d62c65bf0bcda32b57b277d9ad9f9999",
		Name:      "DashboardMonitors",
		Document:  "{ monitors { edges { node { id name } } } }",
		CreatedAt: time.Now().UTC().Add(time.Minute).Truncate(time.Second),
	}
	err = st.RegisterPersistedOperation(ctx, op2)
	require.NoError(t, err)

	list, err := st.ListPersistedOperations(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 2)
	assert.Equal(t, op2.ID, list[0].ID, "most recently created operation comes first")

	// 6. Delete operation.
	err = st.DeletePersistedOperation(ctx, op.ID)
	require.NoError(t, err)

	_, err = st.GetPersistedOperation(ctx, op.ID)
	assert.ErrorIs(t, err, ErrNotFound)

	// Deleting again returns ErrNotFound.
	err = st.DeletePersistedOperation(ctx, op.ID)
	assert.ErrorIs(t, err, ErrNotFound)

	// Remaining operation still present.
	listAfter, err := st.ListPersistedOperations(ctx)
	require.NoError(t, err)
	assert.Len(t, listAfter, 1)
	assert.Equal(t, op2.ID, listAfter[0].ID)
}
