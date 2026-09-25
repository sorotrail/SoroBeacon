package notify

import (
	"errors"
	"net/http"
	"net/textproto"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// TestHealthUpdate maps the outcome of a "send test" click onto the health
// update that records it, so the API and the dashboard report the same thing
// about the same button.
//
// A test send is a real delivery outcome — that is what makes it useful for
// checking a channel you have just fixed — but the update deliberately carries
// no DisableAfter: the auto-disable threshold belongs to real alert
// deliveries, and a diagnostic click must never be what switches an
// operator's alerting off.
func TestHealthUpdate(sendErr error, at time.Time) store.ChannelHealthUpdate {
	u := store.ChannelHealthUpdate{At: at}
	if sendErr == nil {
		u.Success = true
		return u
	}
	u.Error = sendErr.Error()
	u.Permanent = IsPermanent(sendErr)
	return u
}

// IsPermanent reports whether err is a failure the channel will not recover
// from on its own, as opposed to one that heals by itself.
//
// The distinction exists because auto-disabling a channel is a destructive
// answer to a temporary problem. A revoked bot token (401), a webhook the
// operator deleted (403) or a URL that no longer exists (404) fails the same
// way on every alert forever — that is the case auto-disable is for. A 5xx, a
// 429 or a timeout is the provider being briefly unavailable, and taking the
// channel out of rotation for it would turn a short outage into silently lost
// alerts, which is a worse outcome than the failure it was meant to fix. Only
// permanent failures therefore move a channel toward auto-disable; transient
// ones are still counted and reported, just not held against it.
//
// Anything unrecognised is transient, deliberately: health tracking should
// never disable a channel on a failure it does not understand.
func IsPermanent(err error) bool {
	if err == nil {
		return false
	}
	var httpErr *HTTPStatusError
	if errors.As(err, &httpErr) {
		switch httpErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			return true
		}
		return false
	}
	// A rotated SMTP password or a rejected sender fails the email channel
	// the same way a revoked token fails a webhook, and net/smtp reports it
	// as a 5xx reply (535 is the authentication failure every operator hits).
	var smtpErr *textproto.Error
	if errors.As(err, &smtpErr) {
		return smtpErr.Code >= 500
	}
	return false
}
