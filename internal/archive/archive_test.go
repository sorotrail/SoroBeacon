package archive

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/store"
)

func sampleBatch() []store.Alert {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return []store.Alert{
		{ID: 1, MonitorID: 7, RuleID: 9, EventID: "ev-1", Ledger: 100, CreatedAt: created,
			Payload: json.RawMessage(`{"contract_id":"CAAA","event_name":"transfer"}`)},
		{ID: 2, MonitorID: 7, RuleID: 9, EventID: "ev-2", Ledger: 101, CreatedAt: created,
			Payload: json.RawMessage(`{"contract_id":"CAAA","event_name":"mint"}`)},
	}
}

// TestPrunerWritesNDJSONUnderADeterministicKey covers the format choice: one
// JSON object per line, keyed by the batch's own id range.
func TestPrunerWritesNDJSONUnderADeterministicKey(t *testing.T) {
	dir := t.TempDir()
	back, err := NewDir(dir)
	require.NoError(t, err)
	pruner, err := NewPruner(back)
	require.NoError(t, err)

	batch := sampleBatch()
	require.NoError(t, pruner.Archive(context.Background(), batch))

	path := filepath.Join(dir, filepath.FromSlash(BatchKey(batch)))
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	require.Len(t, lines, 2, "one NDJSON object per alert")
	for i, line := range lines {
		var got store.Alert
		require.NoError(t, json.Unmarshal([]byte(line), &got))
		assert.Equal(t, batch[i].ID, got.ID)
		assert.Equal(t, batch[i].EventID, got.EventID)
	}
}

// TestPrunerArchiveIsIdempotent proves re-running the same batch overwrites
// rather than duplicating: a crash after the archive but before the delete
// must not leave the object with two copies of the batch.
func TestPrunerArchiveIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	back, err := NewDir(dir)
	require.NoError(t, err)
	pruner, err := NewPruner(back)
	require.NoError(t, err)

	batch := sampleBatch()
	require.NoError(t, pruner.Archive(context.Background(), batch))
	require.NoError(t, pruner.Archive(context.Background(), batch))

	raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(BatchKey(batch))))
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(string(raw), "\n"), "a re-run overwrites, it does not append")
}

// TestArchiveNeverCarriesChannelConfig is the security invariant: an archive
// object is built only from alerts, so a webhook URL or bot token that lives
// on a channel can never reach it.
func TestArchiveNeverCarriesChannelConfig(t *testing.T) {
	const secret = "https://hooks.example/T000/B000/s3cr3t-token"

	dir := t.TempDir()
	back, err := NewDir(dir)
	require.NoError(t, err)
	pruner, err := NewPruner(back)
	require.NoError(t, err)

	// A channel exists with the secret, but only the alerts are archived.
	_ = store.Channel{ID: 3, Name: "ops", Type: "slack", Config: json.RawMessage(`{"webhook_url":"` + secret + `"}`)}
	batch := sampleBatch()
	require.NoError(t, pruner.Archive(context.Background(), batch))

	raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(BatchKey(batch))))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), secret)
	assert.NotContains(t, string(raw), "webhook_url")
}

func TestFromURL(t *testing.T) {
	t.Run("directory path", func(t *testing.T) {
		dir := t.TempDir()
		back, err := FromURL(filepath.Join(dir, "archive"))
		require.NoError(t, err)
		_, ok := back.(*Dir)
		assert.True(t, ok)
	})

	t.Run("file and dir schemes", func(t *testing.T) {
		for _, raw := range []string{"file:///tmp/sorobeacon-archive-test", "dir:///tmp/sorobeacon-archive-test"} {
			back, err := FromURL(raw)
			require.NoError(t, err, raw)
			_, ok := back.(*Dir)
			assert.True(t, ok, raw)
		}
		_ = os.RemoveAll("/tmp/sorobeacon-archive-test")
	})

	t.Run("s3 requires credentials", func(t *testing.T) {
		t.Setenv("AWS_ACCESS_KEY_ID", "")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "")
		_, err := FromURL("s3://bucket/prefix")
		assert.ErrorContains(t, err, "AWS_ACCESS_KEY_ID")
	})

	t.Run("empty and unsupported", func(t *testing.T) {
		_, err := FromURL("   ")
		assert.Error(t, err)
		_, err = FromURL("gs://bucket/prefix")
		assert.ErrorContains(t, err, "unsupported")
	})
}

// TestS3ArchiveSignsAndUploads drives the S3 backend against a fake server: it
// must PUT the NDJSON body to /<bucket>/<prefix>/<key> with a SigV4
// Authorization header.
func TestS3ArchiveSignsAndUploads(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	var gotPath, gotBody, gotAuth, gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	back, err := FromURL("s3://test-bucket/prefix?region=us-east-1&endpoint=" + srv.URL)
	require.NoError(t, err)
	pruner, err := NewPruner(back)
	require.NoError(t, err)

	batch := sampleBatch()
	require.NoError(t, pruner.Archive(context.Background(), batch))

	assert.Equal(t, "/test-bucket/prefix/"+BatchKey(batch), gotPath)
	assert.True(t, strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 "), gotAuth)
	assert.Contains(t, gotAuth, "Credential=AKIAEXAMPLE/")
	assert.Equal(t, "application/x-ndjson", gotType)

	lines := strings.Split(strings.TrimSpace(gotBody), "\n")
	assert.Len(t, lines, 2)
}

// TestS3ArchiveSurfacesHTTPError proves a failed upload is an error, which is
// what the pruner turns into "skip the delete".
func TestS3ArchiveSurfacesHTTPError(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "access denied", http.StatusForbidden)
	}))
	defer srv.Close()

	back, err := FromURL("s3://test-bucket/prefix?endpoint=" + srv.URL)
	require.NoError(t, err)
	pruner, err := NewPruner(back)
	require.NoError(t, err)

	err = pruner.Archive(context.Background(), sampleBatch())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
}
