package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// A receiver that rejects the request itself will reject it again: the
// payload is what it objected to, and sending it unchanged changes nothing.
// The dispatcher therefore does not spend its retry budget on it — one
// attempt, one dead letter.
//
// This is the shape of a federated /ingest answering 422: the alert cannot be
// stored on the receiving instance, and retrying it three times would only
// write the same failure three times before dead-lettering it anyway.
func TestDispatchDoesNotRetryARejectedRequest(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{channelByType(30, "federation")}}
	n := &mockNotifier{failures: 99, err: &StatusError{
		Code:    http.StatusUnprocessableEntity,
		Snippet: "the alert references a record that does not exist",
	}}
	d := newFailureDispatcher(t, st, map[string]Constructor{
		"federation": func(json.RawMessage) (Notifier, error) { return n, nil },
	})
	d.MaxAttempts = 3

	d.Dispatch(context.Background(), Alert{ID: 103, MonitorID: 42})

	assert.Equal(t, 1, n.calls, "a rejected request must not be retried")
	require.Len(t, st.attempts, 1)
	assert.Equal(t, "failed", st.attempts[0].Status)
	require.Len(t, st.deadLetters, 1, "the delivery is dead-lettered at once")
	assert.Equal(t, 1, st.deadLetters[0].AttemptCount)
	assert.Equal(t, http.StatusUnprocessableEntity, st.deadLetters[0].LastStatus)
}

// A receiver that says "not now" is the opposite: the request is fine and the
// timing is wrong, so the delivery is retried.
func TestDispatchRetriesARateLimitedRequest(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{channelByType(31, "federation")}}
	n := &mockNotifier{failures: 99, err: &StatusError{
		Code:    http.StatusTooManyRequests,
		Snippet: "slow down",
	}}
	d := newFailureDispatcher(t, st, map[string]Constructor{
		"federation": func(json.RawMessage) (Notifier, error) { return n, nil },
	})
	d.MaxAttempts = 3

	d.Dispatch(context.Background(), Alert{ID: 104, MonitorID: 42})

	assert.Equal(t, 3, n.calls, "a rate-limited request is worth another attempt")
	require.Len(t, st.deadLetters, 1)
	assert.Equal(t, 3, st.deadLetters[0].AttemptCount)
}

// A rejected request says nothing about the channel's health: the receiver
// objected to the payload, not to the credential. A 400 must therefore not
// count towards the auto-disable threshold, which is reserved for a channel
// that is genuinely broken.
func TestRejectedRequestDoesNotCountAsAPermanentChannelFailure(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{channelByType(32, "federation")}}
	n := &mockNotifier{failures: 99, err: &HTTPStatusError{
		StatusCode: http.StatusBadRequest,
		Body:       "malformed payload",
	}}
	d := newFailureDispatcher(t, st, map[string]Constructor{
		"federation": func(json.RawMessage) (Notifier, error) { return n, nil },
	})
	d.WithDisableAfterFailures(3)

	d.Dispatch(context.Background(), Alert{ID: 105, MonitorID: 42})

	updates := healthFor(st, 32)
	require.Len(t, updates, 1)
	assert.False(t, updates[0].Permanent,
		"a bad request is our payload's fault, not a broken channel")
}
