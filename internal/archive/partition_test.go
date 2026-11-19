package archive_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/archive"
	"github.com/sorotrail/sorobeacon/internal/archive/archivetest"
	"github.com/sorotrail/sorobeacon/internal/store"
)

var march = store.PartitionDropRecord{
	Name: "alerts_2026_03",
	From: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
	To:   time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
}

func wa(ws string, id int64, day int) store.WorkspaceAlert {
	return store.WorkspaceAlert{WorkspaceID: ws, Alert: store.Alert{
		ID: id, MonitorID: 1, RuleID: 2, EventID: fmt.Sprintf("ev-%d", id),
		CreatedAt: time.Date(2026, 3, day, 12, 0, 0, 0, time.UTC),
		Payload:   json.RawMessage(fmt.Sprintf(`{"contract_id":"C%d"}`, id%2)),
	}}
}

func seeded(t *testing.T, chunkRows int) (*archive.Store, *archivetest.Fake) {
	t.Helper()
	fake := archivetest.New()
	st, err := archive.NewStore(fake)
	require.NoError(t, err)
	st.WithChunkRows(chunkRows).WithClock(func() time.Time { return time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC) })
	var all []store.WorkspaceAlert
	for i := int64(1); i <= 6; i++ {
		all = append(all, wa("acme", i, int(i)))
		all = append(all, wa("beta", 100+i, int(i)))
	}
	n, err := st.ArchivePartition(context.Background(), march, all)
	require.NoError(t, err)
	require.Equal(t, int64(12), n)
	return st, fake
}

func ids(as []store.Alert) []int64 {
	out := make([]int64, 0, len(as))
	for _, a := range as {
		out = append(out, a.ID)
	}
	return out
}

func TestArchivePartitionWritesManifestAndPerWorkspaceChunks(t *testing.T) {
	_, fake := seeded(t, 4)
	assert.Equal(t, []string{"manifests/2026-03.json"}, fake.Keys("manifests/"))
	// 6 alerts per workspace in chunks of 4 is 2 chunks each.
	assert.Len(t, fake.Keys("partitions/2026-03/"), 4)
}

func TestArchivePartitionFailsWhenBackendCorruptsTheCopy(t *testing.T) {
	fake := archivetest.New()
	fake.MutatePut = func(key string, b []byte) []byte {
		if len(b) > 10 && key != archive.ManifestKey("2026-03") {
			return b[:len(b)-5]
		}
		return b
	}
	st, err := archive.NewStore(fake)
	require.NoError(t, err)
	n, err := st.ArchivePartition(context.Background(), march, []store.WorkspaceAlert{wa("acme", 1, 1)})
	require.Error(t, err, "a copy that does not verify must not be reported as durable")
	assert.Zero(t, n)
	var ce *store.ArchiveChunkError
	require.ErrorAs(t, err, &ce)
	assert.Contains(t, ce.Key, "partitions/2026-03/")
}

func TestListArchivedPagesWithoutRepeatOrSkipInBothDirections(t *testing.T) {
	st, _ := seeded(t, 2)
	for _, tc := range []struct {
		sort string
		want []int64
	}{
		{"created_at_desc", []int64{6, 5, 4, 3, 2, 1}},
		{"created_at_asc", []int64{1, 2, 3, 4, 5, 6}},
	} {
		var got []int64
		var after int64
		for i := 0; i < 10; i++ {
			res, err := st.ListArchivedAlerts(context.Background(), "acme", store.ArchiveQuery{
				Filter: store.AlertFilter{Sort: tc.sort, Limit: 4, AfterID: after},
			})
			require.NoError(t, err)
			if len(res.Alerts) == 0 {
				break
			}
			if after != 0 {
				assert.True(t, res.CursorFound)
			}
			for _, a := range res.Alerts {
				assert.True(t, a.Archived)
			}
			got = append(got, ids(res.Alerts)...)
			after = res.Alerts[len(res.Alerts)-1].ID
		}
		assert.Equal(t, tc.want, got, tc.sort)
	}
}

func TestListArchivedNeverLeaksAcrossWorkspaces(t *testing.T) {
	st, _ := seeded(t, 100) // one chunk per workspace, same month
	res, err := st.ListArchivedAlerts(context.Background(), "acme", store.ArchiveQuery{Filter: store.AlertFilter{Limit: 500}})
	require.NoError(t, err)
	assert.Equal(t, []int64{6, 5, 4, 3, 2, 1}, ids(res.Alerts))

	other, err := st.ListArchivedAlerts(context.Background(), "gamma", store.ArchiveQuery{Filter: store.AlertFilter{Limit: 500}})
	require.NoError(t, err)
	assert.Empty(t, other.Alerts)

	// A cursor naming another workspace's alert is not found here.
	res, err = st.ListArchivedAlerts(context.Background(), "acme", store.ArchiveQuery{Filter: store.AlertFilter{AfterID: 103}})
	require.NoError(t, err)
	assert.False(t, res.CursorFound)
	assert.Empty(t, res.Alerts)
}

func TestListArchivedChunkSwappedAcrossWorkspacesIsAnError(t *testing.T) {
	st, fake := seeded(t, 100)
	keys := fake.Keys("partitions/2026-03/")
	require.Len(t, keys, 2)
	// Overwrite one workspace's chunk with the other's bytes.
	rc, _ := fake.Get(context.Background(), keys[1])
	body, err := io.ReadAll(rc)
	require.NoError(t, err)
	fake.Put(keys[0], body)
	_, err = st.ListArchivedAlerts(context.Background(), "acme", store.ArchiveQuery{Filter: store.AlertFilter{Limit: 10}})
	_, errB := st.ListArchivedAlerts(context.Background(), "beta", store.ArchiveQuery{Filter: store.AlertFilter{Limit: 10}})
	require.True(t, err != nil || errB != nil, "a swapped chunk must be detected")
}

func TestListArchivedMissingChunkNamesIt(t *testing.T) {
	st, fake := seeded(t, 100)
	keys := fake.Keys("partitions/2026-03/")
	fake.Delete(keys[0])
	fake.Delete(keys[1])
	_, err := st.ListArchivedAlerts(context.Background(), "acme", store.ArchiveQuery{Filter: store.AlertFilter{Limit: 10}})
	var ce *store.ArchiveChunkError
	require.ErrorAs(t, err, &ce)
	assert.Contains(t, ce.Key, "partitions/2026-03/")
	assert.Equal(t, "missing", ce.Reason)
	assert.Contains(t, err.Error(), ce.Key)
}

func TestListArchivedCorruptChunkNamesIt(t *testing.T) {
	st, fake := seeded(t, 100)
	for _, k := range fake.Keys("partitions/2026-03/") {
		fake.Put(k, []byte("not the original bytes\n"))
	}
	_, err := st.ListArchivedAlerts(context.Background(), "acme", store.ArchiveQuery{Filter: store.AlertFilter{Limit: 10}})
	var ce *store.ArchiveChunkError
	require.ErrorAs(t, err, &ce)
	assert.Contains(t, ce.Reason, "corrupt")
}

func TestListArchivedCorruptManifestNamesIt(t *testing.T) {
	st, fake := seeded(t, 100)
	fake.Put(archive.ManifestKey("2026-03"), []byte("{nope"))
	_, err := st.ListArchivedAlerts(context.Background(), "acme", store.ArchiveQuery{Filter: store.AlertFilter{Limit: 10}})
	var ce *store.ArchiveChunkError
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, archive.ManifestKey("2026-03"), ce.Key)
}

func TestListArchivedHonoursTheFetchCap(t *testing.T) {
	st, _ := seeded(t, 1) // one alert per chunk
	res, err := st.ListArchivedAlerts(context.Background(), "acme", store.ArchiveQuery{
		Filter: store.AlertFilter{Limit: 500}, MaxChunks: 2,
	})
	require.NoError(t, err)
	assert.True(t, res.Capped, "stopping at the cap must be reported, not silent")
	assert.Equal(t, []int64{6, 5}, ids(res.Alerts))
	assert.Equal(t, 2, res.ChunksFetched)
}

func TestListArchivedFilters(t *testing.T) {
	st, _ := seeded(t, 100)
	res, err := st.ListArchivedAlerts(context.Background(), "acme", store.ArchiveQuery{Filter: store.AlertFilter{
		Limit: 500, ContractID: "C0",
		From: time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 3, 6, 0, 0, 0, 0, time.UTC),
	}})
	require.NoError(t, err)
	assert.Equal(t, []int64{4, 2}, ids(res.Alerts))
}

func TestMonthWithoutManifestIsEmptyNotAnError(t *testing.T) {
	st, err := archive.NewStore(archivetest.New())
	require.NoError(t, err)
	st.WithClock(func() time.Time { return march.To })
	res, err := st.ListArchivedAlerts(context.Background(), "acme", store.ArchiveQuery{Filter: store.AlertFilter{Limit: 5}})
	require.NoError(t, err)
	assert.Empty(t, res.Alerts)
}
