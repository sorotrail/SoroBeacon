package poller

import (
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/rules"
)

// TestSetIntervalChangesWhatTheLoopReports pins the reload path's effect on
// the poller: SetInterval makes Interval report the new value immediately,
// which is what the next Run cycle reads.
func TestSetIntervalChangesWhatTheLoopReports(t *testing.T) {
	p := New(nil, nil, rules.NewRegistry(), nil, 5*time.Second, slog.New(slog.DiscardHandler))

	require.Equal(t, 5*time.Second, p.Interval())

	p.SetInterval(250 * time.Millisecond)

	assert.Equal(t, 250*time.Millisecond, p.Interval())
}

// TestSetIntervalIsRaceFreeAgainstConcurrentReaders is the concurrency
// contract a SIGHUP reload relies on: the signal handler writes the new
// interval while the poll loop reads it, and `go test -race` must see no
// data race between the two.
func TestSetIntervalIsRaceFreeAgainstConcurrentReaders(t *testing.T) {
	p := New(nil, nil, rules.NewRegistry(), nil, time.Second, slog.New(slog.DiscardHandler))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	// The reader stands in for the Run loop, which calls Interval after
	// every cycle and before every wait.
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = p.Interval()
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 1; i <= 1000; i++ {
			p.SetInterval(time.Duration(i) * time.Millisecond)
		}
		close(stop)
	}()
	wg.Wait()

	require.Equal(t, 1000*time.Millisecond, p.Interval())
}
