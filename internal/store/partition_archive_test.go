package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingPartitionArchiver notes, at the moment it is called, whether the
// partition it is asked to archive still exists. Archive-before-drop means it
// must.
type recordingPartitionArchiver struct {
	p            *Postgres
	err          error
	partExisted  bool
	got          []WorkspaceAlert
	reportedRows int64
}

func (r *recordingPartitionArchiver) ArchivePartition(ctx context.Context, rec PartitionDropRecord, alerts []WorkspaceAlert) (int64, error) {
	names, err := r.p.alertPartitions(ctx)
	if err != nil {
		return 0, err
	}
	for _, n := range names {
		if n == rec.Name {
			r.partExisted = true
		}
	}
	r.got = alerts
	if r.err != nil {
		return 0, r.err
	}
	if r.reportedRows != 0 {
		return r.reportedRows, nil
	}
	return int64(len(alerts)), nil
}

func seedExpiredAlert(t *testing.T, p *Postgres, part string) {
	t.Helper()
	ctx := context.Background()
	dropPartition(t, p, part)
	t.Cleanup(func() { dropPartition(t, p, part) })
	require.NoError(t, p.EnsureAlertPartitions(ctx, time.Date(2001, time.April, 1, 0, 0, 0, 0, time.UTC), 1))
	m := &Monitor{Name: "archive-mon", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, p.CreateMonitor(ctx, m))
	r := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, p.CreateRule(ctx, r))
	a := &Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "ev-archive-2001"}
	_, err := p.CreateAlert(ctx, a)
	require.NoError(t, err)
	require.NoError(t, p.setAlertCreatedAt(ctx, a.ID, time.Date(2001, time.April, 10, 12, 0, 0, 0, time.UTC)))
}

func archiveLifecycleOpts(arch PartitionArchiver) PartitionLifecycleOptions {
	return PartitionLifecycleOptions{
		Now:               time.Date(2098, time.May, 20, 0, 0, 0, 0, time.UTC),
		RunwayMonths:      1,
		Retention:         90 * 365 * 24 * time.Hour,
		PartitionArchiver: arch,
		// Any non-nil row archiver must not disable partition drops once a
		// partition archiver is present.
		Archiver: nopRowArchiver{},
	}
}

type nopRowArchiver struct{}

func (nopRowArchiver) Archive(context.Context, []Alert) error { return nil }

func TestPostgresPartitionIsArchivedBeforeItIsDropped(t *testing.T) {
	p := newPartitionStore(t)
	ctx := context.Background()
	const part = "alerts_2001_04"
	t.Cleanup(func() { dropPartition(t, p, "alerts_2098_05") })
	seedExpiredAlert(t, p, part)

	arch := &recordingPartitionArchiver{p: p}
	_, err := p.MaintainAlertPartitions(ctx, archiveLifecycleOpts(arch))
	require.NoError(t, err)

	assert.True(t, arch.partExisted, "the archive must be taken while the partition still exists")
	require.NotEmpty(t, arch.got)
	assert.NotEmpty(t, arch.got[0].WorkspaceID, "archived alerts carry their workspace")
	names, err := p.alertPartitions(ctx)
	require.NoError(t, err)
	assert.NotContains(t, names, part, "a verified archive lets the partition go")
}

func TestPostgresPartitionSurvivesAFailedArchive(t *testing.T) {
	p := newPartitionStore(t)
	ctx := context.Background()
	const part = "alerts_2001_04"
	t.Cleanup(func() { dropPartition(t, p, "alerts_2098_05") })
	seedExpiredAlert(t, p, part)

	arch := &recordingPartitionArchiver{p: p, err: errors.New("bucket unavailable")}
	_, err := p.MaintainAlertPartitions(ctx, archiveLifecycleOpts(arch))
	require.Error(t, err)
	assert.Contains(t, err.Error(), part)

	names, err := p.alertPartitions(ctx)
	require.NoError(t, err)
	assert.Contains(t, names, part, "nothing is dropped when the archive did not complete")
	var n int
	require.NoError(t, p.pool.QueryRow(ctx, `SELECT count(*) FROM `+part).Scan(&n))
	assert.Equal(t, 1, n)
}

func TestPostgresPartitionIsNotDroppedWhenArchiveCountDisagrees(t *testing.T) {
	p := newPartitionStore(t)
	ctx := context.Background()
	const part = "alerts_2001_04"
	t.Cleanup(func() { dropPartition(t, p, "alerts_2098_05") })
	seedExpiredAlert(t, p, part)

	arch := &recordingPartitionArchiver{p: p, reportedRows: 99}
	_, err := p.MaintainAlertPartitions(ctx, archiveLifecycleOpts(arch))
	require.Error(t, err)
	names, err := p.alertPartitions(ctx)
	require.NoError(t, err)
	assert.Contains(t, names, part)
}
