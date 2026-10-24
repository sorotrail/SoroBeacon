package store

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNormalizeTags pins the normalisation contract the API, dashboard and
// both store backends share: trim, lowercase, deduplicate, and drop empty
// values plus anything that would break the SQLite quoted-text tag filter
// (double quotes and control characters). Runs without a database.
func TestNormalizeTags(t *testing.T) {
	got := NormalizeTags([]string{" Treasury ", "treasury", "TREASURY", "", "  ", "mainnet", `we"ird`, "ops\nrow"})
	assert.Equal(t, []string{"treasury", "mainnet", "weird", "opsrow"}, got)

	// Never nil, so JSON output is always an array and the NOT NULL column
	// never sees a NULL.
	assert.NotNil(t, NormalizeTags(nil))
	assert.Equal(t, []string{}, NormalizeTags(nil))
	assert.Equal(t, []string{}, NormalizeTags([]string{"", " "}))
}

// names returns the sorted monitor names in a page, so assertions do not
// depend on the listing's sort order.
func names(list []Monitor) []string {
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, m.Name)
	}
	sort.Strings(out)
	return out
}

// testMonitorTags covers the tag contract end to end: normalisation at the
// write, containment filtering at the read, an untagged monitor behaving
// exactly as before, updates replacing the set, and duplication carrying
// tags into the copy. It runs against both backends, so the Postgres @>
// containment clause and the SQLite instr match are held to identical
// results.
func testMonitorTags(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	tagged := &Monitor{Name: "tagged", ContractIDs: []string{"C1"}, Enabled: true,
		Tags: []string{"Treasury", " treasury ", "MAINNET"}}
	require.NoError(t, st.CreateMonitor(ctx, tagged))
	// Normalised on write: trimmed, lowercased, deduplicated, order kept.
	require.Equal(t, []string{"treasury", "mainnet"}, tagged.Tags)

	both := &Monitor{Name: "both", ContractIDs: []string{"C2"}, Enabled: true,
		Tags: []string{"treasury", "ops"}}
	require.NoError(t, st.CreateMonitor(ctx, both))

	untagged := &Monitor{Name: "untagged", ContractIDs: []string{"C3"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, untagged))

	// A monitor with no tags reads back as an explicit empty list, not nil.
	got, err := st.GetMonitor(ctx, untagged.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Tags)
	assert.Empty(t, got.Tags)

	// Single tag: carries = match, so the untagged monitor is excluded.
	list, err := st.ListMonitorsPage(ctx, ListFilter{Tags: []string{"treasury"}, Limit: 50})
	require.NoError(t, err)
	assert.Equal(t, []string{"both", "tagged"}, names(list))

	// Repeated tags AND: only the monitor carrying both matches.
	list, err = st.ListMonitorsPage(ctx, ListFilter{Tags: []string{"treasury", "ops"}, Limit: 50})
	require.NoError(t, err)
	assert.Equal(t, []string{"both"}, names(list))

	// Filter values are normalised the same way writes are.
	list, err = st.ListMonitorsPage(ctx, ListFilter{Tags: []string{" Ops "}, Limit: 50})
	require.NoError(t, err)
	assert.Equal(t, []string{"both"}, names(list))

	// A tag nobody carries.
	list, err = st.ListMonitorsPage(ctx, ListFilter{Tags: []string{"missing"}, Limit: 50})
	require.NoError(t, err)
	assert.Empty(t, list)

	// No tag filter lists everything, untagged monitors included.
	list, err = st.ListMonitorsPage(ctx, ListFilter{Limit: 50})
	require.NoError(t, err)
	assert.Equal(t, []string{"both", "tagged", "untagged"}, names(list))

	// An update replaces the set, normalised again on write.
	tagged.Tags = []string{"Ops"}
	require.NoError(t, st.UpdateMonitor(ctx, tagged))
	got, err = st.GetMonitor(ctx, tagged.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"ops"}, got.Tags)
	list, err = st.ListMonitorsPage(ctx, ListFilter{Tags: []string{"treasury"}, Limit: 50})
	require.NoError(t, err)
	assert.Equal(t, []string{"both"}, names(list))

	// A duplicate belongs in the same groups as its source.
	dup, err := st.DuplicateMonitor(ctx, both.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"treasury", "ops"}, dup.Tags)
}

// testRetentionGaps covers the retention-gap record: nothing recorded means
// ErrNotFound and no gap in stats, each detection is persisted with its
// range and time, and GET /stats's store read surfaces the most recent one.
func testRetentionGaps(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	// Nothing recorded: the healthy case is ErrNotFound, not an error body.
	_, err := st.LatestRetentionGap(ctx, "")
	assert.ErrorIs(t, err, ErrNotFound)
	stats, err := st.GetStats(ctx)
	require.NoError(t, err)
	assert.Nil(t, stats.RetentionGap, "a healthy instance reports no retention_gap")

	first := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	require.NoError(t, st.RecordRetentionGap(ctx, "testnet", 100, 199, first))
	require.NoError(t, st.RecordRetentionGap(ctx, "mainnet", 300, 350, first.Add(24*time.Hour)))
	require.NoError(t, st.RecordRetentionGap(ctx, "testnet", 400, 450, first.Add(48*time.Hour)))

	// Most recent across every network.
	got, err := st.LatestRetentionGap(ctx, "")
	require.NoError(t, err)
	assert.Equal(t, "testnet", got.Network)
	assert.Equal(t, uint32(400), got.FromLedger)
	assert.Equal(t, uint32(450), got.ToLedger)
	assert.Equal(t, first.Add(48*time.Hour), got.DetectedAt.UTC())

	// Most recent for one network.
	got, err = st.LatestRetentionGap(ctx, "mainnet")
	require.NoError(t, err)
	assert.Equal(t, uint32(300), got.FromLedger)

	// A network with no gap: ErrNotFound, not a zero-valued record.
	_, err = st.LatestRetentionGap(ctx, "futurenet")
	assert.ErrorIs(t, err, ErrNotFound)

	// The stats read surfaces the most recent gap for the dashboard.
	stats, err = st.GetStats(ctx)
	require.NoError(t, err)
	require.NotNil(t, stats.RetentionGap)
	assert.Equal(t, "testnet", stats.RetentionGap.Network)
	assert.Equal(t, uint32(400), stats.RetentionGap.FromLedger)
	assert.Equal(t, uint32(450), stats.RetentionGap.ToLedger)
}

// testEventArchive covers the replay corpus: batched idempotent writes
// under re-reads, the filters replay scopes a range with, oldest-first
// keyset paging, and bounded retention deleting only what is older than the
// cutoff.
func testEventArchive(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	// An empty write is a no-op.
	require.NoError(t, st.RecordEvents(ctx, "testnet", nil))

	mk := func(id string, ledger uint32, contract string) ArchivedEvent {
		topics, err := json.Marshal([]any{"transfer"})
		require.NoError(t, err)
		return ArchivedEvent{
			EventID: id, ContractID: contract, Ledger: ledger,
			EventName: "transfer", Topics: topics,
		}
	}
	page := []ArchivedEvent{mk("e1", 10, "CA"), mk("e2", 20, "CA"), mk("e3", 30, "CB")}
	require.NoError(t, st.RecordEvents(ctx, "testnet", page))

	// Re-reading the same page (a crash resume or confirmation-depth hold)
	// must not duplicate the corpus.
	require.NoError(t, st.RecordEvents(ctx, "testnet", page))

	count, err := st.CountArchivedEvents(ctx, EventArchiveFilter{})
	require.NoError(t, err)
	assert.EqualValues(t, 3, count)

	// Ledger range, inclusive at both ends.
	count, err = st.CountArchivedEvents(ctx, EventArchiveFilter{FromLedger: 10, ToLedger: 20})
	require.NoError(t, err)
	assert.EqualValues(t, 2, count)

	// Contract scoping: replay asks for one monitor's contracts.
	list, err := st.ListArchivedEvents(ctx, EventArchiveFilter{ContractIDs: []string{"CB"}})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "e3", list[0].EventID)
	var topics []any
	require.NoError(t, json.Unmarshal(list[0].Topics, &topics))
	assert.Equal(t, []any{"transfer"}, topics)

	// Oldest-first, so a replay walks forward through history.
	list, err = st.ListArchivedEvents(ctx, EventArchiveFilter{Limit: 2})
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "e1", list[0].EventID)
	assert.Equal(t, "e2", list[1].EventID)

	// The id keyset pages forward without repeating rows.
	list, err = st.ListArchivedEvents(ctx, EventArchiveFilter{AfterID: list[1].ID})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "e3", list[0].EventID)

	// Another chain's events are a different corpus.
	count, err = st.CountArchivedEvents(ctx, EventArchiveFilter{Network: "mainnet"})
	require.NoError(t, err)
	assert.Zero(t, count)

	// Bounded retention: past the cutoff goes, recent rows stay.
	require.NoError(t, st.RecordEvents(ctx, "testnet", []ArchivedEvent{mk("old", 5, "CA")}))
	require.NoError(t, st.setEventArchiveIngestedAt(ctx, "old", time.Now().Add(-72*time.Hour)))
	deleted, err := st.DeleteExpiredEvents(ctx, time.Now().Add(-24*time.Hour), 100)
	require.NoError(t, err)
	assert.EqualValues(t, 1, deleted)
	count, err = st.CountArchivedEvents(ctx, EventArchiveFilter{})
	require.NoError(t, err)
	assert.EqualValues(t, 3, count)
}
