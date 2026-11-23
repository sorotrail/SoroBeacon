# Escalation policies

A monitor's attached channels answer "who hears about this alert?". An
**escalation policy** answers "who hears about it *next* if nobody responds?".

A policy belongs to one monitor and is an ordered list of **steps**. Each step
names one or more channels and a delay. When an alert fires, step 0 is
delivered immediately; each later step fires after the previous one, provided
the alert has not been acknowledged in the meantime.

Channels are the only escalation target. There is no separate notion of a
person, a rota or a user account: the second step reaches a person only
because it names a channel that reaches them.

{% hint style="info" %}
A policy **replaces** the flat fan-out. Once a monitor has a policy, the
channels attached to the monitor with `channel_ids` are no longer used for
alerts; only the channels named in the policy's steps are. This is easy to
miss when adding a policy to an existing monitor: if the old channel is not
in step 0, it stops receiving that monitor's alerts.
{% endhint %}

## The model

- A monitor has **at most one** policy. Writing a policy replaces the whole
  thing; there is no partial step edit.
- Steps are ordered and **positions are dense**: they are stored as
  `0, 1, 2, …` with no gaps. Replacing a policy deletes and re-inserts its
  steps, so a removed step cannot survive as a hole in the sequence.
- A step's delay is stored in whole seconds. The API accepts a Go duration
  string (`"30s"`, `"5m"`, `"1h"`); anything below one second is truncated
  toward zero.
- A step must name at least one channel, and every channel must exist when
  the policy is written. A step with no channels would silently do nothing.
- A policy with no steps is rejected. Delete the policy instead.

## When steps fire

The timing rule is the part people get wrong, so it is stated exactly:

1. **Step 0 fires as soon as the alert is dispatched.** Its own `delay` is
   **ignored** — the engine delivers step 0 without reading it. Set it to
   empty or `0`; a non-zero value is accepted and echoed back by the API, but
   has no effect.
2. **Step 1 is due at the moment step 0 fired, plus step 1's delay.** With an
   immediate step 0, that is effectively the alert time plus the delay.
3. **Step N is due at the moment step N−1 actually fired, plus step N's
   delay.** Delays compose; they do not run in parallel and are not measured
   from the original alert once the first step has fired.

The scheduler checks for due steps every **10 seconds** (see
[The scheduler](#the-scheduler)), so an actual firing can be up to about ten
seconds after its due time. The following delay is measured from that actual
firing, not from the original due time, so small delays can accumulate.

A worked example, with an alert raised at `12:00:00`:

| Step | Delay | Channels | Fires at |
| --- | --- | --- | --- |
| 0 | (ignored) | Slack `#oncall` | `12:00:00` |
| 1 | `5m` | PagerDuty | about `12:05:00` |
| 2 | `10m` | Email `ops@` | about `12:15:00` |

Step 2 is fifteen minutes after the alert, not ten: the `10m` is measured from
step 1, and step 1's `5m` is measured from step 0. If a step runs late — a
restart, a backlog, a leader change — its own delay starts when it actually
fired, so the whole remaining schedule shifts with it.

## Configuring a policy

The API is at `GET`, `PUT` and `DELETE /api/v1/monitors/{id}/escalation`.
Writing requires the `monitors:write` and `channels:write` scopes (or an
unrestricted credential); reading requires `monitors:read`.

```sh
curl -s -X PUT localhost:8080/api/v1/monitors/1/escalation \
  -H "Authorization: Bearer $API_TOKEN" \
  -d '{
    "steps": [
      { "delay": "0s",  "channel_ids": [1] },
      { "delay": "10m", "channel_ids": [2] },
      { "delay": "30m", "channel_ids": [3] }
    ]
  }'
```

This pages Slack immediately, PagerDuty after ten minutes, and email after a
further thirty — forty minutes after the alert in total. The response carries
the stored policy with `delay` rendered as a duration string and `position`
filled in.

Deleting the policy returns the monitor to the flat fan-out over its attached
channels. Deleting a policy does not cancel an escalation already in flight
for an old alert; that escalation is completed the next time the scheduler
looks at it (see [Stop conditions](#stop-conditions)).

## Stop conditions

### Acknowledgement

`POST /api/v1/alerts/{id}/acknowledge` is the supported stop. Acknowledging
stamps the alert and completes its escalation **in one database
transaction**, so a step cannot slip in between the two writes. The due-step
query additionally excludes acknowledged alerts, so a stopped escalation does
not fire late even when the scheduler is processing a backlog.

The engine also re-checks the alert's acknowledgement immediately before
delivering a due step, narrowing the window where an acknowledgement arrives
after the due list was read but before the notification is sent. The practical
guarantee is: **once the acknowledgement is committed, no later step is
delivered.** A notification already in flight at that instant can still
complete — the escalation cannot recall a message that has left the process.

### There is no resolution

Alerts do not have a resolved state. Acknowledgement is the only alert-side
action that stops an escalation. Retracting an alert after a chain
reorganisation marks it as no longer canonical but does **not** stop its
escalation. If you need to stop one, acknowledge it.

### End of the policy

Firing the last step completes the escalation. A completed escalation is
never reconsidered, so the final step does not repeat. If you want a
"keep paging until someone acknowledges" policy, that does not exist today:
write enough explicit steps for the window you care about.

### Policy deleted or structurally changed

- **Deleted mid-escalation:** the scheduler finds no policy and completes the
  escalation. No further steps fire.
- **Replaced mid-escalation:** the policy keeps its id but its steps are
  deleted and re-inserted. An in-flight escalation stores only its *next step
  index*, not a copy of the step, so the next firing uses whatever step now
  occupies that index. If the new policy has fewer steps, the index is out of
  range and the escalation is completed. Avoid editing a policy while an
  escalation it started is still running.

### Delivery failure does not stop escalation

If a step's delivery fails, the escalation still advances to the next step.
For each channel in the step the dispatcher retries up to **three** times
(with 1s, 2s and 4s backoff), records every attempt, updates the channel's
health counters, and writes a **dead letter** when the attempts are
exhausted. Only then does the step count as fired. One dead channel therefore
does not silence the rest of the escalation — but it also means a step that
reached nobody still advances the schedule. Watch
[delivery failures](../operations/delivery-failures.md) and channel health if
later steps are unexpectedly firing for alerts the first channel was meant to
cover.

A channel disabled after the policy was written is skipped. A step whose
channel list becomes empty — possible only by editing the database directly,
since the API rejects it — is skipped without a panic or an error, and the
escalation advances.

## Interaction with suppression

Inhibition rules and maintenance windows are evaluated **once, before an
escalation starts**, on the same path that decides whether to deliver at all.
A suppressed alert never schedules an escalation.

They are **not re-evaluated for later steps**. A maintenance window that
starts after the alert, or an inhibition whose source starts firing later,
does not stop steps that are already scheduled. This matters for windows that
begin shortly after a monitor's first notification: the first step may go out
just before the window, and the follow-up steps will still go out during it.
If that is not what you want, acknowledge the alert or remove the policy for
the duration of the window.

Digest mode and a channel's `min_severity` filter are also **not applied to
escalation steps**. Steps call the delivery path directly:

- A digest-mode channel is sent the alert immediately instead of accumulating
  it in a window.
- A channel with a `min_severity` above the alert's severity still receives
  the step.

This is different from the flat fan-out, which honours both. If a step must
not receive low-severity alerts, put it on a monitor whose rules produce the
severity you want, or use a channel without a severity assumption.

## The scheduler

Escalation is driven by a loop inside the service, not by the alert request:

- It looks for due steps every **10 seconds**, in batches of up to 100.
- Only the **leader** instance runs it. On a multi-instance deployment the
  other instances serve reads and do not step escalations, which is what
  prevents both of them paging the same step. The engine itself has no
  per-row claim: if two processes ran it at once, a due step could be
  delivered twice. Do not run the scheduler outside the leader — the
  built-in leadership gate is the guarantee.
- The next due time is persisted on the alert's escalation row, so a restart
  **resumes** an in-flight escalation rather than restarting or dropping it.
  A step whose due time passed while the process was down fires on the first
  tick after it comes back.
- The notification content is snapshotted when the escalation starts, so a
  later step delivered after a restart sends the same alert content even if
  the monitor has since been edited.

## What not to do

- **Do not set a very short second delay to "catch" a transient alert.**
  A `1m` step after an immediate page means every brief hiccup pages twice.
  People learn to ignore a pager that always fires twice. Give the first
  channel time to be seen — ten minutes is a common floor.
- **Do not rely on step 0's delay.** It is ignored. If you need a delay
  before the first notification, that is a rule cooldown or a maintenance
  window question, not an escalation one.
- **Do not use escalation to work around a broken channel.** The dispatcher
  retries a failing channel and dead-letters the alert before the step
  advances; the next step is a fallback, not a fix. Check
  [delivery failures](../operations/delivery-failures.md).
- **Do not expect digest batching in a step.** Escalation delivery ignores
  digest mode; every step is an immediate send.
- **Do not edit a policy mid-escalation** unless you are willing to change
  what the next step fires; the engine addresses steps by index.
