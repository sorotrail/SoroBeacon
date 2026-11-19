package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/archive"
	"github.com/sorotrail/sorobeacon/internal/archive/archivetest"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

const tierRetention = 30 * 24 * time.Hour

// tierDB is the database tier: an in-memory alert list that applies the same
// retention clamp, sort and keyset-cursor rules as store.Postgres.ListAlerts.
type tierDB struct {
	store.Store
	alerts []store.Alert
}

func (d *tierDB) ListAlerts(_ context.Context, f store.AlertFilter) ([]store.Alert, error) {
	if cutoff := time.Now().Add(-tierRetention); f.From.IsZero() || f.From.Before(cutoff) {
		f.From = cutoff
	}
	rows := append([]store.Alert(nil), d.alerts...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].CreatedAt.Before(rows[j].CreatedAt) })
	if f.Sort != "created_at_asc" {
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
	}
	if f.AfterID != 0 {
		idx := -1
		for i, a := range rows {
			if a.ID == f.AfterID {
				idx = i
			}
		}
		if idx < 0 {
			return nil, nil
		}
		rows = rows[idx+1:]
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	var out []store.Alert
	for _, a := range rows {
		if a.CreatedAt.Before(f.From) || (!f.To.IsZero() && !a.CreatedAt.Before(f.To)) {
			continue
		}
		out = append(out, a)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (d *tierDB) GetAlert(_ context.Context, id int64) (*store.Alert, error) {
	for i := range d.alerts {
		if d.alerts[i].ID == id {
			return &d.alerts[i], nil
		}
	}
	return nil, store.ErrNotFound
}

func (d *tierDB) ListAlertsStream(ctx context.Context, f store.AlertFilter, fn func(store.Alert) error) error {
	rows, _ := d.ListAlerts(ctx, f)
	for _, a := range rows {
		if err := fn(a); err != nil {
			return err
		}
	}
	return nil
}

func (d *tierDB) ListMonitors(context.Context, bool) ([]store.Monitor, error) { return nil, nil }

func daysAgo(n int) time.Time { return time.Now().UTC().Add(-time.Duration(n) * 24 * time.Hour) }

// Two tiers with disjoint ids ordered like their timestamps: database ids
// 15..19 (days 5..1) and archived ids 100..139 (days 75..36), plus another
// workspace's archived alerts that must never surface.
type tiers struct {
	router http.Handler
	fake   *archivetest.Fake
}

func newTiers(t *testing.T, withReader bool) tiers {
	t.Helper()
	db := &tierDB{}
	for d := 1; d <= 5; d++ {
		db.alerts = append(db.alerts, store.Alert{ID: int64(20 - d), EventID: fmt.Sprintf("db-%d", d), CreatedAt: daysAgo(d), Payload: json.RawMessage(`{}`)})
	}
	fake := archivetest.New()
	ar, err := archive.NewStore(fake)
	require.NoError(t, err)
	ar.WithChunkRows(1)
	byMonth := map[string][]store.WorkspaceAlert{}
	add := func(ws string, id int64, day int) {
		a := store.Alert{ID: id, EventID: fmt.Sprintf("ar-%d", id), CreatedAt: daysAgo(day), Payload: json.RawMessage(`{}`)}
		m := store.ArchiveMonth(a.CreatedAt)
		byMonth[m] = append(byMonth[m], store.WorkspaceAlert{WorkspaceID: ws, Alert: a})
	}
	for day := 36; day <= 75; day++ {
		add(string(workspace.Default), int64(175-day), day)
		add("other", int64(900+day), day)
	}
	for m, all := range byMonth {
		from, err := time.Parse("2006-01", m)
		require.NoError(t, err)
		_, err = ar.ArchivePartition(context.Background(), store.PartitionDropRecord{Name: "alerts_" + m, From: from, To: from.AddDate(0, 1, 0)}, all)
		require.NoError(t, err)
	}
	srv := New(db, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger()).WithAlertRetention(tierRetention)
	if withReader {
		srv.WithArchiveReader(ar)
	}
	return tiers{router: srv.Routes(), fake: fake}
}

func (ts tiers) get(t *testing.T, path string, q url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path+"?"+q.Encode(), nil)
	rec := httptest.NewRecorder()
	ts.router.ServeHTTP(rec, req)
	return rec
}

type listBody struct {
	Alerts             []store.Alert `json:"alerts"`
	NextCursor         string        `json:"next_cursor"`
	Truncated          bool          `json:"truncated"`
	TruncationBoundary string        `json:"truncation_boundary"`
	TruncationReason   string        `json:"truncation_reason"`
}

func (ts tiers) list(t *testing.T, q url.Values) (listBody, *httptest.ResponseRecorder) {
	t.Helper()
	rec := ts.get(t, "/alerts", q)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var b listBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &b))
	return b, rec
}

func alertIDs(as []store.Alert) []int64 {
	out := make([]int64, 0, len(as))
	for _, a := range as {
		out = append(out, a.ID)
	}
	return out
}

func rfc(t time.Time) string { return t.Format(time.RFC3339) }

func TestAlertsInsideRetentionAreNotTruncated(t *testing.T) {
	ts := newTiers(t, true)
	b, rec := ts.list(t, url.Values{"from": {rfc(daysAgo(10))}, "include_archived": {"true"}})
	assert.False(t, b.Truncated)
	assert.Equal(t, "false", rec.Header().Get("X-Alerts-Truncated"))
	assert.Equal(t, []int64{19, 18, 17, 16, 15}, alertIDs(b.Alerts))
}

func TestAlertsPastTheHorizonReportTruncationWithTheBoundary(t *testing.T) {
	ts := newTiers(t, true)
	b, rec := ts.list(t, url.Values{"from": {rfc(daysAgo(60))}})
	assert.True(t, b.Truncated)
	assert.Equal(t, truncRetention, b.TruncationReason)
	assert.Equal(t, []int64{19, 18, 17, 16, 15}, alertIDs(b.Alerts), "the short answer is the same rows as before, now labelled")
	boundary, err := time.Parse(time.RFC3339, b.TruncationBoundary)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(-tierRetention), boundary, time.Minute)
	assert.Equal(t, "true", rec.Header().Get("X-Alerts-Truncated"))
	assert.Equal(t, b.TruncationBoundary, rec.Header().Get("X-Alerts-Truncation-Boundary"))
}

func TestIncludeArchivedWithoutAnArchiveSaysSo(t *testing.T) {
	ts := newTiers(t, false)
	b, _ := ts.list(t, url.Values{"from": {rfc(daysAgo(60))}, "include_archived": {"true"}})
	assert.True(t, b.Truncated)
	assert.Equal(t, truncArchiveOff, b.TruncationReason)
}

func TestEntirelyArchivedRangeIsServedFromTheArchive(t *testing.T) {
	ts := newTiers(t, true)
	b, _ := ts.list(t, url.Values{"from": {rfc(daysAgo(40).Add(-time.Hour))}, "to": {rfc(daysAgo(37).Add(time.Hour))}, "include_archived": {"true"}, "limit": {"50"}})
	assert.False(t, b.Truncated)
	assert.Equal(t, []int64{138, 137, 136, 135}, alertIDs(b.Alerts)) // days 37..40
	for _, a := range b.Alerts {
		assert.True(t, a.Archived, "the tier is indicated explicitly")
	}
}

func pageAll(t *testing.T, ts tiers, q url.Values) []int64 {
	t.Helper()
	var got []int64
	for i := 0; i < 100; i++ {
		b, _ := ts.list(t, q)
		got = append(got, alertIDs(b.Alerts)...)
		if b.NextCursor == "" {
			return got
		}
		q.Set("cursor", b.NextCursor)
	}
	t.Fatal("paging did not terminate")
	return nil
}

func TestPagingAcrossTheBoundaryNeitherRepeatsNorSkips(t *testing.T) {
	ts := newTiers(t, true)
	var desc, asc []int64
	for id := int64(19); id >= 15; id-- {
		desc = append(desc, id)
	}
	for id := int64(139); id >= 100; id-- {
		desc = append(desc, id)
	}
	for i := len(desc) - 1; i >= 0; i-- {
		asc = append(asc, desc[i])
	}
	base := func(sort string) url.Values {
		return url.Values{"from": {rfc(daysAgo(90))}, "include_archived": {"true"}, "limit": {"4"}, "sort": {sort}}
	}
	assert.Equal(t, desc, pageAll(t, ts, base("created_at_desc")))
	assert.Equal(t, asc, pageAll(t, ts, base("created_at_asc")))
}

func TestArchiveReadsAreBoundedByTheFetchCap(t *testing.T) {
	ts := newTiers(t, true)
	b, rec := ts.list(t, url.Values{"from": {rfc(daysAgo(90))}, "include_archived": {"true"}, "limit": {"500"}})
	assert.True(t, b.Truncated)
	assert.Equal(t, truncArchiveCap, b.TruncationReason)
	assert.Equal(t, "true", rec.Header().Get("X-Alerts-Truncated"))
	assert.Len(t, b.Alerts, 5+store.DefaultArchiveMaxChunks, "database rows plus one row per fetched chunk")
	assert.NotEmpty(t, b.NextCursor, "a capped answer can be continued")
	oldest := b.Alerts[len(b.Alerts)-1].CreatedAt
	boundary, err := time.Parse(time.RFC3339, b.TruncationBoundary)
	require.NoError(t, err)
	assert.WithinDuration(t, oldest, boundary, time.Second)
}

func TestCorruptArchiveChunkIsAnErrorNamingIt(t *testing.T) {
	ts := newTiers(t, true)
	for _, k := range ts.fake.Keys("partitions/") {
		ts.fake.Put(k, []byte("garbage\n"))
	}
	rec := ts.get(t, "/alerts", url.Values{"from": {rfc(daysAgo(90))}, "include_archived": {"true"}, "sort": {"created_at_asc"}})
	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Contains(t, rec.Body.String(), "partitions/")
	assert.Contains(t, rec.Body.String(), "corrupt")
}

func TestMissingArchiveChunkIsAnErrorNamingIt(t *testing.T) {
	ts := newTiers(t, true)
	for _, k := range ts.fake.Keys("partitions/") {
		ts.fake.Delete(k)
	}
	rec := ts.get(t, "/alerts", url.Values{"from": {rfc(daysAgo(90))}, "include_archived": {"true"}})
	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Contains(t, rec.Body.String(), "partitions/")
	assert.Contains(t, rec.Body.String(), "missing")
}

func TestArchivedAlertsNeverLeakAnotherWorkspace(t *testing.T) {
	ts := newTiers(t, true)
	for _, id := range pageAll(t, ts, url.Values{"from": {rfc(daysAgo(90))}, "include_archived": {"true"}, "limit": {"50"}}) {
		assert.Less(t, id, int64(900), "alert %d belongs to another workspace", id)
	}
}

func TestInvalidIncludeArchivedIsRejected(t *testing.T) {
	ts := newTiers(t, true)
	rec := ts.get(t, "/alerts", url.Values{"include_archived": {"maybe"}})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestExportsReportTruncationLikeTheListing(t *testing.T) {
	ts := newTiers(t, true)
	for _, path := range []string{"/alerts.csv", "/alerts/export"} {
		// Not opted in: the headers carry the same truncation as the listing.
		rec := ts.get(t, path, url.Values{"from": {rfc(daysAgo(60))}})
		require.Equal(t, http.StatusOK, rec.Code, path)
		assert.Equal(t, "true", rec.Header().Get("X-Alerts-Truncated"), path)
		assert.NotEmpty(t, rec.Header().Get("X-Alerts-Truncation-Boundary"), path)
		assert.Equal(t, truncRetention, rec.Header().Get("X-Alerts-Truncation-Reason"), path)

		// Opted in: the archive rows are streamed and the cap is reported in
		// trailers, since it is only known once the body is written.
		rec = ts.get(t, path, url.Values{"from": {rfc(daysAgo(90))}, "include_archived": {"true"}})
		require.Equal(t, http.StatusOK, rec.Code, path)
		res := rec.Result()
		_ = res.Body.Close()
		assert.Equal(t, "true", res.Trailer.Get("X-Alerts-Truncated"), path)
		assert.Equal(t, truncArchiveCap, res.Trailer.Get("X-Alerts-Truncation-Reason"), path)
		assert.Contains(t, rec.Body.String(), "ar-", path)
		assert.Contains(t, rec.Body.String(), "db-", path)
		assert.NotContains(t, strings.ToLower(rec.Body.String()), "ar-9", path)
	}
}
