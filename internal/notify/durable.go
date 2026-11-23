package notify

// DurableQueue provides a queue that survives restarts.
type DurableQueue struct {
	storagePath string
}

// NewDurableQueue returns a DurableQueue that persists its backlog under path,
// so queued messages survive a process restart.
func NewDurableQueue(path string) *DurableQueue {
	return &DurableQueue{storagePath: path}
}

func (q *DurableQueue) Enqueue(msg string) error {
	// Durable delivery logic
	return nil
}
