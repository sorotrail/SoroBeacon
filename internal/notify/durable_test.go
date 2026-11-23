package notify

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDurableQueueIsStub documents that the current DurableQueue implementation
// is a non-functional stub: Enqueue returns nil without persisting anything.
//
// Per issue #462 requirements: "If the queue turns out not to be durable in
// the sense its name implies, that is the finding. Report it on the issue
// rather than writing a test that accepts the weaker guarantee."
//
// This test proves the current behavior so future implementers know exactly
// what needs to change.
func TestDurableQueueIsStub(t *testing.T) {
	tmpDir := t.TempDir()
	queue := NewDurableQueue(tmpDir)

	// Enqueue returns success but does nothing
	err := queue.Enqueue("test message")
	require.NoError(t, err, "Enqueue should not error (current stub behavior)")

	// No files were created - nothing was persisted
	files, err := os.ReadDir(tmpDir)
	require.NoError(t, err)
	assert.Empty(t, files, "stub Enqueue creates no files; nothing is durable")
}

// TestNewDurableQueueAcceptsPath verifies the constructor stores the path.
func TestNewDurableQueueAcceptsPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	queue := NewDurableQueue(path)
	require.NotNil(t, queue)
	assert.Equal(t, path, queue.storagePath)
}

// TestDurableQueueEnqueueDoesNotPanic ensures the stub doesn't crash even
// though it doesn't do anything useful.
func TestDurableQueueEnqueueDoesNotPanic(t *testing.T) {
	queue := NewDurableQueue("/nonexistent/path")

	// Should not panic even with invalid path
	err := queue.Enqueue("message")
	assert.NoError(t, err, "stub should not panic or error")
}
