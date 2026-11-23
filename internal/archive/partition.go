package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// Partition archives are laid out so that a read never needs to list the
// bucket and never mixes tenants:
//
//	manifests/<YYYY-MM>.json                     one per archived partition
//	partitions/<YYYY-MM>/<ws-hash>/<NNNNNN>.ndjson   one workspace's alerts
//
// A chunk holds exactly one workspace, in (created_at, id) order, so a
// workspace-scoped read fetches only that workspace's objects. Each line is an
// envelope carrying the workspace id as well, and the reader re-checks it, so
// a chunk that was somehow written or swapped across tenants is an error
// rather than a leak. The manifest records every chunk's checksum, size and
// row count, and is written last: its existence means the chunks were
// complete when it was written.
const (
	manifestVersion = 1
	// DefaultChunkRows bounds one chunk's size, which bounds how much a
	// single archive read has to download and hold.
	DefaultChunkRows = 5000
	// maxMonthScan and missingMonthStop bound how far back a read looks for
	// manifests when the caller gave no lower bound. Without listing the
	// bucket, a run of months with no manifest is the signal that history
	// ends there.
	maxMonthScan     = 240
	missingMonthStop = 12
	defaultReadLimit = 50
	maxReadLimit     = 500
)

// ErrObjectNotFound is what an ObjectStore returns from Get for a missing key.
var ErrObjectNotFound = errors.New("archive: object not found")

// ObjectStore is the object-storage surface the partition archive needs:
// the existing idempotent write plus a read. Dir and S3 implement it; tests
// use an in-memory fake.
type ObjectStore interface {
	Archiver
	Get(ctx context.Context, key string) (io.ReadCloser, error)
}

// ChunkRef describes one chunk in a manifest.
type ChunkRef struct {
	Key         string    `json:"key"`
	WorkspaceID string    `json:"workspace_id"`
	Rows        int64     `json:"rows"`
	Bytes       int64     `json:"bytes"`
	SHA256      string    `json:"sha256"`
	MinID       int64     `json:"min_id"`
	MaxID       int64     `json:"max_id"`
	MinCreated  time.Time `json:"min_created_at"`
	MaxCreated  time.Time `json:"max_created_at"`
}

// Manifest lists every chunk of one archived partition.
type Manifest struct {
	Version   int        `json:"version"`
	Partition string     `json:"partition"`
	Month     string     `json:"month"`
	From      time.Time  `json:"from"`
	To        time.Time  `json:"to"`
	Total     int64      `json:"total"`
	Chunks    []ChunkRef `json:"chunks"`
}

type envelope struct {
	WorkspaceID string      `json:"workspace_id"`
	Alert       store.Alert `json:"alert"`
}

// Store archives partitions and reads them back. It implements
// store.PartitionArchiver and store.ArchiveReader.
type Store struct {
	back      ObjectStore
	chunkRows int
	now       func() time.Time
}

// NewStore wraps an object store. A nil backend is a programming error.
func NewStore(back ObjectStore) (*Store, error) {
	if back == nil {
		return nil, errors.New("archive: nil backend")
	}
	return &Store{back: back, chunkRows: DefaultChunkRows, now: time.Now}, nil
}

// WithChunkRows overrides the chunk size (tests use tiny chunks).
func (s *Store) WithChunkRows(n int) *Store {
	if n > 0 {
		s.chunkRows = n
	}
	return s
}

// WithClock overrides the clock used to bound the manifest scan when a query
// has no upper time bound.
func (s *Store) WithClock(now func() time.Time) *Store {
	if now != nil {
		s.now = now
	}
	return s
}

// ManifestKey is the manifest object for a UTC month ("2026-03").
func ManifestKey(month string) string { return "manifests/" + month + ".json" }

// workspaceDir hashes the workspace id for the key: ids are caller-chosen
// strings, and a hash keeps odd characters (or "..") out of object paths.
func workspaceDir(ws string) string {
	sum := sha256.Sum256([]byte(ws))
	return "ws-" + hex.EncodeToString(sum[:8])
}

// ArchivePartition writes every alert in the partition as per-workspace
// chunks, writes the manifest, then reads everything back and checks it
// against the checksums and the expected row count. Only a nil error means
// the copy is durable; any failure leaves the source partition untouched
// because the caller drops only after this returns.
func (s *Store) ArchivePartition(ctx context.Context, rec store.PartitionDropRecord, alerts []store.WorkspaceAlert) (int64, error) {
	month := store.ArchiveMonth(rec.From)
	byWS := map[string][]store.Alert{}
	for _, wa := range alerts {
		byWS[wa.WorkspaceID] = append(byWS[wa.WorkspaceID], wa.Alert)
	}
	wss := make([]string, 0, len(byWS))
	for ws := range byWS {
		wss = append(wss, ws)
	}
	sort.Strings(wss)

	m := Manifest{Version: manifestVersion, Partition: rec.Name, Month: month, From: rec.From.UTC(), To: rec.To.UTC(), Total: int64(len(alerts))}
	for _, ws := range wss {
		as := byWS[ws]
		sort.Slice(as, func(i, j int) bool {
			if !as[i].CreatedAt.Equal(as[j].CreatedAt) {
				return as[i].CreatedAt.Before(as[j].CreatedAt)
			}
			return as[i].ID < as[j].ID
		})
		for i, seq := 0, 0; i < len(as); i, seq = i+s.chunkRows, seq+1 {
			end := min(i+s.chunkRows, len(as))
			ref, body, err := encodeChunk(month, ws, seq, as[i:end])
			if err != nil {
				return 0, err
			}
			if err := s.back.Archive(ctx, ref.Key, bytes.NewReader(body)); err != nil {
				return 0, fmt.Errorf("archive: store chunk %s: %w", ref.Key, err)
			}
			m.Chunks = append(m.Chunks, ref)
		}
	}
	data, err := json.Marshal(m)
	if err != nil {
		return 0, fmt.Errorf("archive: encode manifest %s: %w", month, err)
	}
	if err := s.back.Archive(ctx, ManifestKey(month), bytes.NewReader(data)); err != nil {
		return 0, fmt.Errorf("archive: store manifest %s: %w", ManifestKey(month), err)
	}

	got, err := s.readManifest(ctx, month)
	if err != nil {
		return 0, err
	}
	if got == nil {
		return 0, &store.ArchiveChunkError{Key: ManifestKey(month), Reason: "missing after write"}
	}
	var rows int64
	for _, ref := range got.Chunks {
		as, err := s.loadChunk(ctx, ref)
		if err != nil {
			return 0, err
		}
		rows += int64(len(as))
	}
	if rows != int64(len(alerts)) || got.Total != int64(len(alerts)) {
		return 0, &store.ArchiveChunkError{Key: ManifestKey(month), Reason: fmt.Sprintf("verified %d rows, expected %d", rows, len(alerts))}
	}
	return rows, nil
}

func encodeChunk(month, ws string, seq int, as []store.Alert) (ChunkRef, []byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	ref := ChunkRef{
		Key:         fmt.Sprintf("partitions/%s/%s/%06d.ndjson", month, workspaceDir(ws), seq),
		WorkspaceID: ws,
		Rows:        int64(len(as)),
		MinID:       as[0].ID,
		MaxID:       as[0].ID,
		MinCreated:  as[0].CreatedAt.UTC(),
		MaxCreated:  as[len(as)-1].CreatedAt.UTC(),
	}
	for i := range as {
		if err := enc.Encode(envelope{WorkspaceID: ws, Alert: as[i]}); err != nil {
			return ChunkRef{}, nil, fmt.Errorf("archive: encode alert %d: %w", as[i].ID, err)
		}
		ref.MinID = min(ref.MinID, as[i].ID)
		ref.MaxID = max(ref.MaxID, as[i].ID)
	}
	sum := sha256.Sum256(buf.Bytes())
	ref.SHA256 = hex.EncodeToString(sum[:])
	ref.Bytes = int64(buf.Len())
	return ref, buf.Bytes(), nil
}

// readManifest returns nil, nil when the month has no manifest (nothing was
// ever archived for it). Any other failure is an error naming the manifest.
func (s *Store) readManifest(ctx context.Context, month string) (*Manifest, error) {
	key := ManifestKey(month)
	rc, err := s.back.Get(ctx, key)
	if errors.Is(err, ErrObjectNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, &store.ArchiveChunkError{Key: key, Reason: "read failed", Err: err}
	}
	defer rc.Close()
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(rc, 64<<20)).Decode(&m); err != nil {
		return nil, &store.ArchiveChunkError{Key: key, Reason: "corrupt manifest", Err: err}
	}
	if m.Version != manifestVersion || m.Month != month {
		return nil, &store.ArchiveChunkError{Key: key, Reason: "corrupt manifest: unexpected version or month"}
	}
	return &m, nil
}

// loadChunk downloads a chunk and verifies size, checksum, row count and that
// every row belongs to the chunk's workspace before returning any of it.
func (s *Store) loadChunk(ctx context.Context, ref ChunkRef) ([]store.Alert, error) {
	rc, err := s.back.Get(ctx, ref.Key)
	if errors.Is(err, ErrObjectNotFound) {
		return nil, &store.ArchiveChunkError{Key: ref.Key, Reason: "missing"}
	}
	if err != nil {
		return nil, &store.ArchiveChunkError{Key: ref.Key, Reason: "read failed", Err: err}
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, ref.Bytes+1))
	if err != nil {
		return nil, &store.ArchiveChunkError{Key: ref.Key, Reason: "read failed", Err: err}
	}
	sum := sha256.Sum256(body)
	if int64(len(body)) != ref.Bytes || hex.EncodeToString(sum[:]) != ref.SHA256 {
		return nil, &store.ArchiveChunkError{Key: ref.Key, Reason: "corrupt: checksum mismatch"}
	}
	var out []store.Alert
	for _, line := range bytes.Split(body, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var e envelope
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, &store.ArchiveChunkError{Key: ref.Key, Reason: "corrupt: bad row", Err: err}
		}
		if e.WorkspaceID != ref.WorkspaceID {
			return nil, &store.ArchiveChunkError{Key: ref.Key, Reason: "corrupt: row belongs to another workspace"}
		}
		out = append(out, e.Alert)
	}
	if int64(len(out)) != ref.Rows {
		return nil, &store.ArchiveChunkError{Key: ref.Key, Reason: "corrupt: row count mismatch"}
	}
	return out, nil
}

// manifestsFor returns the manifests that can overlap the filter's range,
// oldest first.
func (s *Store) manifestsFor(ctx context.Context, f store.AlertFilter) ([]*Manifest, error) {
	upper := f.To
	if upper.IsZero() {
		upper = s.now()
	}
	upper = upper.UTC()
	cur := time.Date(upper.Year(), upper.Month(), 1, 0, 0, 0, 0, time.UTC)
	var lower time.Time
	if !f.From.IsZero() {
		fr := f.From.UTC()
		lower = time.Date(fr.Year(), fr.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	var out []*Manifest
	missing := 0
	for i := 0; i < maxMonthScan; i++ {
		if !lower.IsZero() && cur.Before(lower) {
			break
		}
		m, err := s.readManifest(ctx, store.ArchiveMonth(cur))
		if err != nil {
			return nil, err
		}
		if m == nil {
			missing++
			if lower.IsZero() && missing >= missingMonthStop {
				break
			}
		} else {
			missing = 0
			out = append(out, m)
		}
		cur = cur.AddDate(0, -1, 0)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// ListArchivedAlerts serves one page of a workspace's archived alerts in the
// same order, shape and cursor semantics as the database: the cursor is the
// id of the last alert of the previous page and the next page starts strictly
// after it in sort order. It fetches at most q.MaxChunks chunks.
func (s *Store) ListArchivedAlerts(ctx context.Context, ws workspace.ID, q store.ArchiveQuery) (store.ArchiveResult, error) {
	f := q.Filter
	limit := f.Limit
	if limit <= 0 || limit > maxReadLimit {
		limit = defaultReadLimit
	}
	maxChunks := q.MaxChunks
	if maxChunks <= 0 {
		maxChunks = store.DefaultArchiveMaxChunks
	}
	asc := f.Sort == "created_at_asc"

	manifests, err := s.manifestsFor(ctx, f)
	if err != nil {
		return store.ArchiveResult{}, err
	}
	var refs []ChunkRef
	for _, m := range manifests {
		for _, c := range m.Chunks {
			if c.WorkspaceID != string(ws) {
				continue
			}
			if !f.From.IsZero() && c.MaxCreated.Before(f.From) {
				continue
			}
			if !f.To.IsZero() && !c.MinCreated.Before(f.To) {
				continue
			}
			refs = append(refs, c)
		}
	}
	if !asc {
		for i, j := 0, len(refs)-1; i < j; i, j = i+1, j-1 {
			refs[i], refs[j] = refs[j], refs[i]
		}
	}

	var res store.ArchiveResult
	searching := f.AfterID != 0
	for _, ref := range refs {
		// While hunting for the cursor, a chunk whose id range cannot hold it
		// is skipped without a download; once found, every later chunk is
		// read.
		if searching && (f.AfterID < ref.MinID || f.AfterID > ref.MaxID) {
			continue
		}
		if res.ChunksFetched >= maxChunks {
			res.Capped = true
			break
		}
		rows, err := s.loadChunk(ctx, ref)
		if err != nil {
			return store.ArchiveResult{}, err
		}
		res.ChunksFetched++
		if !asc {
			for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
		for _, a := range rows {
			if searching {
				if a.ID == f.AfterID {
					searching = false
					res.CursorFound = true
				}
				continue
			}
			if !matches(f, a) {
				continue
			}
			a.Archived = true
			res.Alerts = append(res.Alerts, a)
			if len(res.Alerts) == limit {
				return res, nil
			}
		}
	}
	return res, nil
}

// matches applies the filter fields ListAlerts honours. Severity is not among
// them: the database listing ignores it, and the tiers must agree.
func matches(f store.AlertFilter, a store.Alert) bool {
	if !f.From.IsZero() && a.CreatedAt.Before(f.From) {
		return false
	}
	if !f.To.IsZero() && !a.CreatedAt.Before(f.To) {
		return false
	}
	if f.MonitorID != 0 && a.MonitorID != f.MonitorID {
		return false
	}
	if f.RuleID != 0 && a.RuleID != f.RuleID {
		return false
	}
	if f.Network != "" && a.Network != f.Network {
		return false
	}
	switch f.Acknowledged {
	case "acknowledged":
		if a.AcknowledgedAt == nil {
			return false
		}
	case "unacknowledged":
		if a.AcknowledgedAt != nil {
			return false
		}
	}
	if f.ContractID != "" {
		var p struct {
			ContractID string `json:"contract_id"`
		}
		if json.Unmarshal(a.Payload, &p) != nil || p.ContractID != f.ContractID {
			return false
		}
	}
	if f.Query != "" {
		q := strings.ToLower(f.Query)
		if !strings.Contains(strings.ToLower(a.EventID), q) && !strings.Contains(strings.ToLower(string(a.Payload)), q) {
			return false
		}
	}
	return true
}

var (
	_ store.PartitionArchiver = (*Store)(nil)
	_ store.ArchiveReader     = (*Store)(nil)
)

// Tiered is the archiver the server wires: row-batch archiving for the
// retention loop plus whole-partition archiving and reads from one backend.
type Tiered struct {
	*Pruner
	*Store
}
