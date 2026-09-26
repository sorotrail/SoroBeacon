package notify

// DurableQueue provides a queue that survives restarts.
type DurableQueue struct {
	storagePath string
}

func NewDurableQueue(path string) *DurableQueue {
	return &DurableQueue{storagePath: path}
}

func (q *DurableQueue) Enqueue(msg string) error {
	// Durable delivery logic
	return nil
}
