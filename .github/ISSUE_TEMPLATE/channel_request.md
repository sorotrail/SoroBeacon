---
name: Channel request
about: Request a notification channel (or volunteer to add one)
labels: enhancement, channel
---

**The service**

Name of the service you want SoroBeacon to notify (PagerDuty, Matrix,
ntfy.sh, …).

**API docs for sending a message**

A link to the docs for the send-a-message / create-incident call, not
the marketing site. That is the only API SoroBeacon needs.

**Authentication**

How callers authenticate: a webhook URL, an API token, both, or
something else. Do not paste live secrets.

**Rate limits and message size**

Any documented rate limit or payload size cap that would affect
delivery.

**How contained the work is**

A new channel is a `notify.Notifier` registered on `DefaultFactory`
in `internal/notify`. If that is enough for you to send a PR instead
of a request, please do — CONTRIBUTING.md has the plug-in table.
