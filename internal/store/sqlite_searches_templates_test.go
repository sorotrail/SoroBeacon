package store

// Saved searches and monitor templates reached the Store interface in a
// different change from the SQLite backend, so *SQLite had to grow both the
// methods and the schema to satisfy it. These tests pin the SQLite side of
// that contract: migrations 0007/0008 apply, values round-trip to the same Go
// shapes the Postgres backend returns, and the single-default invariant and
// ErrNotFound behaviour match it exactly.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSQLiteSavedSearches(t *testing.T) {
	st := newTestSQLite(t)
	ctx := context.Background()

	first := &SavedSearch{Name: "errors",
		Filter:    SavedSearchFilter{MonitorID: 7, Sort: "created_at_desc"},
		IsDefault: true}
	require.NoError(t, st.CreateSavedSearch(ctx, first))
	require.NotZero(t, first.ID)
	require.False(t, first.CreatedAt.IsZero())

	second := &SavedSearch{Name: "audit", Filter: SavedSearchFilter{ContractID: "CCONTRACT"}}
	require.NoError(t, st.CreateSavedSearch(ctx, second))

	got, err := st.GetSavedSearch(ctx, first.ID)
	require.NoError(t, err)
	assert.Equal(t, "errors", got.Name)
	assert.True(t, got.IsDefault, "the default row must stay default")
	assert.Equal(t, SavedSearchFilter{MonitorID: 7, Sort: "created_at_desc"}, got.Filter,
		"the filter must round-trip through the TEXT column untouched")

	list, err := st.ListSavedSearches(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "audit", list[0].Name, "listing is ordered by name")
	assert.Equal(t, "errors", list[1].Name)

	// Exactly one default at a time: promoting a second row demotes the first,
	// which the partial unique index is what makes enforceable.
	require.NoError(t, st.SetDefaultSearch(ctx, second.ID))
	got, err = st.GetSavedSearch(ctx, second.ID)
	require.NoError(t, err)
	assert.True(t, got.IsDefault)
	got, err = st.GetSavedSearch(ctx, first.ID)
	require.NoError(t, err)
	assert.False(t, got.IsDefault, "promoting one row must demote the other")

	require.NoError(t, st.ClearDefaultSearch(ctx, second.ID))
	got, err = st.GetSavedSearch(ctx, second.ID)
	require.NoError(t, err)
	assert.False(t, got.IsDefault)

	assert.ErrorIs(t, st.SetDefaultSearch(ctx, 99999), ErrNotFound)
	assert.ErrorIs(t, st.ClearDefaultSearch(ctx, 99999), ErrNotFound)
	_, err = st.GetSavedSearch(ctx, 99999)
	assert.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, st.DeleteSavedSearch(ctx, first.ID))
	assert.ErrorIs(t, st.DeleteSavedSearch(ctx, first.ID), ErrNotFound,
		"a second delete must report the row as missing")
}

func TestSQLiteMonitorTemplates(t *testing.T) {
	st := newTestSQLite(t)
	ctx := context.Background()

	tpl := &MonitorTemplate{
		Name:        "token watcher",
		Description: "watch a SEP-41 token",
		Rules: []MonitorTemplateRule{
			{Type: "token_event", Params: json.RawMessage(`{"event":"transfer"}`)},
		},
		ChannelIDs: []int64{2, 5},
		Parameters: []TemplateParameter{{Name: "holder", Required: true}},
	}
	require.NoError(t, st.CreateMonitorTemplate(ctx, tpl))
	require.NotZero(t, tpl.ID)
	require.False(t, tpl.CreatedAt.IsZero())

	got, err := st.GetMonitorTemplate(ctx, tpl.ID)
	require.NoError(t, err)
	assert.Equal(t, "token watcher", got.Name)
	assert.Equal(t, "watch a SEP-41 token", got.Description)
	require.Len(t, got.Rules, 1)
	assert.Equal(t, "token_event", got.Rules[0].Type)
	assert.JSONEq(t, `{"event":"transfer"}`, string(got.Rules[0].Params))
	assert.Equal(t, []int64{2, 5}, got.ChannelIDs,
		"channel ids must survive the TEXT column that replaced BIGINT[]")
	require.Len(t, got.Parameters, 1)
	assert.Equal(t, "holder", got.Parameters[0].Name)
	assert.True(t, got.Parameters[0].Required)

	got.Description = "renamed"
	got.Rules = nil
	got.ChannelIDs = nil
	require.NoError(t, st.UpdateMonitorTemplate(ctx, got))
	again, err := st.GetMonitorTemplate(ctx, tpl.ID)
	require.NoError(t, err)
	assert.Equal(t, "renamed", again.Description)
	assert.Empty(t, again.Rules)
	assert.NotNil(t, again.ChannelIDs, "an emptied list must come back as a list, not nil")

	list, err := st.ListMonitorTemplates(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "token watcher", list[0].Name)

	assert.ErrorIs(t, st.UpdateMonitorTemplate(ctx, &MonitorTemplate{ID: 99999}), ErrNotFound)
	_, err = st.GetMonitorTemplate(ctx, 99999)
	assert.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, st.DeleteMonitorTemplate(ctx, tpl.ID))
	assert.ErrorIs(t, st.DeleteMonitorTemplate(ctx, tpl.ID), ErrNotFound)
}
