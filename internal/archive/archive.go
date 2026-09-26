// Package archive persists alerts that retention is about to delete, so an
// operator can keep history without paying for it in Postgres. Archiving is
// off by default: nothing is written unless an Archiver is configured.
//
// Format: NDJSON, one alert per line. It streams (a batch never has to be
// buffered whole), it is greppable with ordinary tools, and it is trivially
// re-readable. Parquet would be smaller and queryable at rest, but it needs a
// schema dependency and would make the common "did this event ever fire?"
// question harder, not easier; the payload is JSON already, so NDJSON keeps
// the archive self-describing.
//
// Alert payloads carry contract data (contract id, topics, value). They do
// not carry channel config — that lives in the channels table and is never
// read by this package — so an archive object can never leak a webhook URL,
// bot token or SMTP credential. See archive_test.go, which pins that.
package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// Archiver stores one archive object. Implementations must overwrite an
// existing object at the same key rather than failing or appending: that is
// what makes re-running an archive idempotent.
type Archiver interface {
	Archive(ctx context.Context, key string, r io.Reader) error
}

// Pruner implements store.AlertArchiver: it serialises each expired batch to
// NDJSON under a deterministic key and hands it to the backend. The key is
// derived from the batch's own id range, so the same batch always lands on the
// same object and a retry overwrites instead of duplicating.
type Pruner struct {
	back Archiver
}

// NewPruner wraps a backend. A nil backend is a programming error, so it is
// rejected rather than silently dropped.
func NewPruner(back Archiver) (*Pruner, error) {
	if back == nil {
		return nil, errors.New("archive: nil backend")
	}
	return &Pruner{back: back}, nil
}

// Archive writes one batch of expired alerts. An empty batch is a no-op.
func (p *Pruner) Archive(ctx context.Context, alerts []store.Alert) error {
	if len(alerts) == 0 {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for i := range alerts {
		if err := enc.Encode(&alerts[i]); err != nil {
			return fmt.Errorf("archive: encode alert %d: %w", alerts[i].ID, err)
		}
	}
	if err := p.back.Archive(ctx, BatchKey(alerts), bytes.NewReader(buf.Bytes())); err != nil {
		return fmt.Errorf("archive: store batch: %w", err)
	}
	return nil
}

// BatchKey is alerts/<utc-day>/<firstID>-<lastID>.ndjson. The day keeps the
// archive browsable; the id range makes the key stable for a given batch so a
// retry targets the same object.
func BatchKey(alerts []store.Alert) string {
	first, last := alerts[0], alerts[len(alerts)-1]
	day := first.CreatedAt.UTC().Format("2006-01-02")
	return fmt.Sprintf("alerts/%s/%010d-%010d.ndjson", day, first.ID, last.ID)
}

// FromURL builds an Archiver from an ARCHIVE_URL value:
//
//	/var/lib/sorobeacon/archive      a local directory (also file:// and dir://)
//	s3://bucket/prefix               an S3 bucket, region from ?region= or
//	                                 AWS_REGION, credentials from
//	                                 AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY
//
// An empty value is an error; callers check for "unset" first and treat it as
// "archiving disabled", which is why there is no silent no-op here.
func FromURL(raw string) (Archiver, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("archive: ARCHIVE_URL is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("archive: invalid ARCHIVE_URL: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "", "file", "dir":
		return NewDir(localPath(raw, u))
	case "s3":
		return newS3(u)
	default:
		return nil, fmt.Errorf("archive: unsupported ARCHIVE_URL scheme %q (want a directory path, file://, dir:// or s3://)", u.Scheme)
	}
}

// localPath recovers the filesystem path from a plain path or a file/dir URL.
// url.Parse puts the first segment of file://relative/path in Host, so it is
// rejoined; file:///abs/path parses cleanly.
func localPath(raw string, u *url.URL) string {
	if u.Scheme == "" {
		return raw
	}
	if u.Host != "" {
		return u.Host + u.Path
	}
	return u.Path
}

// Dir stores archive objects under a local directory. It is the default
// backend and the one the tests use, so archiving is testable without cloud
// access.
type Dir struct {
	root string
}

// NewDir creates (if needed) and returns a directory-backed Archiver.
func NewDir(root string) (*Dir, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("archive: directory path is empty")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("archive: create directory %q: %w", root, err)
	}
	return &Dir{root: root}, nil
}

// Archive writes the object at root/key. It writes to a temporary file in the
// destination directory and renames it into place, so a crash mid-write cannot
// leave a truncated object that a later run would treat as done, and a re-run
// simply overwrites.
func (d *Dir) Archive(ctx context.Context, key string, r io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path := filepath.Join(d.root, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("archive: create %q: %w", filepath.Dir(path), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".archive-*")
	if err != nil {
		return fmt.Errorf("archive: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed

	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("archive: write %q: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("archive: close %q: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("archive: rename into %q: %w", path, err)
	}
	return nil
}
