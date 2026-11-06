package grpc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/stretchr/testify/require"
)

type streamAlertStore struct {
	store.Store
	mu       sync.Mutex
	alerts   []store.Alert
	started  chan struct{}
	startOne sync.Once
}

func (s *streamAlertStore) ListAlerts(_ context.Context, f store.AlertFilter) ([]store.Alert, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.Alert
	for _, a := range s.alerts {
		if a.ID > f.AfterID && (f.MonitorID == 0 || a.MonitorID == f.MonitorID) {
			out = append(out, a)
		}
	}
	s.startOne.Do(func() { close(s.started) })
	return out, nil
}

func (s *streamAlertStore) GetMonitor(_ context.Context, id int64) (*store.Monitor, error) {
	return &store.Monitor{ID: id}, nil
}

func (s *streamAlertStore) add(a store.Alert) {
	s.mu.Lock()
	s.alerts = append(s.alerts, a)
	s.mu.Unlock()
}

type captureAlertStream struct {
	ctx     context.Context
	sent    chan *AlertProto
	sendErr error
}

func (s *captureAlertStream) Send(a *AlertProto) error {
	if s.sendErr != nil {
		return s.sendErr
	}
	s.sent <- a
	return nil
}
func (s *captureAlertStream) Context() context.Context { return s.ctx }

func TestStreamAlertsSendsNewAlertsInOrder(t *testing.T) {
	st := &streamAlertStore{started: make(chan struct{})}
	s := &Server{store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &captureAlertStream{ctx: ctx, sent: make(chan *AlertProto, 2)}
	done := make(chan error, 1)
	go func() { done <- s.StreamAlerts(&StreamAlertsRequest{MonitorID: 7}, stream) }()
	select {
	case <-st.started:
	case <-time.After(time.Second):
		t.Fatal("stream did not subscribe")
	}
	st.add(store.Alert{ID: 10, MonitorID: 7, RuleID: 3, EventID: "first", Payload: []byte("one")})
	st.add(store.Alert{ID: 11, MonitorID: 7, RuleID: 3, EventID: "second", Payload: []byte("two")})
	for i, want := range []string{"first", "second"} {
		select {
		case got := <-stream.sent:
			require.Equal(t, want, got.EventID)
			require.Equal(t, int64(7), got.MonitorID)
			require.Equal(t, []byte([]string{"one", "two"}[i]), got.Payload)
		case <-time.After(4 * time.Second):
			t.Fatalf("timed out waiting for alert %q", want)
		}
	}
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("cancelled stream did not stop promptly")
	}
}

func TestStreamAlertsStopsAfterSendFailure(t *testing.T) {
	st := &streamAlertStore{started: make(chan struct{})}
	s := &Server{store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sendFailure := errors.New("client disconnected")
	stream := &captureAlertStream{ctx: ctx, sent: make(chan *AlertProto, 1), sendErr: sendFailure}
	done := make(chan error, 1)
	go func() { done <- s.StreamAlerts(&StreamAlertsRequest{MonitorID: 4}, stream) }()
	<-st.started
	st.add(store.Alert{ID: 1, MonitorID: 4, EventID: "disconnect"})
	select {
	case err := <-done:
		require.ErrorIs(t, err, sendFailure)
	case <-time.After(4 * time.Second):
		t.Fatal("stream did not stop after the client send failed")
	}
}

func TestStreamAlertsCancellationStopsHandler(t *testing.T) {
	st := &streamAlertStore{started: make(chan struct{})}
	s := &Server{store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	stream := &captureAlertStream{ctx: ctx, sent: make(chan *AlertProto, 1)}
	done := make(chan error, 1)
	go func() { done <- s.StreamAlerts(&StreamAlertsRequest{}, stream) }()
	<-st.started
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("cancelled handler did not return")
	}
}
