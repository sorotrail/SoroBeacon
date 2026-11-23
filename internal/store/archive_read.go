package store

import (
	"context"
	"fmt"
	"time"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// DefaultArchiveMaxChunks is the most archive chunks one request will fetch.
// A chunk is one object-store GET, so this is the bound on how slow and how
// expensive an opt-in archive read can be. A request that would need more
// stops at a chunk boundary and says so (see ArchiveResult.Capped); it never
// returns a silently short answer.
const DefaultArchiveMaxChunks = 16

// WorkspaceAlert is an alert together with the workspace that owns it. The
// Alert type deliberately carries no workspace id (the store scopes by
// context), so the partition archiver, which sees every tenant at once, is
// handed the pairing explicitly.
type WorkspaceAlert struct {
	WorkspaceID string
	Alert       Alert
}

// PartitionArchiver copies a whole expired partition to durable storage and
// verifies the copy before returning. A nil error is the store's permission
// to drop the partition: it means every alert is in the archive, readable and
// checksummed. It returns the number of alerts verified so the caller can
// compare it with what is still in the partition at drop time.
type PartitionArchiver interface {
	ArchivePartition(ctx context.Context, rec PartitionDropRecord, alerts []WorkspaceAlert) (int64, error)
}

// ArchiveQuery is one bounded read of archived alerts. Filter has the same
// meaning as for ListAlerts, including Sort, Limit and the AfterID cursor;
// Filter.To is the exclusive upper bound (the retention horizon).
type ArchiveQuery struct {
	Filter    AlertFilter
	MaxChunks int
}

// ArchiveResult is a page of archived alerts in the same shape and order the
// database would have returned them, flagged Archived.
type ArchiveResult struct {
	Alerts []Alert
	// CursorFound reports that Filter.AfterID names an archived alert. False
	// with a non-zero AfterID means the cursor belongs to another tier.
	CursorFound bool
	// ChunksFetched is how many chunk objects this read downloaded.
	ChunksFetched int
	// Capped is true when the read stopped at MaxChunks with matching data
	// possibly remaining.
	Capped bool
}

// ArchiveReader reads archived alerts for one workspace. It lives in the
// store package so callers (the API) depend on this interface and never on
// an object-storage client.
type ArchiveReader interface {
	ListArchivedAlerts(ctx context.Context, ws workspace.ID, q ArchiveQuery) (ArchiveResult, error)
}

// ArchiveChunkError reports a chunk (or manifest) that is missing or fails
// verification. It names the object so an operator can find it; the reader
// returns it instead of a short result.
type ArchiveChunkError struct {
	Key    string
	Reason string
	Err    error
}

func (e *ArchiveChunkError) Error() string {
	msg := fmt.Sprintf("archive chunk %s: %s", e.Key, e.Reason)
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *ArchiveChunkError) Unwrap() error { return e.Err }

// ArchiveMonth is the UTC month a partition covers, as used in object keys.
func ArchiveMonth(t time.Time) string { return t.UTC().Format("2006-01") }
