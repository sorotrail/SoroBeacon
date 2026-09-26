package notify

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/store"
)

type scriptedNotifier struct {
	responses []error
	calls     int
}

func (n *scriptedNotifier) Send(_ context.Context, _ Alert) error {
	if n.calls >= len(n.responses) {
		return n.responses[len(n.responses)-1]
	}
	err := n.responses[n.calls]
	n.calls++
	return err
}

type panicNotifier struct {
	secret string
}

func (n *panicNotifier) Send(_ context.Context, _ Alert) error {
	panic("panic secret: " + n.secret)
}

func newFailureDispatcher(t *testing.T, st *fakeDispatchStore, constructors map[string]Constructor) *Dispatcher {
	t.Helper()
	f := &Factory{constructors: map[string]Constructor{}}
	for name, c := range constructors {
		f.Register(name, c)
	}
	d := NewDispatcher(st, f, slog.New(slog.DiscardHandler))
	d.BaseBackoff = 25 * time.Millisecond
	return d
}

func channelByType(id int64, typ string) store.Channel {
	return store.Channel{ID: id, Name: typ, Type: typ, Config: json.RawMessage(`{}`), Enabled: true}
}

func TestDispatchPartialFailureDoesNotBlockOtherChannels(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{
		channelByType(1, "good-one"),
		channelByType(2, "good-two"),
		channelByType(3, "bad"),
	}}
	d := newFailureDispatcher(t, st, map[string]Constructor{
		"good-one": func(json.RawMessage) (Notifier, error) { return &scriptedNotifier{responses: []error{nil}}, nil },
		"good-two": func(json.RawMessage) (Notifier, error) { return &scriptedNotifier{responses: []error{nil}}, nil },
		"bad": func(json.RawMessage) (Notifier, error) {
			return &scriptedNotifier{responses: []error{errors.New("token secret-bad-1")}}, nil
		},
	})
	d.MaxAttempts = 1

	d.Dispatch(context.Background(), Alert{ID: 101, MonitorID: 42})

	require.Len(t, st.attempts, 3)
	statuses := map[int64]string{}
	for _, a := range st.attempts {
		statuses[a.ChannelID] = a.Status
		if a.ChannelID == 3 {
			assert.NotContains(t, a.ResponseSnippet, "secret")
			assert.NotContains(t, a.ResponseSnippet, "token")
		}
	}
	assert.Equal(t, "success", statuses[1])
	assert.Equal(t, "success", statuses[2])
	assert.Equal(t, "failed", statuses[3])
}

func TestDispatchEveryChannelFailsRecordsAttempts(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{
		channelByType(10, "bad-one"),
		channelByType(11, "bad-two"),
		channelByType(12, "bad-three"),
	}}
	d := newFailureDispatcher(t, st, map[string]Constructor{
		"bad-one": func(json.RawMessage) (Notifier, error) {
			return &scriptedNotifier{responses: []error{errors.New("token secret-bad-one")}}, nil
		},
		"bad-two": func(json.RawMessage) (Notifier, error) {
			return &scriptedNotifier{responses: []error{errors.New("token secret-bad-two")}}, nil
		},
		"bad-three": func(json.RawMessage) (Notifier, error) {
			return &scriptedNotifier{responses: []error{errors.New("token secret-bad-three")}}, nil
		},
	})
	d.MaxAttempts = 1

	d.Dispatch(context.Background(), Alert{ID: 102, MonitorID: 42})

	assert.Len(t, st.attempts, 3)
	for _, a := range st.attempts {
		assert.Equal(t, "failed", a.Status)
		assert.NotContains(t, a.ResponseSnippet, "secret")
		assert.NotContains(t, a.ResponseSnippet, "token")
	}
}

func TestDispatchRetryRespectsBackoffAndRecordsOutcome(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{channelByType(20, "retry")}}
	d := newFailureDispatcher(t, st, map[string]Constructor{
		"retry": func(json.RawMessage) (Notifier, error) {
			return &scriptedNotifier{responses: []error{errors.New("token secret-retry"), nil}}, nil
		},
	})

	start := time.Now()
	d.Dispatch(context.Background(), Alert{ID: 103, MonitorID: 42})
	elapsed := time.Since(start)

	require.Len(t, st.attempts, 2)
	assert.Equal(t, "failed", st.attempts[0].Status)
	assert.Equal(t, "success", st.attempts[1].Status)
	assert.NotContains(t, st.attempts[0].ResponseSnippet, "secret")
	assert.NotContains(t, st.attempts[0].ResponseSnippet, "token")
	assert.GreaterOrEqual(t, elapsed, d.BaseBackoff)
}

func TestDispatchStopsAfterMaxAttempts(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{channelByType(30, "exhaust")}}
	d := newFailureDispatcher(t, st, map[string]Constructor{
		"exhaust": func(json.RawMessage) (Notifier, error) {
			return &scriptedNotifier{responses: []error{errors.New("token secret-exhaust"), errors.New("token secret-exhaust"), errors.New("token secret-exhaust")}}, nil
		},
	})

	d.Dispatch(context.Background(), Alert{ID: 104, MonitorID: 42})

	assert.Len(t, st.attempts, d.MaxAttempts)
	for _, a := range st.attempts {
		assert.Equal(t, "failed", a.Status)
		assert.NotContains(t, a.ResponseSnippet, "secret")
		assert.NotContains(t, a.ResponseSnippet, "token")
	}
}

func TestDispatchDoesNotCrashOnPanickingNotifier(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{channelByType(40, "panic")}}
	d := newFailureDispatcher(t, st, map[string]Constructor{
		"panic": func(json.RawMessage) (Notifier, error) {
			return &panicNotifier{secret: "secret-panic"}, nil
		},
	})
	// The dispatcher should isolate a panic to one channel and keep going.
	d.MaxAttempts = 1

	assert.NotPanics(t, func() {
		d.Dispatch(context.Background(), Alert{ID: 105, MonitorID: 42})
	})
	require.Len(t, st.attempts, 1)
	assert.Equal(t, "failed", st.attempts[0].Status)
	assert.NotContains(t, st.attempts[0].ResponseSnippet, "secret")
	assert.NotContains(t, st.attempts[0].ResponseSnippet, "panic")
}
