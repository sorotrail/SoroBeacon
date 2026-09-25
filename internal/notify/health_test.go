package notify

import (
	"errors"
	"fmt"
	"net/textproto"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsPermanent(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil is not a failure at all",
			err:  nil,
			want: false,
		},
		{
			name: "revoked webhook token",
			err:  &HTTPStatusError{StatusCode: 401, Body: "Unauthorized"},
			want: true,
		},
		{
			name: "deleted webhook",
			err:  &HTTPStatusError{StatusCode: 403, Body: "Forbidden"},
			want: true,
		},
		{
			name: "removed channel endpoint",
			err:  &HTTPStatusError{StatusCode: 404, Body: "Not Found"},
			want: true,
		},
		{
			name: "provider outage",
			err:  &HTTPStatusError{StatusCode: 500, Body: "Internal Server Error"},
			want: false,
		},
		{
			name: "bad gateway",
			err:  &HTTPStatusError{StatusCode: 502, Body: "Bad Gateway"},
			want: false,
		},
		{
			name: "rate limited",
			err:  &HTTPStatusError{StatusCode: 429, Body: "Too Many Requests"},
			want: false,
		},
		{
			name: "bad request is our bug, not the channel's",
			err:  &HTTPStatusError{StatusCode: 400, Body: "Bad Request"},
			want: false,
		},
		{
			name: "wrapped status error still classifies",
			err:  fmt.Errorf("discord: %w", &HTTPStatusError{StatusCode: 401, Body: "Unauthorized"}),
			want: true,
		},
		{
			name: "timeout",
			err:  errors.New("post: context deadline exceeded"),
			want: false,
		},
		{
			name: "connection refused",
			err:  errors.New("post: dial tcp 127.0.0.1:443: connect: connection refused"),
			want: false,
		},
		{
			name: "rotated smtp password",
			err:  fmt.Errorf("email: %w", &textproto.Error{Code: 535, Msg: "authentication failed"}),
			want: true,
		},
		{
			name: "smtp mailbox rejected",
			err:  &textproto.Error{Code: 550, Msg: "mailbox unavailable"},
			want: true,
		},
		{
			name: "smtp temporary failure",
			err:  &textproto.Error{Code: 451, Msg: "try again later"},
			want: false,
		},
		{
			name: "an unrecognised error is never held against a channel",
			err:  errors.New("something new went wrong"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsPermanent(tt.err))
		})
	}
}
