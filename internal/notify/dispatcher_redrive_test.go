package notify

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// redriveStore implements only the reads and writes RedriveDeadLetter uses.
// It embeds store.Store so any unexpected call fails loudly at compile time
// rather than silently exercising a nil implementation.
type redriveStore struct {
	store.Store
	alert    *store.Alert
	channel  *store.Channel
	attempts []store.DeliveryAttempt
	deleted  []int64
}

func (s *redriveStore) GetAlert(context.Context, int64) (*store.Alert, error) {
	if s.alert == nil {
		return nil, store.ErrNotFound
	}
	return s.alert, nil
}

func (s *redriveStore) GetChannel(context.Context, int64) (*store.Channel, error) {
	if s.channel == nil {
		return nil, store.ErrNotFound
	}
	return s.channel, nil
}

func (s *redriveStore) ListDeliveryAttempts(context.Context, int64, string) ([]store.DeliveryAttempt, error) {
	return s.attempts, nil
}

func (s *redriveStore) RecordDeliveryAttempt(_ context.Context, d *store.DeliveryAttempt) error {
	s.attempts = append(s.attempts, *d)
	return nil
}

func (s *redriveStore) DeleteDeadLetter(_ context.Context, id int64) error {
	s.deleted = append(s.deleted, id)
	return nil
}

func redriveDispatcher(t *testing.T, st *redriveStore, n Notifier) *Dispatcher {
	t.Helper()
	f := &Factory{constructors: map[string]Constructor{}}
	f.Register("mock", func(json.RawMessage) (Notifier, error) { return n, nil })
	return NewDispatcher(st, f, slog.New(slog.DiscardHandler))
}

func TestRedriveDeadLetterSuccessRecordsAndClears(t *testing.T) {
	st := &redriveStore{
		alert:   &store.Alert{ID: 10, MonitorID: 1, RuleID: 2, EventID: "ev-1"},
		channel: &store.Channel{ID: 7, Name: "ops", Type: "mock", Enabled: true},
	}
	n := &mockNotifier{}
	d := redriveDispatcher(t, st, n)

	err := d.RedriveDeadLetter(context.Background(), 3, store.DeadLetter{ID: 3, AlertID: 10, ChannelID: 7})
	require.NoError(t, err)
	assert.Equal(t, 1, n.calls)
	require.Len(t, st.attempts, 1)
	assert.Equal(t, store.DeliveryStatusSuccess, st.attempts[0].Status)
	assert.Equal(t, []int64{3}, st.deleted)
}

func TestRedriveDeadLetterFailureKeepsTheRecordAndRedactsTheError(t *testing.T) {
	const secret = "SUPER-SECRET-TOKEN"
	st := &redriveStore{
		alert:   &store.Alert{ID: 10, MonitorID: 1, RuleID: 2, EventID: "ev-1"},
		channel: &store.Channel{ID: 7, Name: "ops", Type: "mock", Enabled: true},
	}
	n := &mockNotifier{failures: 1, err: errors.New("post https://hooks.example/" + secret + ": connection refused")}
	d := redriveDispatcher(t, st, n)

	err := d.RedriveDeadLetter(context.Background(), 3, store.DeadLetter{ID: 3, AlertID: 10, ChannelID: 7})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
	assert.Equal(t, 1, n.calls)
	require.Len(t, st.attempts, 1)
	assert.Equal(t, store.DeliveryStatusFailed, st.attempts[0].Status)
	assert.NotContains(t, st.attempts[0].ResponseSnippet, secret)
	assert.Empty(t, st.deleted, "a failed redrive must leave the dead letter retryable")
}

func TestRedriveDeadLetterAlreadySucceededClearsAndDoesNotSend(t *testing.T) {
	st := &redriveStore{
		alert:   &store.Alert{ID: 10, MonitorID: 1, RuleID: 2, EventID: "ev-1"},
		channel: &store.Channel{ID: 7, Name: "ops", Type: "mock", Enabled: true},
		attempts: []store.DeliveryAttempt{
			{AlertID: 10, ChannelID: 7, Status: store.DeliveryStatusSuccess},
		},
	}
	n := &mockNotifier{}
	d := redriveDispatcher(t, st, n)

	err := d.RedriveDeadLetter(context.Background(), 3, store.DeadLetter{ID: 3, AlertID: 10, ChannelID: 7})
	assert.ErrorIs(t, err, ErrAlreadySucceeded)
	assert.Zero(t, n.calls)
	assert.Equal(t, []int64{3}, st.deleted)
}

func TestRedriveDeadLetterRefusesDisabledChannel(t *testing.T) {
	st := &redriveStore{
		alert:   &store.Alert{ID: 10, MonitorID: 1, RuleID: 2, EventID: "ev-1"},
		channel: &store.Channel{ID: 7, Name: "ops", Type: "mock", Enabled: false},
	}
	n := &mockNotifier{}
	d := redriveDispatcher(t, st, n)

	err := d.RedriveDeadLetter(context.Background(), 3, store.DeadLetter{ID: 3, AlertID: 10, ChannelID: 7})
	assert.ErrorIs(t, err, ErrChannelDisabled)
	assert.Zero(t, n.calls)
	assert.Empty(t, st.deleted)
}

func TestRedriveDeadLetterMissingAlertDoesNotSend(t *testing.T) {
	st := &redriveStore{
		channel: &store.Channel{ID: 7, Name: "ops", Type: "mock", Enabled: true},
	}
	n := &mockNotifier{}
	d := redriveDispatcher(t, st, n)

	err := d.RedriveDeadLetter(context.Background(), 3, store.DeadLetter{ID: 3, AlertID: 10, ChannelID: 7})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "get alert")
	assert.Zero(t, n.calls)
	assert.Empty(t, st.deleted)
}

func TestRedriveDeadLetterMissingChannelDoesNotSend(t *testing.T) {
	st := &redriveStore{
		alert: &store.Alert{ID: 10, MonitorID: 1, RuleID: 2, EventID: "ev-1"},
	}
	n := &mockNotifier{}
	d := redriveDispatcher(t, st, n)

	err := d.RedriveDeadLetter(context.Background(), 3, store.DeadLetter{ID: 3, AlertID: 10, ChannelID: 7})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "get channel")
	assert.Zero(t, n.calls)
	assert.Empty(t, st.deleted)
}
