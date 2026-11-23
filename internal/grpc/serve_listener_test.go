package grpc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// acceptSignalListener reports when the server first waits for a connection,
// so the test knows Serve has started rather than racing cancellation against
// it.
type acceptSignalListener struct {
	net.Listener
	accepted chan struct{}
	once     sync.Once
}

func (l *acceptSignalListener) Accept() (net.Conn, error) {
	l.once.Do(func() { close(l.accepted) })
	return l.Listener.Accept()
}

// failListener rejects every Accept call with a fixed error so ServeListener
// can be tested without racing a real socket shutdown.
type failListener struct {
	addr net.Addr
	err  error
}

func (l failListener) Accept() (net.Conn, error) { return nil, l.err }
func (l failListener) Close() error              { return nil }
func (l failListener) Addr() net.Addr            { return l.addr }

func TestServeListener(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("returns nil when the context is cancelled", func(t *testing.T) {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		s := New(nil, nil, log)
		signaled := &acceptSignalListener{Listener: lis, accepted: make(chan struct{})}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.ServeListener(ctx, signaled) }()

		select {
		case <-signaled.accepted:
		case <-time.After(5 * time.Second):
			t.Fatal("ServeListener did not start serving")
		}
		cancel()

		select {
		case err := <-done:
			assert.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("ServeListener did not return after context cancellation")
		}
	})

	t.Run("returns the listener error", func(t *testing.T) {
		wantErr := errors.New("listener unavailable")
		s := New(nil, nil, log)

		err := s.ServeListener(context.Background(), failListener{
			addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)},
			err:  wantErr,
		})

		require.ErrorIs(t, err, wantErr)
	})
}
